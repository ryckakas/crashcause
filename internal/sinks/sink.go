// Package sinks defines the destinations a Diagnosis can be emitted to:
// stdout, Prometheus, and Loki. Each sink is independently enabled by the
// caller (typically the watch command) and failures in one sink must not
// affect the others.
package sinks

import (
	"context"

	"github.com/ryckakas/crashcause/internal/engine"
)

// Sink receives every emitted Diagnosis. Emit must not block indefinitely.
type Sink interface {
	Name() string
	Emit(ctx context.Context, d engine.Diagnosis) error
	Close(ctx context.Context) error
}
