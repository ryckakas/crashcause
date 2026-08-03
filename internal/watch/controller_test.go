package watch

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"golang.org/x/time/rate"

	"github.com/ryckakas/crashcause/internal/engine"
)

// TestControllerDedupAcrossBackoffCycles is the headline guarantee of watch
// mode (spec §9): a workload that crashes for the same reason every backoff
// cycle produces ONE log line and N counter increments. The counter is what
// answers "how often did this actually crash"; the log stream must stay
// readable.
func TestControllerDedupAcrossBackoffCycles(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))
	ctx := context.Background()

	const cycles = 4
	for i := 1; i <= cycles; i++ {
		c.processPod(ctx, crashLoopPod(testPodName, testPodUID, int32(i)))
	}

	requireLineCount(t, out, 1)
	requireCauseCount(t, c, engine.CauseAppExitNonzero, cycles)

	d := out.diagnoses(t)[0]
	if d.Cause != engine.CauseAppExitNonzero {
		t.Fatalf("cause = %q, want %q", d.Cause, engine.CauseAppExitNonzero)
	}
	if d.Namespace != testNamespace || d.Container != testContainer || d.Pod != testPodName {
		t.Fatalf("identity = %s/%s container %q, want %s/%s container %q",
			d.Namespace, d.Pod, d.Container, testNamespace, testPodName, testContainer)
	}
	if d.Owner != testOwner() {
		t.Fatalf("owner = %+v, want %+v", d.Owner, testOwner())
	}
	if !d.Timestamp.Equal(testBaseTime) {
		t.Fatalf("timestamp = %s, want the injected clock %s", d.Timestamp, testBaseTime)
	}
}

// TestControllerDedupAcrossReplicaSetTwin is decision log entry 2: the dedup key
// is the WORKLOAD, not the pod, so a ReplicaSet replacing a crashing pod with an
// identical twin (new name, new UID, same ownerReference) is the same incident.
func TestControllerDedupAcrossReplicaSetTwin(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))
	ctx := context.Background()

	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 3))
	requireLineCount(t, out, 1)

	c.processPod(ctx, crashLoopPod(testTwinName, testTwinUID, 1))

	requireLineCount(t, out, 1)
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 2)
}

// TestControllerCauseChangeReemits: dedup suppresses repetition, not news. A
// workload that switches from an application exit to an OOM kill must say so
// immediately.
func TestControllerCauseChangeReemits(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))
	ctx := context.Background()

	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 1))
	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 2))
	requireLineCount(t, out, 1)

	c.processPod(ctx, oomPod(testPodName, testPodUID, 3))

	requireLineCount(t, out, 2)
	ds := out.diagnoses(t)
	if ds[0].Cause != engine.CauseAppExitNonzero {
		t.Fatalf("first cause = %q, want %q", ds[0].Cause, engine.CauseAppExitNonzero)
	}
	if ds[1].Cause != engine.CauseOOMKilled {
		t.Fatalf("second cause = %q, want %q", ds[1].Cause, engine.CauseOOMKilled)
	}
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 2)
	requireCauseCount(t, c, engine.CauseOOMKilled, 1)
}

// TestControllerReemitsAfterInterval: a condition that is still true after the
// reemit interval is worth saying again, so the incident does not fall off the
// end of a log retention window while it is still burning.
func TestControllerReemitsAfterInterval(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(6*time.Hour, 50*time.Millisecond))
	ctx := context.Background()

	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 1))
	requireLineCount(t, out, 1)

	// Not yet: the interval has not elapsed.
	clk.advance(20 * time.Millisecond)
	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 2))
	requireLineCount(t, out, 1)

	clk.advance(40 * time.Millisecond)
	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 3))

	requireLineCount(t, out, 2)
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 3)
}

// TestControllerTTLEvictionForgetsSeriesAndReemits covers the series lifecycle
// (spec §7.2): a workload that goes quiet is eventually evicted from the dedup
// cache, and its metric series is dropped with it — otherwise /metrics grows
// forever. After eviction the same crash is news again.
func TestControllerTTLEvictionForgetsSeriesAndReemits(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(50*time.Millisecond, time.Hour))
	ctx := context.Background()

	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 1))
	requireLineCount(t, out, 1)
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 1)

	// Still within the TTL: a sweep must change nothing.
	clk.advance(20 * time.Millisecond)
	c.sweepDedup()
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 1)
	if c.cache.size() != 1 {
		t.Fatalf("dedup cache size = %d, want 1 before the TTL elapses", c.cache.size())
	}

	clk.advance(time.Second)
	c.sweepDedup()

	if got := len(workloadSamples(t, c)); got != 0 {
		t.Fatalf("%d %s series survived TTL eviction, want 0; scrape:\n%s",
			got, diagnosesMetric, scrapeMetrics(t, c))
	}
	if c.cache.size() != 0 {
		t.Fatalf("dedup cache size = %d after eviction, want 0", c.cache.size())
	}

	// The next identical crash is a new incident: it emits, and the counter
	// series is recreated from zero.
	c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 2))
	requireLineCount(t, out, 2)
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 1)
}

