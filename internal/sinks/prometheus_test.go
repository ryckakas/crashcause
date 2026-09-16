package sinks

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ryckakas/crashcause/internal/engine"
)

func promNewTestSink(t *testing.T) *Prometheus {
	t.Helper()
	return NewPrometheus(prometheus.NewRegistry())
}

// promExposition renders p's /metrics endpoint through its real HTTP
// handler and returns the raw Prometheus text-exposition body. Tests assert
// against this black-box output rather than any in-process collector API,
// since that is exactly what a real scraper would see.
func promExposition(t *testing.T, p *Prometheus) string {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics endpoint status = %d, want %d", rec.Code, http.StatusOK)
	}
	return rec.Body.String()
}

// promSeriesLines returns every non-comment exposition line whose sample
// name is exactly metric (i.e. the line is "metric{...} value" or, for an
// unlabeled metric, "metric value"). If the metric currently has no series
// at all, the family's HELP/TYPE comments (and the metric itself) are
// simply absent from the body, so this returns an empty slice.
func promSeriesLines(t *testing.T, p *Prometheus, metric string) []string {
	t.Helper()
	var lines []string
	for _, line := range strings.Split(promExposition(t, p), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, metric+"{") || strings.HasPrefix(line, metric+" ") {
			lines = append(lines, line)
		}
	}
	return lines
}

// promSampleValue finds the single exposition line for metric whose label
// section contains every "key=\"value\"" pair in labels, and returns its
// value. It fails the test if the match count is not exactly one, so it
// doubles as an assertion that the expected label set (names and values)
// actually appears in the output.
func promSampleValue(t *testing.T, p *Prometheus, metric string, labels ...string) float64 {
	t.Helper()
	var matches []string
	for _, line := range promSeriesLines(t, p, metric) {
		ok := true
		for _, lbl := range labels {
			if !strings.Contains(line, lbl) {
				ok = false
				break
			}
		}
		if ok {
			matches = append(matches, line)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("metric %s labels %v: got %d matching lines, want 1: %v", metric, labels, len(matches), matches)
	}
	fields := strings.Fields(matches[0])
	value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
	if err != nil {
		t.Fatalf("parse value from exposition line %q: %v", matches[0], err)
	}
	return value
}

func TestPrometheusIncCrash(t *testing.T) {
	p := promNewTestSink(t)
	owner := engine.Owner{Kind: "Deployment", Name: "checkout"}

	p.IncCrash("payments", owner, engine.CauseOOMKilled)
	p.IncCrash("payments", owner, engine.CauseOOMKilled)
	p.IncCrash("payments", owner, engine.CauseOOMKilled)

	got := promSampleValue(t, p, "crashcause_diagnoses_total",
		`namespace="payments"`, `owner_kind="Deployment"`, `owner_name="checkout"`, `cause="oom_killed"`)
	if got != 3 {
		t.Fatalf("crashcause_diagnoses_total = %v, want 3", got)
	}

	// A different cause for the same workload must be its own series, not
	// merged into the one above.
	p.IncCrash("payments", owner, engine.CauseAppExitNonzero)
	gotOther := promSampleValue(t, p, "crashcause_diagnoses_total",
		`namespace="payments"`, `owner_kind="Deployment"`, `owner_name="checkout"`, `cause="app_exit_nonzero"`)
	if gotOther != 1 {
		t.Fatalf("crashcause_diagnoses_total (app_exit_nonzero) = %v, want 1", gotOther)
	}
	if lines := promSeriesLines(t, p, "crashcause_diagnoses_total"); len(lines) != 2 {
		t.Fatalf("series count = %d, want 2: %v", len(lines), lines)
	}
}

func TestPrometheusOwnerNormalization(t *testing.T) {
	cases := []struct {
		name      string
		owner     engine.Owner
		wantKind  string
		wantOwner string
	}{
		{
			name:      "job with cronjob name folds to CronJob",
			owner:     engine.Owner{Kind: "Job", Name: "backup-job-29471234", CronJobName: "backup-job"},
			wantKind:  "CronJob",
			wantOwner: "backup-job",
		},
		{
			name:      "cronjob-spawned job name truncates trailing digits",
			owner:     engine.Owner{Kind: "Job", Name: "backup-job-29471234"},
			wantKind:  "Job",
			wantOwner: "backup-job",
		},
		{
			name:      "generateName-style job name truncates trailing random suffix",
			owner:     engine.Owner{Kind: "Job", Name: "ci-run-x7k2p"},
			wantKind:  "Job",
			wantOwner: "ci-run",
		},
		{
			name:      "deployment name is never truncated",
			owner:     engine.Owner{Kind: "Deployment", Name: "ci-run-x7k2p"},
			wantKind:  "Deployment",
			wantOwner: "ci-run-x7k2p",
		},
		{
			name:      "job name with no recognizable suffix is untouched",
			owner:     engine.Owner{Kind: "Job", Name: "my-worker"},
			wantKind:  "Job",
			wantOwner: "my-worker",
		},
		{
			name:      "pathological name that would truncate to empty is kept intact",
			owner:     engine.Owner{Kind: "Job", Name: "-99999"},
			wantKind:  "Job",
			wantOwner: "-99999",
		},
		{
			name:      "empty kind and name stay empty, no placeholder",
			owner:     engine.Owner{},
			wantKind:  "",
			wantOwner: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, gotOwner := normalizeOwner(tc.owner)
			if gotKind != tc.wantKind || gotOwner != tc.wantOwner {
				t.Fatalf("normalizeOwner(%+v) = (%q, %q), want (%q, %q)",
					tc.owner, gotKind, gotOwner, tc.wantKind, tc.wantOwner)
			}
		})
	}
}

