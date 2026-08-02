package watch

import (
	"testing"
	"time"

	"github.com/ryckakas/crashcause/internal/engine"
)

// key builds an emission key for the canonical test workload.
func key(container string, cause engine.CauseCode) emissionKey {
	return emissionKey{workload: testWorkloadKey(), container: container, cause: cause}
}

func TestWorkloadKeyFor(t *testing.T) {
	tests := []struct {
		name    string
		owner   engine.Owner
		podName string
		want    workloadKey
	}{
		{
			name:    "owned pod keys on the owner",
			owner:   engine.Owner{Kind: "Deployment", Name: "api"},
			podName: "api-5d4f-aaaaa",
			want:    workloadKey{namespace: testNamespace, owner: "Deployment/api"},
		},
		{
			name:    "twin of the same owner shares the key",
			owner:   engine.Owner{Kind: "Deployment", Name: "api"},
			podName: "api-5d4f-bbbbb",
			want:    workloadKey{namespace: testNamespace, owner: "Deployment/api"},
		},
		{
			name:    "bare pod falls back to the pod name",
			owner:   engine.Owner{},
			podName: "debug-shell",
			want:    workloadKey{namespace: testNamespace, owner: "debug-shell"},
		},
		{
			name:    "half-populated owner falls back to the pod name",
			owner:   engine.Owner{Kind: "ReplicaSet"},
			podName: "orphan",
			want:    workloadKey{namespace: testNamespace, owner: "orphan"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := workloadKeyFor(testNamespace, tc.owner, tc.podName); got != tc.want {
				t.Fatalf("workloadKeyFor = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDedupCacheObserveSuppressesRepeats is the core dedup contract: first sight
// emits, repetition inside the reemit interval does not.
func TestDedupCacheObserveSuppressesRepeats(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c := newDedupCache(clk.Now, time.Hour, 10*time.Minute)
	k := key(testContainer, engine.CauseAppExitNonzero)

	if !c.observe(k, testOwner()) {
		t.Fatal("first observation must emit")
	}
	for i := 0; i < 5; i++ {
		clk.advance(time.Minute)
		if c.observe(k, testOwner()) {
			t.Fatalf("observation %d inside the reemit interval must be suppressed", i)
		}
	}
	if c.size() != 1 {
		t.Fatalf("cache size = %d, want 1", c.size())
	}
}

// TestDedupCacheReemitsAfterInterval: the gap is measured from the last
// EMISSION, so a suppressed observation must not push the deadline out.
func TestDedupCacheReemitsAfterInterval(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c := newDedupCache(clk.Now, time.Hour, 10*time.Minute)
	k := key(testContainer, engine.CauseAppExitNonzero)

	if !c.observe(k, testOwner()) {
		t.Fatal("first observation must emit")
	}
	clk.advance(9 * time.Minute)
	if c.observe(k, testOwner()) {
		t.Fatal("observation before the interval elapsed must be suppressed")
	}
	clk.advance(time.Minute) // exactly 10m since the last emission
	if !c.observe(k, testOwner()) {
		t.Fatal("observation at the reemit interval must emit")
	}
	clk.advance(time.Second)
	if c.observe(k, testOwner()) {
		t.Fatal("the reemit deadline must restart from the emission that just happened")
	}
}

// TestDedupCacheCauseFlapEmits is why the cache tracks the last cause per
// (workload, container) separately from the emission keys: in an A→B→A flap the
// return to A is news even though A's own key was seen minutes ago.
func TestDedupCacheCauseFlapEmits(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c := newDedupCache(clk.Now, time.Hour, time.Hour)

	a := key(testContainer, engine.CauseAppExitNonzero)
	b := key(testContainer, engine.CauseOOMKilled)

	if !c.observe(a, testOwner()) {
		t.Fatal("A: first observation must emit")
	}
	clk.advance(time.Minute)
	if !c.observe(b, testOwner()) {
		t.Fatal("B: an unseen key must emit")
	}
	clk.advance(time.Minute)
	if !c.observe(a, testOwner()) {
		t.Fatal("A again: the cause changed back, which must emit even inside the reemit interval")
	}
	clk.advance(time.Minute)
	if c.observe(a, testOwner()) {
		t.Fatal("A repeated with no cause change must be suppressed again")
	}
	if c.size() != 2 {
		t.Fatalf("cache size = %d, want 2 (one key per cause)", c.size())
	}
}

// TestDedupCacheContainersAreIndependent: two containers of one workload are
// separate incidents, and the last-cause index must not leak between them.
func TestDedupCacheContainersAreIndependent(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c := newDedupCache(clk.Now, time.Hour, time.Hour)

	if !c.observe(key("app", engine.CauseAppExitNonzero), testOwner()) {
		t.Fatal("app: first observation must emit")
	}
	if !c.observe(key("sidecar", engine.CauseAppExitNonzero), testOwner()) {
		t.Fatal("sidecar: a different container is a different key and must emit")
	}
	if c.observe(key("app", engine.CauseAppExitNonzero), testOwner()) {
		t.Fatal("app repeated must be suppressed")
	}
	if c.size() != 2 {
		t.Fatalf("cache size = %d, want 2", c.size())
	}
}

// TestDedupCacheSweepEvictsAndReportsWorkloads: the TTL runs from the last time
// a key was OBSERVED, so a still-crashing workload keeps its series while a
// genuinely quiet one is forgotten.
func TestDedupCacheSweepEvictsAndReportsWorkloads(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c := newDedupCache(clk.Now, 30*time.Minute, time.Hour)

	quiet := key("app", engine.CauseAppExitNonzero)
	c.observe(quiet, testOwner())

	// Nothing is stale yet.
	clk.advance(30 * time.Minute)
	if forgotten := c.sweep(); len(forgotten) != 0 {
		t.Fatalf("sweep at exactly the TTL forgot %d workloads, want 0", len(forgotten))
	}
	if c.size() != 1 {
		t.Fatalf("cache size = %d, want 1", c.size())
	}

	clk.advance(time.Second)
	forgotten := c.sweep()
	if len(forgotten) != 1 {
		t.Fatalf("sweep past the TTL forgot %d workloads, want 1", len(forgotten))
	}
	if forgotten[0].namespace != testNamespace {
		t.Fatalf("forgotten namespace = %q, want %q", forgotten[0].namespace, testNamespace)
	}
	// The Owner must come back verbatim: ForgetSeries only deletes the series
	// IncCrash created if it is handed the same Owner value.
	if forgotten[0].owner != testOwner() {
		t.Fatalf("forgotten owner = %+v, want %+v", forgotten[0].owner, testOwner())
	}
	if c.size() != 0 {
		t.Fatalf("cache size = %d after eviction, want 0", c.size())
	}

	// The cause index was pruned with the entries, so the key is unseen again.
	if !c.observe(quiet, testOwner()) {
		t.Fatal("an evicted key must emit on its next observation")
	}
}

// TestDedupCacheSweepKeepsWorkloadWithLiveKey: a workload only counts as
// forgotten when its LAST key expires; losing one cause while another is still
// live must not retire the series.
func TestDedupCacheSweepKeepsWorkloadWithLiveKey(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c := newDedupCache(clk.Now, 30*time.Minute, time.Hour)

	stale := key("app", engine.CauseAppExitNonzero)
	live := key("sidecar", engine.CauseOOMKilled)
	c.observe(stale, testOwner())

	clk.advance(20 * time.Minute)
	c.observe(live, testOwner())

	clk.advance(20 * time.Minute) // stale is 40m old, live is 20m old
	forgotten := c.sweep()
	if len(forgotten) != 0 {
		t.Fatalf("sweep forgot %d workloads while one key is still live, want 0", len(forgotten))
	}
	if c.size() != 1 {
		t.Fatalf("cache size = %d, want 1 (only the stale key evicted)", c.size())
	}

	clk.advance(31 * time.Minute)
	if forgotten := c.sweep(); len(forgotten) != 1 {
		t.Fatalf("sweep forgot %d workloads once the last key expired, want 1", len(forgotten))
	}
}

// TestDedupCacheForgetWorkload is the pod-deletion path: it drops every key of
// one workload and returns the Owner the caller must forget the series with.
func TestDedupCacheForgetWorkload(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c := newDedupCache(clk.Now, time.Hour, time.Hour)

	c.observe(key("app", engine.CauseAppExitNonzero), testOwner())
	c.observe(key("app", engine.CauseOOMKilled), testOwner())
	c.observe(key("sidecar", engine.CauseAppExitNonzero), testOwner())
	if c.size() != 3 {
		t.Fatalf("cache size = %d, want 3", c.size())
	}

	owner, ok := c.forgetWorkload(testWorkloadKey())
	if !ok {
		t.Fatal("forgetWorkload must report a tracked workload")
	}
	if owner != testOwner() {
		t.Fatalf("owner = %+v, want %+v", owner, testOwner())
	}
	if c.size() != 0 {
		t.Fatalf("cache size = %d after forgetWorkload, want 0", c.size())
	}

	// Every key of the workload is genuinely gone, cause index included.
	if !c.observe(key("app", engine.CauseAppExitNonzero), testOwner()) {
		t.Fatal("a forgotten workload's key must emit again")
	}
}

func TestDedupCacheForgetUnknownWorkload(t *testing.T) {
	c := newDedupCache(newFakeClock(testBaseTime).Now, time.Hour, time.Hour)
	if _, ok := c.forgetWorkload(workloadKey{namespace: "other", owner: "Deployment/none"}); ok {
		t.Fatal("forgetWorkload must report false for a workload it never tracked")
	}
}

// TestDedupCacheNamespacesAreIndependent guards against a cross-namespace
// collision between two workloads with the same owner name.
func TestDedupCacheNamespacesAreIndependent(t *testing.T) {
	c := newDedupCache(newFakeClock(testBaseTime).Now, time.Hour, time.Hour)

	prod := emissionKey{
		workload:  workloadKey{namespace: "prod", owner: "Deployment/api"},
		container: testContainer,
		cause:     engine.CauseAppExitNonzero,
	}
	staging := prod
	staging.workload.namespace = "staging"

	if !c.observe(prod, testOwner()) {
		t.Fatal("prod must emit")
	}
	if !c.observe(staging, testOwner()) {
		t.Fatal("the same workload name in another namespace must emit independently")
	}
}

// TestDedupCacheNilClockDefaults keeps the zero-config constructor usable.
func TestDedupCacheNilClockDefaults(t *testing.T) {
	c := newDedupCache(nil, time.Hour, time.Hour)
	if c.now == nil {
		t.Fatal("newDedupCache(nil, ...) must substitute a real clock")
	}
	if !c.observe(key(testContainer, engine.CauseUnknown), testOwner()) {
		t.Fatal("first observation must emit")
	}
}
