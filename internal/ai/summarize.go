package ai

import (
	"context"
	"fmt"
	"time"
)

// Summarizer ties redaction and a Provider together. It is the supported
// entry point into this package: it guarantees that nothing reaches a
// provider without passing through the Redactor first, and that a call is
// always bounded by a deadline.
//
// It never fails a diagnosis. Callers attach a successful summary to
// Diagnosis.AISummary; on error they emit a one-line notice and proceed with
// the rules-only result (spec §6, failure semantics).
type Summarizer struct {
	provider Provider
	redactor *Redactor
	timeout  time.Duration
}

// NewSummarizer returns a Summarizer over p using r.
//
// A nil r means redaction is disabled (--ai-redact=false); that is the
// caller's explicit choice and is honored, not silently overridden.
func NewSummarizer(p Provider, r *Redactor) *Summarizer {
	return &Summarizer{provider: p, redactor: r, timeout: DefaultTimeout}
}

// Provider returns the underlying provider, or nil.
func (s *Summarizer) Provider() Provider {
	if s == nil {
		return nil
	}
	return s.provider
}

// Summarize redacts req, calls the provider, and returns the summary or an
// error. It applies DefaultTimeout when ctx carries no deadline of its own,
// so a hung provider cannot stall a diagnosis.
//
// The returned error is never fatal to the caller: on error the caller emits
// a one-line notice and reports the rules-only diagnosis. The configured API
// key never appears in the returned error.
func (s *Summarizer) Summarize(ctx context.Context, req Request) (string, error) {
	if s == nil || s.provider == nil {
		return "", ErrNoProvider
	}

	req = s.redact(req)

	timeout := s.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	summary, err := s.provider.Summarize(ctx, req)
	if err != nil {
		return "", fmt.Errorf("ai summary unavailable: %w", err)
	}
	return summary, nil
}

// redact scrubs every string field of req. Cause is a fixed snake_case code
// and no pattern can match it, but it is passed through anyway so that no
// field is exempt by construction.
func (s *Summarizer) redact(req Request) Request {
	if s.redactor == nil {
		return req
	}
	return Request{
		Cause:    s.redactor.Redact(req.Cause),
		Evidence: s.redactor.RedactLines(req.Evidence),
		LogTail:  s.redactor.RedactLines(req.LogTail),
		Image:    s.redactor.Redact(req.Image),
	}
}
