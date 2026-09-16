package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Provider identifiers accepted by New (the --ai-provider flag).
const (
	ProviderAnthropic = "anthropic"
	ProviderOpenAI    = "openai"
	ProviderOllama    = "ollama"
)

// Defaults applied by New when the corresponding Config field is zero.
const (
	// DefaultTimeout bounds a single summarization call. AI is additive, so a
	// slow provider must never hold up a diagnosis for long.
	DefaultTimeout = 15 * time.Second

	// DefaultMaxTokens caps the response; summaries are 2-4 sentences.
	DefaultMaxTokens = 512

	defaultAnthropicBaseURL = "https://api.anthropic.com"
	defaultOpenAIBaseURL    = "https://api.openai.com"
	defaultOllamaBaseURL    = "http://localhost:11434"

	defaultAnthropicModel = "claude-haiku-4-5-20251001"
	defaultOpenAIModel    = "gpt-4o-mini"
	defaultOllamaModel    = "llama3.1"

	anthropicVersion = "2023-06-01"

	maxErrorBodyChars = 200
	maxResponseBytes  = 1 << 20
)

// Sentinel errors returned by this package.
var (
	// ErrUnsupportedProvider is returned by New for an unknown --ai-provider.
	ErrUnsupportedProvider = errors.New("unsupported ai provider")
	// ErrMissingAPIKey is returned by New when a key-requiring provider was
	// configured without one. The key itself is never part of any error.
	ErrMissingAPIKey = errors.New("missing API key: set CRASHCAUSE_AI_API_KEY")
	// ErrHTTPStatus wraps any non-2xx provider response.
	ErrHTTPStatus = errors.New("provider returned a non-success status")
	// ErrEmptyResponse is returned when a provider answered successfully but
	// with no usable text.
	ErrEmptyResponse = errors.New("provider returned an empty summary")
	// ErrNoProvider is returned by a Summarizer that has no provider.
	ErrNoProvider = errors.New("no ai provider configured")
)

// Config describes an AI provider client.
//
// APIKey is supplied by the CALLER, which reads it from the
// CRASHCAUSE_AI_API_KEY environment variable — never from a flag (ps leakage)
// and never from a config file. It is used only as a request header: it is
// never logged and never appears in an error returned by this package.
type Config struct {
	// Provider is one of anthropic, openai, ollama.
	Provider string
	// APIKey is required for anthropic and openai, ignored for ollama.
	APIKey string
	// BaseURL overrides the provider's default endpoint (--ai-url). For
	// ollama this is how a self-hosted instance is reached.
	BaseURL string
	// Model overrides the provider's default model.
	Model string
	// Timeout bounds a single request; DefaultTimeout when zero.
	Timeout time.Duration
	// MaxTokens caps the generated summary; DefaultMaxTokens when zero.
	MaxTokens int
}

// SupportedProviders lists the accepted Config.Provider values.
func SupportedProviders() []string {
	return []string{ProviderAnthropic, ProviderOpenAI, ProviderOllama}
}

// New builds the Provider named by cfg.Provider. End users implement nothing:
// BYO-key means bring your own API key, the tool ships every provider client.
func New(cfg Config) (Provider, error) {
	switch strings.ToLower(strings.TrimSpace(cfg.Provider)) {
	case ProviderAnthropic:
		c, err := newClient(cfg, ProviderAnthropic, defaultAnthropicBaseURL, defaultAnthropicModel, true)
		if err != nil {
			return nil, err
		}
		return &anthropicProvider{client: c}, nil
	case ProviderOpenAI:
		c, err := newClient(cfg, ProviderOpenAI, defaultOpenAIBaseURL, defaultOpenAIModel, true)
		if err != nil {
			return nil, err
		}
		return &openAIProvider{client: c}, nil
	case ProviderOllama:
		// Keyless, self-hosted: the privacy-preserving path, nothing leaves
		// the operator's machine or cluster.
		c, err := newClient(cfg, ProviderOllama, defaultOllamaBaseURL, defaultOllamaModel, false)
		if err != nil {
			return nil, err
		}
		return &ollamaProvider{client: c}, nil
	default:
		return nil, fmt.Errorf("%w: %q (supported: %s)", ErrUnsupportedProvider, cfg.Provider, strings.Join(SupportedProviders(), ", "))
	}
}

// client is the plain net/http machinery shared by every provider. No vendor
// SDKs are used anywhere in this package (spec §3).
type client struct {
	name       string
	apiKey     string
	baseURL    string
	model      string
	maxTokens  int
	httpClient *http.Client
}

func newClient(cfg Config, name, defaultBaseURL, defaultModel string, requireKey bool) (*client, error) {
	if requireKey && strings.TrimSpace(cfg.APIKey) == "" {
		return nil, fmt.Errorf("ai provider %s: %w", name, ErrMissingAPIKey)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = defaultModel
	}
	return &client{
		name:       name,
		apiKey:     strings.TrimSpace(cfg.APIKey),
		baseURL:    baseURL,
		model:      model,
		maxTokens:  maxTokens,
		httpClient: &http.Client{Timeout: timeout},
	}, nil
}

// postJSON sends payload to path and decodes a 2xx JSON body into out. The
// response body is always closed and is read under a size limit.
func (c *client) postJSON(ctx context.Context, path string, header http.Header, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("ai %s: encode request: %w", c.name, err)
	}
	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("ai %s: build request: %w", c.name, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	for key, values := range header {
		for _, v := range values {
			req.Header.Add(key, v)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ai %s: request failed: %w", c.name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("ai %s: read response: %w", c.name, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		return fmt.Errorf("ai %s: %w: %d: %s", c.name, ErrHTTPStatus, resp.StatusCode, c.safeBody(raw))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("ai %s: decode response: %w", c.name, err)
	}
	return nil
}

// safeBody truncates a provider error body for inclusion in an error string
// and, belt and braces, strips the API key in case the provider echoed it
// back. The key must never reach a log or an error message.
func (c *client) safeBody(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if c.apiKey != "" {
		s = strings.ReplaceAll(s, c.apiKey, redactedPlaceholder)
	}
	return truncate(s)
}

func truncate(s string) string {
	runes := []rune(s)
	if len(runes) <= maxErrorBodyChars {
		return s
	}
	return string(runes[:maxErrorBodyChars]) + "... (truncated)"
}

func finalizeSummary(name, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("ai %s: %w", name, ErrEmptyResponse)
	}
	return text, nil
}