func TestPrometheusForgetSeries(t *testing.T) {
	p := promNewTestSink(t)
	appA := engine.Owner{Kind: "Deployment", Name: "app-a"}
	appB := engine.Owner{Kind: "Deployment", Name: "app-b"}

	p.IncCrash("ns1", appA, engine.CauseOOMKilled)
	p.IncCrash("ns1", appA, engine.CauseAppExitNonzero)
	p.IncCrash("ns1", appB, engine.CauseOOMKilled)

	if lines := promSeriesLines(t, p, "crashcause_diagnoses_total"); len(lines) != 3 {
		t.Fatalf("series count before forget = %d, want 3: %v", len(lines), lines)
	}

	p.ForgetSeries("ns1", appA)

	lines := promSeriesLines(t, p, "crashcause_diagnoses_total")
	if len(lines) != 1 {
		t.Fatalf("series count after forget = %d, want 1: %v", len(lines), lines)
	}
	gotB := promSampleValue(t, p, "crashcause_diagnoses_total",
		`namespace="ns1"`, `owner_kind="Deployment"`, `owner_name="app-b"`, `cause="oom_killed"`)
	if gotB != 1 {
		t.Fatalf("app-b series survived with wrong value = %v, want 1", gotB)
	}
}

// TestPrometheusForgetSeriesNormalization checks that ForgetSeries applies
// the SAME owner normalization as IncCrash, so a caller that hands back the
// Owner it incremented with always removes the series it created — even
// though that series is keyed by normalized labels the caller never sees.
//
// Note the deliberate limitation: normalization is only self-consistent, not
// convergent across owner shapes. A Job carrying CronJobName is folded to
// owner_kind="CronJob", while the same Job WITHOUT CronJobName is truncated
// to owner_kind="Job" — different label sets, so forgetting by one does not
// remove the other. Watch mode must therefore forget with the same Owner it
// incremented with (which it has: the dedup-cache key holds it).
func TestPrometheusForgetSeriesNormalization(t *testing.T) {
	t.Run("cronjob fold", func(t *testing.T) {
		p := promNewTestSink(t)
		owner := engine.Owner{Kind: "Job", Name: "backup-job-29471234", CronJobName: "backup-job"}
		p.IncCrash("ns1", owner, engine.CauseOOMKilled)

		lines := promSeriesLines(t, p, "crashcause_diagnoses_total")
		if len(lines) != 1 {
			t.Fatalf("series count before forget = %d, want 1: %v", len(lines), lines)
		}
		if !strings.Contains(lines[0], `owner_kind="CronJob"`) ||
			!strings.Contains(lines[0], `owner_name="backup-job"`) {
			t.Fatalf("series was not normalized to the CronJob owner: %v", lines)
		}

		p.ForgetSeries("ns1", owner)

		if lines := promSeriesLines(t, p, "crashcause_diagnoses_total"); len(lines) != 0 {
			t.Fatalf("series count after forget = %d, want 0: %v", len(lines), lines)
		}
	})

	t.Run("bare job truncation", func(t *testing.T) {
		p := promNewTestSink(t)
		owner := engine.Owner{Kind: "Job", Name: "ci-run-x7k2p"}
		p.IncCrash("ns1", owner, engine.CauseAppExitNonzero)

		lines := promSeriesLines(t, p, "crashcause_diagnoses_total")
		if len(lines) != 1 {
			t.Fatalf("series count before forget = %d, want 1: %v", len(lines), lines)
		}
		if !strings.Contains(lines[0], `owner_kind="Job"`) ||
			!strings.Contains(lines[0], `owner_name="ci-run"`) {
			t.Fatalf("series was not truncated to the logical Job owner: %v", lines)
		}

		// A later run of the same pipeline has a different pod-level name but
		// normalizes to the same series, so forgetting with it works too —
		// that is the point of the truncation heuristic.
		p.ForgetSeries("ns1", engine.Owner{Kind: "Job", Name: "ci-run-b3m9q"})

		if lines := promSeriesLines(t, p, "crashcause_diagnoses_total"); len(lines) != 0 {
			t.Fatalf("series count after forget = %d, want 0: %v", len(lines), lines)
		}
	})
}

