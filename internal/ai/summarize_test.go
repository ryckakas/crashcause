package ai

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeProvider captures the Request it was handed so tests can assert that
// redaction happened BEFORE anything reached a provider.
type fakeProvider struct {
	got      Request
	calls    int
	hadDL    bool
	summary  string
	err      error
	blockFor time.Duration
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Summarize(ctx context.Context, req Request) (string, error) {
	f.calls++
	f.got = req
	_, f.hadDL = ctx.Deadline()
	if f.blockFor > 0 {
		select {
		case <-time.After(f.blockFor):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return f.summary, f.err
}

func secretRequest() Request {
	return Request{
		Cause: "app_exit_nonzero",
		Evidence: []string{
			"exit code 1",
			`env dump: DB_PASSWORD=hunter2 AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`,
		},
		LogTail: []string{
			`Connecting to postgres://app:s3cr3t@db.prod.svc:5432/billing`,
			`GET /v1/invoices -> 401 (Authorization: Bearer abc123DEFghi456)`,
			`dial tcp 10.2.3.4:5432: connect: connection refused`,
			`	at com.example.billing.InvoiceRepository.findAll(InvoiceRepository.java:117)`,
		},
		Image: "ghcr.io/example/billing:1.4.2",
	}
}

func TestSummarizerRedactsBeforeSending(t *testing.T) {
	r, err := NewRedactor(RedactorOptions{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	fake := &fakeProvider{summary: "  Postgres refused the connection.  "}
	s := NewSummarizer(fake, r)

	got, err := s.Summarize(context.Background(), secretRequest())
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if want := "Postgres refused the connection."; strings.TrimSpace(got) != want {
		t.Errorf("Summarize() = %q, want %q", got, want)
	}
	if fake.calls != 1 {
		t.Errorf("provider called %d times, want 1", fake.calls)
	}

	sent := strings.Join(append(append([]string{fake.got.Cause, fake.got.Image}, fake.got.Evidence...), fake.got.LogTail...), "\n")
	for _, secret := range []string{"hunter2", "s3cr3t", "AKIAIOSFODNN7EXAMPLE", "abc123DEFghi456"} {
		if strings.Contains(sent, secret) {
			t.Errorf("SECRET %q REACHED THE PROVIDER:\n%s", secret, sent)
		}
	}
	// Diagnostic facts must survive the trip.
	for _, keep := range []string{
		"app_exit_nonzero",
		"ghcr.io/example/billing:1.4.2",
		"exit code 1",
		"db.prod.svc:5432",
		"10.2.3.4:5432",
		"InvoiceRepository.java:117",
	} {
		if !strings.Contains(sent, keep) {
			t.Errorf("diagnostic fact %q was destroyed:\n%s", keep, sent)
		}
	}
}

func TestSummarizerAppliesDefaultDeadline(t *testing.T) {
	fake := &fakeProvider{summary: "ok"}
	s := NewSummarizer(fake, nil)

	if _, err := s.Summarize(context.Background(), Request{Cause: "unknown"}); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !fake.hadDL {
		t.Error("Summarize must impose a deadline when the caller's context has none")
	}
}

func TestSummarizerWithTimeoutAppliesConfiguredDeadline(t *testing.T) {
	// A provider slower than the configured timeout must be cut off by it,
	// proving the value is actually used rather than DefaultTimeout (15s).
	fake := &fakeProvider{blockFor: 5 * time.Second}
	s := NewSummarizerWithTimeout(fake, nil, 40*time.Millisecond)

	start := time.Now()
	if _, err := s.Summarize(context.Background(), Request{Cause: "unknown"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the configured deadline to fire, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("configured timeout was ignored (%s elapsed)", elapsed)
	}
}

func TestSummarizerWithTimeoutRejectsNonPositive(t *testing.T) {
	fake := &fakeProvider{summary: "ok"}
	for _, d := range []time.Duration{0, -time.Second} {
		if got := NewSummarizerWithTimeout(fake, nil, d).timeout; got != DefaultTimeout {
			t.Errorf("NewSummarizerWithTimeout(%v).timeout = %v, want DefaultTimeout", d, got)
		}
	}
}

func TestSummarizerKeepsCallerDeadline(t *testing.T) {
	fake := &fakeProvider{blockFor: 5 * time.Second}
	s := NewSummarizer(fake, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	start := time.Now()
	got, err := s.Summarize(ctx, Request{Cause: "unknown"})
	if err == nil {
		t.Fatalf("expected an error, got %q", got)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected the caller's deadline to be honored, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Summarize ignored the caller deadline (%s)", elapsed)
	}
}

func TestSummarizerNilRedactorSendsUnredacted(t *testing.T) {
	// --ai-redact=false is an explicit operator choice and is honored.
	fake := &fakeProvider{summary: "ok"}
	s := NewSummarizer(fake, nil)

	if _, err := s.Summarize(context.Background(), secretRequest()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !strings.Contains(strings.Join(fake.got.Evidence, "\n"), "hunter2") {
		t.Error("a nil Redactor must skip redaction rather than half-apply it")
	}
}

func TestSummarizerProviderErrorIsReturnedNotPanicked(t *testing.T) {
	sentinel := errors.New("provider is down")
	fake := &fakeProvider{err: sentinel}
	r, err := NewRedactor(RedactorOptions{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}
	s := NewSummarizer(fake, r)

	got, err := s.Summarize(context.Background(), secretRequest())
	if got != "" {
		t.Errorf("summary should be empty on error, got %q", got)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("provider error should be wrapped, got %v", err)
	}
}

func TestSummarizerEmptyProviderResult(t *testing.T) {
	fake := &fakeProvider{err: ErrEmptyResponse}
	s := NewSummarizer(fake, nil)
	if _, err := s.Summarize(context.Background(), Request{}); !errors.Is(err, ErrEmptyResponse) {
		t.Errorf("expected ErrEmptyResponse, got %v", err)
	}
}

func TestSummarizerWithoutProvider(t *testing.T) {
	var s *Summarizer
	if _, err := s.Summarize(context.Background(), Request{}); !errors.Is(err, ErrNoProvider) {
		t.Errorf("nil Summarizer: expected ErrNoProvider, got %v", err)
	}
	if s.Provider() != nil {
		t.Error("nil Summarizer.Provider() should be nil")
	}

	empty := NewSummarizer(nil, nil)
	if _, err := empty.Summarize(context.Background(), Request{}); !errors.Is(err, ErrNoProvider) {
		t.Errorf("expected ErrNoProvider, got %v", err)
	}
}

func TestSummarizerProviderAccessor(t *testing.T) {
	fake := &fakeProvider{}
	if got := NewSummarizer(fake, nil).Provider(); got != Provider(fake) {
		t.Errorf("Provider() = %v, want the configured provider", got)
	}
}

// TestSummarizerEndToEndWithRealProvider wires the real HTTP client behind the
// Summarizer to prove redaction happens on the wire, not just in the fake.
func TestSummarizerEndToEndRedactsOnTheWire(t *testing.T) {
	srv, rec := newTestServer(t, 200, `{"message":{"content":"Postgres refused the connection."}}`)
	p, err := New(Config{Provider: ProviderOllama, BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := NewRedactor(RedactorOptions{})
	if err != nil {
		t.Fatalf("NewRedactor: %v", err)
	}

	if _, err := NewSummarizer(p, r).Summarize(context.Background(), secretRequest()); err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	_, _, body := rec.snapshot()
	msgs := chatMessages(t, body)
	if len(msgs) != 2 {
		t.Fatalf("expected system+user messages, got %v", msgs)
	}
	prompt := msgs[1]["content"]
	for _, secret := range []string{"hunter2", "s3cr3t", "AKIAIOSFODNN7EXAMPLE", "abc123DEFghi456"} {
		if strings.Contains(prompt, secret) {
			t.Errorf("SECRET %q WAS SENT OVER THE WIRE:\n%s", secret, prompt)
		}
	}
	if !strings.Contains(prompt, "db.prod.svc:5432") {
		t.Errorf("host:port should survive redaction:\n%s", prompt)
	}
}