// TestControllerLifecycleNoiseIsSilent is decision log entry 7: a pod being
// deleted whose container took SIGTERM (exit 143) is a normal rolling update.
// It must produce no line AND no counter increment — the engine returns no
// diagnosis at all, so nothing downstream ever sees it.
func TestControllerLifecycleNoiseIsSilent(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))
	ctx := context.Background()

	c.processPod(ctx, terminatingPod(testPodName, testPodUID))

	requireLineCount(t, out, 0)
	if got := len(diagnosesSamples(t, c)); got != 0 {
		t.Fatalf("%d %s series exist, want none; scrape:\n%s", got, diagnosesMetric, scrapeMetrics(t, c))
	}
	if c.cache.size() != 0 {
		t.Fatalf("dedup cache size = %d, want 0", c.cache.size())
	}
}

// TestControllerForgetPodRetiresWorkload: deleting the LAST pod of a workload
// retires its dedup keys and metric series immediately, without waiting for the
// TTL.
func TestControllerForgetPodRetiresWorkload(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))
	ctx := context.Background()

	pod := crashLoopPod(testPodName, testPodUID, 1)
	c.processPod(ctx, pod)
	requireLineCount(t, out, 1)
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 1)

	c.forgetPod(pod)

	if got := len(workloadSamples(t, c)); got != 0 {
		t.Fatalf("%d %s series survived pod deletion, want 0; scrape:\n%s",
			got, diagnosesMetric, scrapeMetrics(t, c))
	}
	if c.cache.size() != 0 {
		t.Fatalf("dedup cache size = %d after forgetting the last pod, want 0", c.cache.size())
	}
}

// TestControllerForgetPodKeepsWorkloadWithTwin: a rollout deletes pods while
// their replacements are already running. The workload is still alive, so its
// series and dedup state must survive — otherwise every rollout would reset
// dedup and re-emit.
func TestControllerForgetPodKeepsWorkloadWithTwin(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, out := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))
	ctx := context.Background()

	old := crashLoopPod(testPodName, testPodUID, 3)
	twin := crashLoopPod(testTwinName, testTwinUID, 1)
	c.processPod(ctx, old)
	c.processPod(ctx, twin)
	requireLineCount(t, out, 1)
	requireCauseCount(t, c, engine.CauseAppExitNonzero, 2)

	c.forgetPod(old)

	requireCauseCount(t, c, engine.CauseAppExitNonzero, 2)
	if c.cache.size() != 1 {
		t.Fatalf("dedup cache size = %d, want 1 (the twin keeps the workload alive)", c.cache.size())
	}

	// And the twin still dedups: the workload was never forgotten.
	c.processPod(ctx, crashLoopPod(testTwinName, testTwinUID, 2))
	requireLineCount(t, out, 1)
}

// TestControllerObserveSkipsUnchangedFingerprint checks the informer-side half
// of trigger detection without an informer: a re-delivered identical pod (what
// a resync produces) must not enqueue a second collection.
func TestControllerObserveSkipsUnchangedFingerprint(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, _ := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))

	pod := crashLoopPod(testPodName, testPodUID, 1)
	c.observe(pod)
	if got := len(c.queue); got != 1 {
		t.Fatalf("queue length after first observe = %d, want 1", got)
	}

	c.observe(pod.DeepCopy())
	if got := len(c.queue); got != 1 {
		t.Fatalf("queue length after a resync-style repeat = %d, want 1", got)
	}

	// A restart bump is a genuinely new situation.
	c.observe(crashLoopPod(testPodName, testPodUID, 2))
	if got := len(c.queue); got != 2 {
		t.Fatalf("queue length after a restart bump = %d, want 2", got)
	}

	// A healthy pod is never worth an API round trip, even though its
	// fingerprint changed.
	c.observe(healthyPod(testPodName, testPodUID))
	if got := len(c.queue); got != 2 {
		t.Fatalf("queue length after the pod recovered = %d, want 2", got)
	}
}

// TestControllerObserveReenqueuesStaticallyBrokenPod: a pod whose broken state
// never changes (constant fingerprint) must still be re-enqueued once per
// ReemitInterval — otherwise nothing ever refreshes its dedup entry and the
// TTL sweep drops its metric series while the pod is still broken.
func TestControllerObserveReenqueuesStaticallyBrokenPod(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, _ := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))

	pod := crashLoopPod(testPodName, testPodUID, 1)
	c.observe(pod)
	if got := len(c.queue); got != 1 {
		t.Fatalf("queue length after first observe = %d, want 1", got)
	}

	// Just under the reemit interval: still suppressed.
	clk.advance(time.Hour - time.Millisecond)
	c.observe(pod.DeepCopy())
	if got := len(c.queue); got != 1 {
		t.Fatalf("queue length just under ReemitInterval = %d, want 1", got)
	}

	// Exactly at the interval: re-enqueued.
	clk.advance(time.Millisecond)
	c.observe(pod.DeepCopy())
	if got := len(c.queue); got != 2 {
		t.Fatalf("queue length exactly at ReemitInterval = %d, want 2", got)
	}

	// The re-enqueue resets the clock: the next repeat is suppressed again.
	c.observe(pod.DeepCopy())
	if got := len(c.queue); got != 2 {
		t.Fatalf("queue length right after a re-enqueue = %d, want 2", got)
	}
}