// TestPrometheusLogFetchesSkipped checks that AddLogFetchesSkipped adds the
// delta to crashcause_log_fetches_skipped_total (a plain Counter, spec §8).
func TestPrometheusLogFetchesSkipped(t *testing.T) {
	p := promNewTestSink(t)

	p.AddLogFetchesSkipped(2)
	p.AddLogFetchesSkipped(3)
	p.AddLogFetchesSkipped(0) // must be a no-op, not an error

	got := promSampleValue(t, p, "crashcause_log_fetches_skipped_total")
	if got != 5 {
		t.Fatalf("crashcause_log_fetches_skipped_total = %v, want 5", got)
	}
}

func TestPrometheusHandlerServesMetrics(t *testing.T) {
	p := promNewTestSink(t)
	p.IncCrash("ns1", engine.Owner{Kind: "Deployment", Name: "app-a"}, engine.CauseOOMKilled)
	p.AddLogFetchesSkipped(1)

	body := promExposition(t, p)
	if !strings.Contains(body, "crashcause_diagnoses_total") {
		t.Fatalf("body missing crashcause_diagnoses_total:\n%s", body)
	}
	if !strings.Contains(body, "crashcause_log_fetches_skipped_total") {
		t.Fatalf("body missing crashcause_log_fetches_skipped_total:\n%s", body)
	}
}

// TestPrometheusHandlerDefaultGatherer checks that a nil Registerer results
// in a Handler backed by the sink's own isolated registry (not the global
// default), so metrics from other packages/tests never leak into it.
func TestPrometheusHandlerDefaultGatherer(t *testing.T) {
	p := NewPrometheus(nil)
	p.IncCrash("ns1", engine.Owner{Kind: "Deployment", Name: "solo"}, engine.CauseEvicted)

	body := promExposition(t, p)
	if !strings.Contains(body, `owner_name="solo"`) {
		t.Fatalf("body missing expected series:\n%s", body)
	}
}
