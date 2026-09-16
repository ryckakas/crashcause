// Package watch implements crashcause's controller mode (spec §5.2): pod
// informers feed the collector, the pure engine classifies what they see, and
// the resulting diagnoses are deduplicated per workload before reaching the
// log-style sinks.
//
// The package owns three pieces of policy that the rest of the tool
// deliberately does not know about:
//
//   - trigger detection — which pod updates are worth an API round trip at all
//     (a fingerprint of per-container restart counts and state kinds, so the
//     same observed state never causes collection twice);
//   - emission dedup — the workload-keyed cache from decision log entry 2,
//     which keeps a crashlooping workload from writing a log line every
//     backoff cycle while the Prometheus counter still counts every crash;
//   - the runtime — informers, leader election, health/metrics endpoints and
//     graceful shutdown.
package watch

import (
	"os"
	"time"

	"golang.org/x/time/rate"
)

// Defaults applied by Config.withDefaults. They mirror the flag defaults in
// internal/cli/watch.go so that programmatic callers and CLI users get the
// same behavior.
const (
	defaultReemitInterval = time.Hour
	defaultDedupTTL       = 6 * time.Hour
	// defaultLogRateLimit is 10 pod-log fetches per minute (spec §8).
	defaultLogRateLimit = rate.Limit(10.0 / 60.0)
	// logBurst lets a small cluster-wide crash burst through immediately
	// while the sustained rate stays at LogRateLimit.
	logBurst = 5

	defaultPreviousLines      = int64(60)
	defaultInitStuckThreshold = 10 * time.Minute

	defaultLeaderElectionID = "crashcause"
	defaultSweepInterval    = time.Minute
)

// podNamespaceEnv is the conventional downward-API environment variable a
// Helm chart sets on the controller pod; it is the best default for the
// leader-election lease namespace when running in-cluster.
const podNamespaceEnv = "POD_NAMESPACE"

// Config is the full runtime configuration of the watch controller.
//
// The zero value is usable: withDefaults fills in every field that has a
// documented default. Namespace allowlists follow two different (deliberate)
// conventions:
//
//   - Namespaces and LogNamespaces: empty means "all namespaces";
//   - AINamespaces: empty means "AI is off everywhere", because per §6 the
//     per-namespace allowlist is the PRIMARY privacy control and must never
//     default to sending every team's logs to a provider. Operators opt into
//     cluster-wide AI explicitly with the single entry "*".
type Config struct {
	// Namespaces is the watch allowlist; empty watches every namespace.
	Namespaces []string
	// LabelSelector filters watched pods (applied to the informer's list and
	// watch calls, so filtered pods never enter the cache at all).
	LabelSelector string

	// ReemitInterval is the minimum gap between two emissions of the same
	// dedup key while the condition persists. Default 1h.
	ReemitInterval time.Duration
	// DedupTTL is how long an unseen dedup key is remembered before it is
	// evicted (and its metric series forgotten). Default 6h.
	DedupTTL time.Duration

	// CollectLogs is the logCollection.enabled equivalent. False means no
	// pods/log request is ever issued.
	CollectLogs bool
	// LogNamespaces restricts which namespaces' pods may have logs fetched;
	// empty means all of them.
	LogNamespaces []string
	// LogRateLimit is the sustained pod-log fetch rate. Default 10/min.
	LogRateLimit rate.Limit
	// PreviousLines is the log tail length per container. Default 60.
	PreviousLines int64
	// InitStuckThreshold is how long an init container may run before it is
	// considered stuck. Default 10m.
	InitStuckThreshold time.Duration

	// AINamespaces is the AI allowlist. Empty keeps AI inert even when a
	// Summarizer is configured; the entry "*" enables it everywhere.
	AINamespaces []string

	// MetricsAddr serves /metrics when non-empty.
	MetricsAddr string
	// HealthAddr serves /healthz and /readyz when non-empty.
	HealthAddr string
	// LokiURL adds the Loki push sink when non-empty.
	LokiURL string

	// LeaderElect enables client-go leader election (default off: one replica
	// is the documented normal case).
	LeaderElect             bool
	LeaderElectionNamespace string
	LeaderElectionID        string

	// clock is an injection point for tests; nil means time.Now.
	clock func() time.Time
	// sweepInterval overrides the dedup sweep ticker in tests.
	sweepInterval time.Duration
}

func (cfg Config) withDefaults() Config {
	if cfg.ReemitInterval <= 0 {
		cfg.ReemitInterval = defaultReemitInterval
	}
	if cfg.DedupTTL <= 0 {
		cfg.DedupTTL = defaultDedupTTL
	}
	if cfg.LogRateLimit <= 0 {
		cfg.LogRateLimit = defaultLogRateLimit
	}
	if cfg.PreviousLines <= 0 {
		cfg.PreviousLines = defaultPreviousLines
	}
	if cfg.InitStuckThreshold <= 0 {
		cfg.InitStuckThreshold = defaultInitStuckThreshold
	}
	if cfg.LeaderElectionID == "" {
		cfg.LeaderElectionID = defaultLeaderElectionID
	}
	if cfg.LeaderElectionNamespace == "" {
		cfg.LeaderElectionNamespace = leaderElectionNamespaceDefault()
	}
	if cfg.clock == nil {
		cfg.clock = time.Now
	}
	if cfg.sweepInterval <= 0 {
		cfg.sweepInterval = defaultSweepInterval
	}
	return cfg
}

// leaderElectionNamespaceDefault prefers the downward-API namespace of the
// running pod and falls back to "default" when running outside a cluster.
func leaderElectionNamespaceDefault() string {
	if ns := os.Getenv(podNamespaceEnv); ns != "" {
		return ns
	}
	return "default"
}

// namespaceAllowed reports whether ns passes an allowlist whose empty value
// means "everything" (Namespaces, LogNamespaces).
func namespaceAllowed(allowlist []string, ns string) bool {
	if len(allowlist) == 0 {
		return true
	}
	for _, a := range allowlist {
		if a == ns || a == "*" {
			return true
		}
	}
	return false
}

// aiNamespaceAllowed reports whether ns passes the AI allowlist, whose empty
// value means "nothing" (spec §6: the allowlist is the primary control, so it
// must fail closed). The wildcard entry "*" is the explicit opt-in to all
// namespaces.
func aiNamespaceAllowed(allowlist []string, ns string) bool {
	for _, a := range allowlist {
		if a == "*" || a == ns {
			return true
		}
	}
	return false
}