// jobRunPod is a crashing pod owned by one run Job; distinct run names
// normalize to the same metric series (the trailing digits are stripped).
func jobRunPod(name string, uid types.UID, jobName string) *corev1.Pod {
	pod := crashLoopPod(name, uid, 1)
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Name:       jobName,
		UID:        types.UID("job-uid-" + jobName),
		Controller: boolPtr(true),
	}}
	return pod
}

// TestControllerForgetPodKeepsSharedJobSeries: per-run Jobs are distinct
// workloads for dedup, but normalizeOwner folds them into ONE metric series.
// Forgetting one finished run must not wipe the series other live runs still
// increment; only losing the last run may retire it.
func TestControllerForgetPodKeepsSharedJobSeries(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, _ := newTestController(t, clk, dedupConfig(6*time.Hour, time.Hour))
	ctx := context.Background()

	sharedSeries := func() []promSample {
		var out []promSample
		for _, s := range diagnosesSamples(t, c) {
			if s.labels["owner_kind"] == "Job" && s.labels["owner_name"] == "backup" {
				out = append(out, s)
			}
		}
		return out
	}

	runA := jobRunPod("backup-29471234-abcde", "job-pod-a", "backup-29471234")
	runB := jobRunPod("backup-29475678-fghij", "job-pod-b", "backup-29475678")
	c.processPod(ctx, runA)
	c.processPod(ctx, runB)

	got := sharedSeries()
	if len(got) != 1 || got[0].value != 2 {
		t.Fatalf("shared Job series = %+v, want one series with value 2; scrape:\n%s",
			got, scrapeMetrics(t, c))
	}

	c.forgetPod(runA)

	got = sharedSeries()
	if len(got) != 1 || got[0].value != 2 {
		t.Fatalf("shared Job series after forgetting one run = %+v, want it untouched; scrape:\n%s",
			got, scrapeMetrics(t, c))
	}

	c.forgetPod(runB)

	if got := sharedSeries(); len(got) != 0 {
		t.Fatalf("shared Job series after forgetting the last run = %+v, want none; scrape:\n%s",
			got, scrapeMetrics(t, c))
	}
}

// TestControllerLeaderElectedStandbyStopsPromptly: a replica that never held
// the lease has no work loop to drain, so runLeaderElected must return as soon
// as the elector does instead of waiting out the full drain timeout.
func TestControllerLeaderElectedStandbyStopsPromptly(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	cfg := dedupConfig(6*time.Hour, time.Hour)
	cfg.LeaderElect = true
	c, _, _ := newTestController(t, clk, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- c.runLeaderElected(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runLeaderElected on a canceled context = %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runLeaderElected did not return promptly for a replica that never led")
	}
}

// TestControllerReportLogSkipsIsDelta guards the poll-and-report loop: the
// Prometheus counter must grow by the DELTA of the collector's cumulative skip
// count, never by the cumulative value itself.
func TestControllerReportLogSkipsIsDelta(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	cfg := dedupConfig(6*time.Hour, time.Hour)
	cfg.CollectLogs = true
	// Effectively burst-only: exactly logBurst fetches ever pass the limiter,
	// so every fetch beyond that is a deterministic skip.
	cfg.LogRateLimit = rate.Limit(1e-9)
	c, _, _ := newTestController(t, clk, cfg)
	ctx := context.Background()

	requireSkipCounter := func(want float64) {
		t.Helper()
		samples := parseSamples(t, scrapeMetrics(t, c), "crashcause_log_fetches_skipped_total")
		if len(samples) != 1 {
			t.Fatalf("expected exactly one crashcause_log_fetches_skipped_total sample, got %d", len(samples))
		}
		if samples[0].value != want {
			t.Fatalf("crashcause_log_fetches_skipped_total = %v, want %v", samples[0].value, want)
		}
	}

	// Nothing skipped yet: reporting twice must not create movement.
	c.reportLogSkips()
	c.reportLogSkips()
	requireSkipCounter(0)

	// Exhaust the burst, then force two skips.
	for i := 0; i < logBurst+2; i++ {
		c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 1))
	}
	c.reportLogSkips()
	requireSkipCounter(2)

	// Three more skips: the counter must grow by the delta (3), not by the
	// collector's cumulative total (5) again.
	for i := 0; i < 3; i++ {
		c.processPod(ctx, crashLoopPod(testPodName, testPodUID, 1))
	}
	c.reportLogSkips()
	requireSkipCounter(5)
}
