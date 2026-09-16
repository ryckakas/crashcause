package watch

import (
	"sync"
	"time"

	"github.com/ryckakas/crashcause/internal/engine"
	"github.com/ryckakas/crashcause/internal/sinks"
)

// workloadKey identifies the logical workload a diagnosis belongs to.
//
// The owner field is "Kind/Name" of the resolved owner (Deployment/api rather
// than the ReplicaSet), falling back to the POD name for a bare pod that has
// no controller. Pod names are otherwise deliberately absent: a ReplicaSet
// replacing a crashing pod with an identical twin must not read as a new
// incident (decision log entry 2).
type workloadKey struct {
	namespace string
	owner     string
}

// workloadKeyFor builds the workload identity of one diagnosis. podName is
// only used when the pod has no owner at all.
func workloadKeyFor(namespace string, owner engine.Owner, podName string) workloadKey {
	name := podName
	if owner.Kind != "" && owner.Name != "" {
		name = owner.Kind + "/" + owner.Name
	}
	return workloadKey{namespace: namespace, owner: name}
}

// emissionKey is the spec §5.2 dedup key: (namespace, owner, container,
// cause). restartCount is deliberately NOT part of it — including it would
// re-emit on every backoff cycle, which is the exact failure the workload-
// keyed design replaced.
type emissionKey struct {
	workload  workloadKey
	container string
	cause     engine.CauseCode
}

// causeKey tracks the last cause seen for one container of one workload, so
// that a CHANGE of cause emits immediately even when the new cause's own key
// was seen (and suppressed) earlier — e.g. an A→B→A flap inside one
// reemit interval.
type causeKey struct {
	workload  workloadKey
	container string
}

// owner is stored verbatim (not just its string form) because
// sinks.Prometheus.ForgetSeries must be called with the SAME engine.Owner
// value IncCrash was called with: normalization is self-consistent, not
// convergent, so a reconstructed Owner could delete a different series than
// the one that was created.
type dedupEntry struct {
	owner    engine.Owner
	lastEmit time.Time
	lastSeen time.Time
}

// forgottenWorkload is a workload whose last dedup key was evicted; the
// caller drops its Prometheus series.
type forgottenWorkload struct {
	namespace string
	owner     engine.Owner
}

// dedupCache is the in-memory, TTL-bounded emission cache. There is no
// persistence by design (spec §5.2): after a controller restart every key
// re-emits once, which is documented and accepted.
type dedupCache struct {
	mu     sync.Mutex
	now    func() time.Time
	ttl    time.Duration
	reemit time.Duration

	entries    map[emissionKey]*dedupEntry
	lastCause  map[causeKey]engine.CauseCode
	byWorkload map[workloadKey]map[emissionKey]struct{}
}

func newDedupCache(now func() time.Time, ttl, reemit time.Duration) *dedupCache {
	if now == nil {
		now = time.Now
	}
	return &dedupCache{
		now:        now,
		ttl:        ttl,
		reemit:     reemit,
		entries:    make(map[emissionKey]*dedupEntry),
		lastCause:  make(map[causeKey]engine.CauseCode),
		byWorkload: make(map[workloadKey]map[emissionKey]struct{}),
	}
}

// observe records one observed diagnosis and reports whether it should reach
// the log-style sinks. It emits when the key is unseen, when the cause changed
// for this (workload, container), or when ReemitInterval has elapsed since the
// key's last emission; otherwise it suppresses.
//
// It is called for EVERY observed crash — the Prometheus counter is
// incremented by the caller before this, never gated by it.
func (c *dedupCache) observe(k emissionKey, owner engine.Owner) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	ck := causeKey{workload: k.workload, container: k.container}
	prevCause, hadPrev := c.lastCause[ck]

	entry, seen := c.entries[k]
	var emit bool
	switch {
	case !seen:
		emit = true
	case hadPrev && prevCause != k.cause:
		emit = true
	case now.Sub(entry.lastEmit) >= c.reemit:
		emit = true
	}

	if !seen {
		entry = &dedupEntry{}
		c.entries[k] = entry
		set := c.byWorkload[k.workload]
		if set == nil {
			set = make(map[emissionKey]struct{})
			c.byWorkload[k.workload] = set
		}
		set[k] = struct{}{}
	}

	entry.owner = owner
	entry.lastSeen = now
	if emit {
		entry.lastEmit = now
	}
	c.lastCause[ck] = k.cause

	return emit
}

// sweep evicts every key untouched for longer than the TTL and returns the
// workloads that lost their last key, so the caller can drop their metric
// series (spec §7.2: series lifecycle is tied to this cache).
//
// The TTL runs from the last time a key was OBSERVED, not last emitted: a
// workload that is still crashing keeps both its cache entry and its metric
// series, and only genuinely quiet workloads are forgotten.
func (c *dedupCache) sweep() []forgottenWorkload {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	var forgotten []forgottenWorkload
	evicted := false

	for k, entry := range c.entries {
		if now.Sub(entry.lastSeen) <= c.ttl {
			continue
		}
		evicted = true
		delete(c.entries, k)
		if set := c.byWorkload[k.workload]; set != nil {
			delete(set, k)
			if len(set) == 0 {
				delete(c.byWorkload, k.workload)
				forgotten = append(forgotten, forgottenWorkload{
					namespace: k.workload.namespace,
					owner:     entry.owner,
				})
			}
		}
	}

	if evicted {
		c.pruneCauseIndexLocked()
	}
	return forgotten
}

// forgetWorkload drops every key of one workload and returns the Owner that
// was used to increment its metrics, if the workload was tracked at all.
func (c *dedupCache) forgetWorkload(wk workloadKey) (engine.Owner, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	set, ok := c.byWorkload[wk]
	if !ok {
		return engine.Owner{}, false
	}
	var owner engine.Owner
	for k := range set {
		if entry, ok := c.entries[k]; ok {
			owner = entry.owner
			delete(c.entries, k)
		}
	}
	delete(c.byWorkload, wk)
	c.pruneCauseIndexLocked()
	return owner, true
}

// hasSeries reports whether any live entry still maps to the given metric
// series identity. O(entries) is fine: the cache is bounded by the TTL sweep
// and forget calls are rare (pod deletion, sweep).
func (c *dedupCache) hasSeries(key sinks.SeriesKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, entry := range c.entries {
		if sinks.SeriesKeyFor(k.workload.namespace, entry.owner) == key {
			return true
		}
	}
	return false
}

// pruneCauseIndexLocked drops last-cause records for (workload, container)
// pairs that no longer have any live key, so the index cannot outlive the
// entries it describes.
func (c *dedupCache) pruneCauseIndexLocked() {
	live := make(map[causeKey]struct{}, len(c.entries))
	for k := range c.entries {
		live[causeKey{workload: k.workload, container: k.container}] = struct{}{}
	}
	for ck := range c.lastCause {
		if _, ok := live[ck]; !ok {
			delete(c.lastCause, ck)
		}
	}
}

// size reports how many emission keys are currently cached (tests, and the
// bounded-memory claim in the spec).
func (c *dedupCache) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
