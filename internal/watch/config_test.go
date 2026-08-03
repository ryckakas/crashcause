package watch

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/ryckakas/crashcause/internal/engine"
)

// TestNamespaceAllowed covers the watch/log allowlist convention: EMPTY MEANS
// ALL, because a controller with no --namespaces flag is meant to watch the
// whole cluster.
func TestNamespaceAllowed(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
		ns        string
		want      bool
	}{
		{name: "nil allows everything", allowlist: nil, ns: "prod", want: true},
		{name: "empty allows everything", allowlist: []string{}, ns: "prod", want: true},
		{name: "listed namespace", allowlist: []string{"prod", "staging"}, ns: "prod", want: true},
		{name: "unlisted namespace", allowlist: []string{"prod", "staging"}, ns: "kube-system", want: false},
		{name: "wildcard entry", allowlist: []string{"*"}, ns: "anything", want: true},
		{name: "wildcard among names", allowlist: []string{"prod", "*"}, ns: "kube-system", want: true},
		{name: "empty namespace is not implicitly allowed", allowlist: []string{"prod"}, ns: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := namespaceAllowed(tc.allowlist, tc.ns); got != tc.want {
				t.Fatalf("namespaceAllowed(%v, %q) = %v, want %v", tc.allowlist, tc.ns, got, tc.want)
			}
		})
	}
}

// TestAINamespaceAllowed covers the opposite convention (spec §6): the AI
// allowlist FAILS CLOSED, because it is the primary privacy control. An
// operator who configured a provider but no allowlist must not have every
// team's logs shipped off-cluster; "*" is the explicit cluster-wide opt-in.
func TestAINamespaceAllowed(t *testing.T) {
	tests := []struct {
		name      string
		allowlist []string
		ns        string
		want      bool
	}{
		{name: "nil allows nothing", allowlist: nil, ns: "prod", want: false},
		{name: "empty allows nothing", allowlist: []string{}, ns: "prod", want: false},
		{name: "listed namespace", allowlist: []string{"prod"}, ns: "prod", want: true},
		{name: "unlisted namespace", allowlist: []string{"prod"}, ns: "staging", want: false},
		{name: "wildcard is the explicit opt-in", allowlist: []string{"*"}, ns: "anything", want: true},
		{name: "wildcard among names", allowlist: []string{"prod", "*"}, ns: "staging", want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := aiNamespaceAllowed(tc.allowlist, tc.ns); got != tc.want {
				t.Fatalf("aiNamespaceAllowed(%v, %q) = %v, want %v", tc.allowlist, tc.ns, got, tc.want)
			}
		})
	}
}

// TestConfigWithDefaults pins the documented defaults: a zero Config must be
// usable, and non-positive durations must never disable a mechanism.
func TestConfigWithDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()

	if cfg.ReemitInterval != defaultReemitInterval {
		t.Fatalf("ReemitInterval = %v, want %v", cfg.ReemitInterval, defaultReemitInterval)
	}
	if cfg.DedupTTL != defaultDedupTTL {
		t.Fatalf("DedupTTL = %v, want %v", cfg.DedupTTL, defaultDedupTTL)
	}
	if cfg.LogRateLimit != defaultLogRateLimit {
		t.Fatalf("LogRateLimit = %v, want %v", cfg.LogRateLimit, defaultLogRateLimit)
	}
	if cfg.PreviousLines != defaultPreviousLines {
		t.Fatalf("PreviousLines = %v, want %v", cfg.PreviousLines, defaultPreviousLines)
	}
	if cfg.InitStuckThreshold != defaultInitStuckThreshold {
		t.Fatalf("InitStuckThreshold = %v, want %v", cfg.InitStuckThreshold, defaultInitStuckThreshold)
	}
	if cfg.LeaderElectionID != defaultLeaderElectionID {
		t.Fatalf("LeaderElectionID = %q, want %q", cfg.LeaderElectionID, defaultLeaderElectionID)
	}
	if cfg.LeaderElectionNamespace == "" {
		t.Fatal("LeaderElectionNamespace must default to something")
	}
	if cfg.sweepInterval != defaultSweepInterval {
		t.Fatalf("sweepInterval = %v, want %v", cfg.sweepInterval, defaultSweepInterval)
	}
	if cfg.clock == nil {
		t.Fatal("clock must default to a real clock")
	}

	// Negative values are replaced, explicit positive ones are preserved.
	negative := Config{ReemitInterval: -time.Second, DedupTTL: -time.Second}.withDefaults()
	if negative.ReemitInterval != defaultReemitInterval || negative.DedupTTL != defaultDedupTTL {
		t.Fatalf("negative durations must be replaced by defaults, got %v / %v",
			negative.ReemitInterval, negative.DedupTTL)
	}
	explicit := Config{ReemitInterval: time.Second, DedupTTL: 2 * time.Second, sweepInterval: 3 * time.Second}.withDefaults()
	if explicit.ReemitInterval != time.Second || explicit.DedupTTL != 2*time.Second || explicit.sweepInterval != 3*time.Second {
		t.Fatalf("explicit small durations must survive withDefaults, got %+v", explicit)
	}
}

// TestNewRejectsNilClient keeps the constructor's only hard precondition.
func TestNewRejectsNilClient(t *testing.T) {
	if _, err := New(nil, Config{}, nil, nil); err == nil {
		t.Fatal("New(nil client) must fail")
	}
}

// TestNewDiscardsNilWriter: a nil writer must be a working discard sink, not a
// panic on the first crash.
func TestNewDiscardsNilWriter(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	cfg := dedupConfig(time.Hour, time.Hour)
	cfg.clock = clk.Now
	c, err := New(fake.NewClientset(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	c.processPod(context.Background(), crashLoopPod(testPodName, testPodUID, 1))
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 1)
}
