package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// testAPIKey is deliberately not shaped like any real provider credential.
const testAPIKey = "test-key-not-a-real-credential"

func sampleRequest() Request {
	return Request{
		Cause:    "app_exit_nonzero",
		Evidence: []string{"exit code 1", "no OOMKilled reason", "restartCount 7"},
		LogTail: []string{
			"java.lang.IllegalStateException: Failed to obtain JDBC Connection",
			"Caused by: org.postgresql.util.PSQLException: Connection refused",
		},
		Image: "ghcr.io/example/billing:1.4.2",
	}
}

type recorder struct {
	mu     sync.Mutex
	path   string
	header http.Header
	body   map[string]any
}

func (rec *recorder) snapshot() (string, http.Header, map[string]any) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.path, rec.header, rec.body
}

// newTestServer records the incoming request and replies with status/response.
func newTestServer(t *testing.T, status int, response string) (*httptest.Server, *recorder) {
	t.Helper()
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("server: decode request body: %v", err)
		}
		rec.mu.Lock()
		rec.path = r.URL.Path
		rec.header = r.Header.Clone()
		rec.body = body
		rec.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := w.Write([]byte(response)); err != nil {
			t.Errorf("server: write response: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func chatMessages(t *testing.T, body map[string]any) []map[string]string {
	t.Helper()
	raw, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("request body has no messages array: %v", body)
	}
	out := make([]map[string]string, 0, len(raw))
	for _, m := range raw {
		msg, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("message is not an object: %v", m)
		}
		role, _ := msg["role"].(string)
		content, _ := msg["content"].(string)
		out = append(out, map[string]string{"role": role, "content": content})
	}
	return out
}

func assertPromptCarriesEvidence(t *testing.T, prompt string) {
	t.Helper()
	for _, want := range []string{
		"app_exit_nonzero",
		"ghcr.io/example/billing:1.4.2",
		"exit code 1",
		"restartCount 7",
		"java.lang.IllegalStateException: Failed to obtain JDBC Connection",
		"org.postgresql.util.PSQLException: Connection refused",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("user prompt does not carry %q:\n%s", want, prompt)
		}
	}
}

