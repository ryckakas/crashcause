package sinks

import (
	"net/http"
	"regexp"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ryckakas/crashcause/internal/engine"
)

// jobTrailingIndex matches a trailing all-digit segment on a Job name, e.g.
// the "-29471234" in a CronJob-created "backup-job-29471234". This is the
// suffix Kubernetes appends when a CronJob spawns a Job (derived from the
// scheduled time), so stripping it collapses one CronJob's Jobs back into a
// single logical owner_name.
var jobTrailingIndex = regexp.MustCompile(`-\d+$`)

// jobTrailingGenerateName matches a trailing 5-character lowercase
// alphanumeric segment, e.g. the "-x7k2p" in "ci-run-x7k2p". This is the
// random suffix appended by generateName for Jobs created directly (not via
// a CronJob), so stripping it collapses per-run Jobs into one logical name.
var jobTrailingGenerateName = regexp.MustCompile(`-[a-z0-9]{5}$`)

// Prometheus is a metrics sink (spec §7.2). It is deliberately NOT a Sink
// implementation: watch mode's log-style sinks (stdout, Loki, ...) are
// deduplicated so a flapping container doesn't spam a log line every few
// seconds, but the crash *counter* must reflect reality regardless of that
// dedup, so callers invoke IncCrash directly on every observed crash and
// only route to the dedup-gated Sink set for the human-readable emission.
type Prometheus struct {
	diagnoses      *prometheus.CounterVec
	logFetchesSkip prometheus.Counter
	gatherer       prometheus.Gatherer
}

// NewPrometheus creates the crashcause metrics and registers them on reg.
//
// A nil reg makes the sink create and own a fresh *prometheus.Registry
// instead of registering on the caller's registry (or the global default).
// This is useful in tests, where a scratch registry avoids cross-test
// collisions, and for exposing an isolated /metrics endpoint that carries
// only crashcause's own series (no Go runtime/process metrics mixed in).
func NewPrometheus(reg prometheus.Registerer) *Prometheus {
	var gatherer prometheus.Gatherer
	if reg == nil {
		r := prometheus.NewRegistry()
		reg = r
		gatherer = r
	} else if g, ok := reg.(prometheus.Gatherer); ok {
		// Most Registerer implementations used in practice (notably
		// *prometheus.Registry) are also Gatherers. When that holds, serve
		// exactly what was registered on so /metrics matches reality.
		gatherer = g
	} else {
		// The caller passed a Registerer that isn't also a Gatherer (e.g. a
		// custom wrapper). Fall back to the global default gatherer rather
		// than fail; this only matters for Handler(), not for the counters
		// themselves, which are always registered on the passed-in reg.
		gatherer = prometheus.DefaultGatherer
	}

	factory := promauto.With(reg)

	diagnoses := factory.NewCounterVec(prometheus.CounterOpts{
		Name: "crashcause_diagnoses_total",
		Help: "Total number of crash diagnoses, by namespace, owner and cause. " +
			"Confidence is deliberately not a label: a confidence shift for the " +
			"same underlying cause must not split one logical series into two.",
	}, []string{"namespace", "owner_kind", "owner_name", "cause"})

	logFetchesSkip := factory.NewCounter(prometheus.CounterOpts{
		Name: "crashcause_log_fetches_skipped_total",
		Help: "Total number of log fetches skipped by the pod-log rate limiter (spec §8).",
	})

	return &Prometheus{
		diagnoses:      diagnoses,
		logFetchesSkip: logFetchesSkip,
		gatherer:       gatherer,
	}
}

