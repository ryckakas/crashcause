package sinks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/ryckakas/crashcause/internal/engine"
)

// stdoutSink writes one JSON object per line to an io.Writer.
//
// This is the default sink (spec §7.1): a newline-delimited JSON stream is
// consumable by any log pipeline (promtail/Loki, CloudWatch, Vector, ...)
// without the tool having to integrate with it. The line carries the full
// Diagnosis, including the optional ai_summary field when the AI layer ran.
//
// Timestamps are encoded by encoding/json's time.Time marshaller, i.e.
// RFC 3339 with nanosecond precision (a valid RFC 3339 representation).
type stdoutSink struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewStdout returns a Sink that emits each Diagnosis as a single line of JSON
// to w. It is safe for concurrent use: Emit serializes writes with a mutex, so
// lines from concurrent goroutines are never interleaved. Close is a no-op —
// the sink does not own w and must not close the process's stdout.
func NewStdout(w io.Writer) Sink {
	enc := json.NewEncoder(w)
	// Diagnosis explanations and log-derived evidence routinely contain <, >
	// and &; escaping them would make the log lines needlessly unreadable
	// without making the JSON any more valid.
	enc.SetEscapeHTML(false)
	return &stdoutSink{enc: enc}
}

// Name identifies the sink in logs and error messages.
func (s *stdoutSink) Name() string { return "stdout" }

// Emit marshals d as one JSON object followed by a newline. It never blocks on
// anything but the underlying writer, so ctx is unused.
func (s *stdoutSink) Emit(_ context.Context, d engine.Diagnosis) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(d); err != nil {
		return fmt.Errorf("sinks: stdout encode diagnosis: %w", err)
	}
	return nil
}

// Close is a no-op; the writer belongs to the caller.
func (s *stdoutSink) Close(_ context.Context) error { return nil }