func TestAnthropicSummarize(t *testing.T) {
	srv, rec := newTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"  The container exited because Postgres refused the connection.  "}]}`)

	p, err := New(Config{Provider: ProviderAnthropic, APIKey: testAPIKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != ProviderAnthropic {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderAnthropic)
	}

	got, err := p.Summarize(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if want := "The container exited because Postgres refused the connection."; got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}

	path, header, body := rec.snapshot()
	if path != "/v1/messages" {
		t.Errorf("path = %q, want /v1/messages", path)
	}
	if header.Get("x-api-key") != testAPIKey {
		t.Errorf("x-api-key header = %q", header.Get("x-api-key"))
	}
	if header.Get("anthropic-version") != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", header.Get("anthropic-version"), anthropicVersion)
	}
	if header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", header.Get("Content-Type"))
	}
	if got, want := body["model"], defaultAnthropicModel; got != want {
		t.Errorf("model = %v, want %v", got, want)
	}
	if system, _ := body["system"].(string); system != systemPrompt {
		t.Errorf("system prompt = %q", system)
	}
	msgs := chatMessages(t, body)
	if len(msgs) != 1 || msgs[0]["role"] != "user" {
		t.Fatalf("expected a single user message, got %v", msgs)
	}
	assertPromptCarriesEvidence(t, msgs[0]["content"])
}

func TestAnthropicSummarizeConcatenatesTextBlocks(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":""},{"type":"tool_use"},{"type":"text","text":"Postgres refused"},{"type":"text","text":" the connection."}]}`)

	p, err := New(Config{Provider: ProviderAnthropic, APIKey: testAPIKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := p.Summarize(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if want := "Postgres refused the connection."; got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
}

func TestOpenAISummarize(t *testing.T) {
	srv, rec := newTestServer(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"Postgres refused the connection."}}]}`)

	p, err := New(Config{Provider: ProviderOpenAI, APIKey: testAPIKey, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != ProviderOpenAI {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderOpenAI)
	}

	got, err := p.Summarize(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if want := "Postgres refused the connection."; got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}

	path, header, body := rec.snapshot()
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", path)
	}
	if want := "Bearer " + testAPIKey; header.Get("Authorization") != want {
		t.Errorf("Authorization header = %q, want %q", header.Get("Authorization"), want)
	}
	if got, want := body["model"], defaultOpenAIModel; got != want {
		t.Errorf("model = %v, want %v", got, want)
	}
	if _, ok := body["max_completion_tokens"]; !ok {
		t.Errorf("request body has no max_completion_tokens: %v", body)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Errorf("request body still carries the deprecated max_tokens: %v", body)
	}
	msgs := chatMessages(t, body)
	if len(msgs) != 2 {
		t.Fatalf("expected system+user messages, got %v", msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != systemPrompt {
		t.Errorf("first message = %v, want the system prompt", msgs[0])
	}
	if msgs[1]["role"] != "user" {
		t.Errorf("second message role = %q, want user", msgs[1]["role"])
	}
	assertPromptCarriesEvidence(t, msgs[1]["content"])
}

func TestOllamaSummarize(t *testing.T) {
	srv, rec := newTestServer(t, http.StatusOK,
		`{"message":{"role":"assistant","content":"Postgres refused the connection."}}`)

	// No API key: ollama is the keyless, self-hosted path.
	p, err := New(Config{Provider: ProviderOllama, BaseURL: srv.URL + "/"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.Name() != ProviderOllama {
		t.Errorf("Name() = %q, want %q", p.Name(), ProviderOllama)
	}

	got, err := p.Summarize(context.Background(), sampleRequest())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if want := "Postgres refused the connection."; got != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}

	path, header, body := rec.snapshot()
	if path != "/api/chat" {
		t.Errorf("path = %q, want /api/chat", path)
	}
	if header.Get("Authorization") != "" || header.Get("x-api-key") != "" {
		t.Errorf("ollama must not send credentials, got %v", header)
	}
	if got, want := body["model"], defaultOllamaModel; got != want {
		t.Errorf("model = %v, want %v", got, want)
	}
	if stream, ok := body["stream"].(bool); !ok || stream {
		t.Errorf("stream = %v, want false", body["stream"])
	}
	msgs := chatMessages(t, body)
	if len(msgs) != 2 || msgs[0]["role"] != "system" {
		t.Fatalf("expected system+user messages, got %v", msgs)
	}
	assertPromptCarriesEvidence(t, msgs[1]["content"])
}

func TestProviderNonSuccessStatusNeverLeaksKey(t *testing.T) {
	// A hostile/verbose provider echoing the key back must still not put it
	// in our error, and the body must be truncated.
	longBody := `{"error":{"type":"authentication_error","message":"invalid x-api-key ` +
		testAPIKey + ` ` + strings.Repeat("padding ", 60) + `"}}`

	for _, provider := range SupportedProviders() {
		t.Run(provider, func(t *testing.T) {
			srv, _ := newTestServer(t, http.StatusUnauthorized, longBody)
			p, err := New(Config{Provider: provider, APIKey: testAPIKey, BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			summary, err := p.Summarize(context.Background(), sampleRequest())
			if err == nil {
				t.Fatalf("expected an error for a 401 response, got summary %q", summary)
			}
			if summary != "" {
				t.Errorf("summary should be empty on error, got %q", summary)
			}
			if !errors.Is(err, ErrHTTPStatus) {
				t.Errorf("error should wrap ErrHTTPStatus, got %v", err)
			}
			msg := err.Error()
			if !strings.Contains(msg, "401") {
				t.Errorf("error should mention the status code, got %q", msg)
			}
			if strings.Contains(msg, testAPIKey) {
				t.Fatalf("API KEY LEAKED INTO ERROR: %q", msg)
			}
			if !strings.Contains(msg, "authentication_error") {
				t.Errorf("error should quote the provider body, got %q", msg)
			}
			if !strings.Contains(msg, "truncated") {
				t.Errorf("long body should be truncated, got %q", msg)
			}
			if len(msg) > 400 {
				t.Errorf("error message is not bounded (%d chars): %q", len(msg), msg)
			}
		})
	}
}

func TestProviderTimeoutDoesNotHang(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	p, err := New(Config{
		Provider: ProviderOllama,
		BaseURL:  srv.URL,
		Timeout:  50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	start := time.Now()
	if _, err := p.Summarize(context.Background(), sampleRequest()); err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Summarize hung for %s instead of honoring the timeout", elapsed)
	}
}

func TestProviderContextCancellation(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	p, err := New(Config{Provider: ProviderOllama, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := p.Summarize(ctx, sampleRequest()); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected a context deadline error, got %v", err)
	}
}

func TestProviderEmptyResponse(t *testing.T) {
	cases := map[string]string{
		ProviderAnthropic: `{"content":[]}`,
		ProviderOpenAI:    `{"choices":[]}`,
		ProviderOllama:    `{"message":{"role":"assistant","content":"   "}}`,
	}
	for provider, response := range cases {
		t.Run(provider, func(t *testing.T) {
			srv, _ := newTestServer(t, http.StatusOK, response)
			p, err := New(Config{Provider: provider, APIKey: testAPIKey, BaseURL: srv.URL})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if _, err := p.Summarize(context.Background(), sampleRequest()); !errors.Is(err, ErrEmptyResponse) {
				t.Errorf("expected ErrEmptyResponse, got %v", err)
			}
		})
	}
}

func TestProviderMalformedResponse(t *testing.T) {
	srv, _ := newTestServer(t, http.StatusOK, `not json at all`)
	p, err := New(Config{Provider: ProviderOllama, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.Summarize(context.Background(), sampleRequest()); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestNewValidation(t *testing.T) {
	if _, err := New(Config{Provider: "gemini", APIKey: testAPIKey}); !errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("expected ErrUnsupportedProvider, got %v", err)
	}
	if _, err := New(Config{}); !errors.Is(err, ErrUnsupportedProvider) {
		t.Errorf("expected ErrUnsupportedProvider for an empty provider, got %v", err)
	}
	for _, provider := range []string{ProviderAnthropic, ProviderOpenAI} {
		if _, err := New(Config{Provider: provider}); !errors.Is(err, ErrMissingAPIKey) {
			t.Errorf("%s without a key: expected ErrMissingAPIKey, got %v", provider, err)
		}
	}
	if _, err := New(Config{Provider: ProviderOllama}); err != nil {
		t.Errorf("ollama must not require a key, got %v", err)
	}
	if _, err := New(Config{Provider: "  ANTHROPIC  ", APIKey: testAPIKey}); err != nil {
		t.Errorf("provider name should be normalized, got %v", err)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	tests := []struct {
		provider string
		baseURL  string
		model    string
	}{
		{ProviderAnthropic, defaultAnthropicBaseURL, defaultAnthropicModel},
		{ProviderOpenAI, defaultOpenAIBaseURL, defaultOpenAIModel},
		{ProviderOllama, defaultOllamaBaseURL, defaultOllamaModel},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			p, err := New(Config{Provider: tc.provider, APIKey: testAPIKey})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			c := clientOf(t, p)
			if c.baseURL != tc.baseURL {
				t.Errorf("baseURL = %q, want %q", c.baseURL, tc.baseURL)
			}
			if c.model != tc.model {
				t.Errorf("model = %q, want %q", c.model, tc.model)
			}
			if c.httpClient.Timeout != DefaultTimeout {
				t.Errorf("timeout = %s, want %s", c.httpClient.Timeout, DefaultTimeout)
			}
			if c.maxTokens != DefaultMaxTokens {
				t.Errorf("maxTokens = %d, want %d", c.maxTokens, DefaultMaxTokens)
			}
		})
	}

	p, err := New(Config{
		Provider:  ProviderOllama,
		BaseURL:   "http://ollama.ai-tools.svc:11434///",
		Model:     "mistral",
		Timeout:   3 * time.Second,
		MaxTokens: 64,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := clientOf(t, p)
	if c.baseURL != "http://ollama.ai-tools.svc:11434" {
		t.Errorf("baseURL = %q, trailing slashes should be trimmed", c.baseURL)
	}
	if c.model != "mistral" || c.maxTokens != 64 || c.httpClient.Timeout != 3*time.Second {
		t.Errorf("overrides not applied: %+v", c)
	}
}

func clientOf(t *testing.T, p Provider) *client {
	t.Helper()
	switch v := p.(type) {
	case *anthropicProvider:
		return v.client
	case *openAIProvider:
		return v.client
	case *ollamaProvider:
		return v.client
	default:
		t.Fatalf("unexpected provider type %T", p)
		return nil
	}
}

func TestBuildUserPrompt(t *testing.T) {
	prompt := buildUserPrompt(sampleRequest())
	assertPromptCarriesEvidence(t, prompt)

	sparse := buildUserPrompt(Request{Cause: "unknown"})
	if strings.Contains(sparse, "Evidence:") || strings.Contains(sparse, "log lines") {
		t.Errorf("empty sections should be omitted, got:\n%s", sparse)
	}
	if !strings.Contains(sparse, "unknown") {
		t.Errorf("cause missing from prompt: %s", sparse)
	}
	if buildUserPrompt(Request{}) == "" {
		t.Error("an empty Request must still produce a non-empty prompt")
	}
	if buildUserPrompt(Request{Evidence: []string{"", "  "}, LogTail: []string{""}}) == "" {
		t.Error("blank-only fields must still produce a non-empty prompt")
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short"); got != "short" {
		t.Errorf("truncate() = %q", got)
	}
	got := truncate(strings.Repeat("x", 500))
	if !strings.HasSuffix(got, "... (truncated)") {
		t.Errorf("truncate() = %q, want a truncation marker", got)
	}
	if len([]rune(got)) != maxErrorBodyChars+len("... (truncated)") {
		t.Errorf("truncate() length = %d", len([]rune(got)))
	}
}