// normalizeOwner applies cardinality-discipline heuristics to an Owner
// before it becomes label values. Shared by IncCrash and ForgetSeries so the
// two always agree on which series a workload maps to.
//
// CI clusters routinely mint thousands of uniquely-named Jobs (one per
// CronJob run, one per pipeline invocation); without normalization each one
// would mint its own permanent time series, and the /metrics endpoint would
// grow without bound over weeks of churn. So:
//
//  1. A Job owned by a CronJob is folded into a single "CronJob" series
//     keyed by the CronJob's name (all runs of the same CronJob count
//     together).
//  2. A bare Job (no CronJobName) has its name best-effort truncated to
//     strip the auto-generated run-specific suffix: first a trailing
//     all-digit segment (CronJob-style, in case CronJobName wasn't
//     populated), then a trailing 5-char generateName-style segment.
//     This is a heuristic, not a guarantee — it recognizes the two
//     conventions Kubernetes itself uses, nothing more.
//  3. Anything else (Deployment, StatefulSet, DaemonSet, ReplicaSet, ...)
//     passes through unchanged.
//
// Empty Kind/Name are left empty rather than replaced with a placeholder.
func normalizeOwner(owner engine.Owner) (kind, name string) {
	kind, name = owner.Kind, owner.Name

	if kind != "Job" {
		return kind, name
	}

	if owner.CronJobName != "" {
		return "CronJob", owner.CronJobName
	}

	if name == "" {
		return kind, name
	}

	trimmed := name
	if stripped := jobTrailingIndex.ReplaceAllString(trimmed, ""); stripped != "" {
		trimmed = stripped
	}
	if stripped := jobTrailingGenerateName.ReplaceAllString(trimmed, ""); stripped != "" {
		trimmed = stripped
	}
	return kind, trimmed
}

// IncCrash increments crashcause_diagnoses_total for one observed crash.
//
// This must be called on EVERY crash the tool observes, independent of
// whatever dedup logic gates the log-style sinks (stdout/Loki) — the counter
// exists precisely to answer "how many times has this actually crashed",
// which dedup'd log lines cannot answer on their own.
func (p *Prometheus) IncCrash(namespace string, owner engine.Owner, cause engine.CauseCode) {
	ownerKind, ownerName := normalizeOwner(owner)
	p.diagnoses.WithLabelValues(namespace, ownerKind, ownerName, string(cause)).Inc()
}

// ForgetSeries removes every crashcause_diagnoses_total series belonging to
// one workload (all causes), so the /metrics endpoint stays bounded over
// weeks of workload churn instead of accumulating a permanent series for
// every workload that ever existed. Callers invoke this on dedup-cache TTL
// expiry or workload deletion.
//
// Deletion matches only on namespace/owner_kind/owner_name — deliberately
// not on cause — so all cause series for the workload are forgotten
// together, not just whichever cause happened to be last observed.
//
// The same normalizeOwner heuristic used by IncCrash is applied here, so a
// caller that passes back the Owner it incremented with always removes the
// series it created, even though that series is keyed by normalized labels
// the caller never sees. Note that normalization is self-consistent, not
// convergent across owner shapes: a Job carrying CronJobName folds to
// owner_kind="CronJob" while the same Job without it truncates to
// owner_kind="Job", and those are different series. Callers must therefore
// forget with the same Owner value they incremented with (watch mode has it:
// the dedup-cache key holds the Owner).
func (p *Prometheus) ForgetSeries(namespace string, owner engine.Owner) {
	ownerKind, ownerName := normalizeOwner(owner)
	p.diagnoses.DeletePartialMatch(prometheus.Labels{
		"namespace":  namespace,
		"owner_kind": ownerKind,
		"owner_name": ownerName,
	})
}

// Handler returns the http.Handler serving /metrics for this sink's
// registry: the *prometheus.Registry it was given (or created) when that
// registry is also a Gatherer, or prometheus.DefaultGatherer otherwise (see
// NewPrometheus). Each call builds a fresh handler; the underlying gatherer
// reference is fixed at construction time.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.gatherer, promhttp.HandlerOpts{})
}

// AddLogFetchesSkipped adds delta to crashcause_log_fetches_skipped_total.
//
// The pod-log token-bucket rate limiter (spec §8) tracks its own skip count
// internally (e.g. an atomic counter incremented on every skip); watch mode
// polls that counter periodically and reports the difference here rather
// than calling this once per skip, so the limiter itself doesn't need a
// dependency on Prometheus. prometheus.Counter is itself safe for concurrent
// use, so no additional locking is needed here.
func (p *Prometheus) AddLogFetchesSkipped(delta uint64) {
	if delta == 0 {
		return
	}
	p.logFetchesSkip.Add(float64(delta))
}
