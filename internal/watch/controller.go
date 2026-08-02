package watch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	toolscache "k8s.io/client-go/tools/cache"

	"golang.org/x/time/rate"

	"github.com/ryckakas/crashcause/internal/ai"
	"github.com/ryckakas/crashcause/internal/collect"
	"github.com/ryckakas/crashcause/internal/engine"
	"github.com/ryckakas/crashcause/internal/sinks"
)

// Runtime tuning constants. These are deliberately not flags: they are
// implementation details of the controller loop, not operational policy.
const (
	// informerResync re-delivers every cached pod periodically so that a
	// missed watch event cannot hide a crash forever. Resynced updates cost
	// nothing when the pod's fingerprint is unchanged.
	informerResync = 10 * time.Minute

	// queueSize bounds the collection backlog. Reaching it means collection
	// is far slower than the cluster is producing crashes; dropping is then
	// the right answer, because the next fingerprint change re-triggers.
	queueSize = 512

	// workerCount is how many pods are collected+classified concurrently.
	// Collection is I/O bound and rate limited, so a small number suffices.
	workerCount = 2

	// emitTimeout bounds a single sink Emit call, so a slow sink cannot stall
	// the worker that is holding a diagnosis.
	emitTimeout = 5 * time.Second

	// shutdownTimeout bounds graceful shutdown of the HTTP servers and the
	// final sink flush.
	shutdownTimeout = 10 * time.Second

	// logSkipPollInterval is how often the collector's rate-limiter skip
	// counter is folded into the Prometheus counter.
	logSkipPollInterval = 30 * time.Second
)

// errLostLeadership is returned by Run when leader election was enabled and
// the lease was lost while the parent context was still alive. Losing the
// lease is a clean stop: the process exits and whatever supervises it decides
// whether to restart and stand by for the lease again.
var errLostLeadership = errors.New("watch: lost leadership")

// podRecord is the controller's per-pod bookkeeping: the fingerprint that
// suppresses repeat collection, and (once collection has resolved it) the
// workload the pod belongs to, which decides when a metric series may be
// forgotten on pod deletion.
type podRecord struct {
	fingerprint string
	workload    workloadKey
	ownerKnown  bool
}

// Controller is the watch-mode runtime.
type Controller struct {
	client     kubernetes.Interface
	cfg        Config
	summarizer *ai.Summarizer
	now        func() time.Time

	// Two collectors, one with logs enabled and one without: collect.Options
	// is fixed per collector, but the log allowlist is per namespace, so the
	// choice is made per pod at collection time.
	logCollector   *collect.Collector
	noLogCollector *collect.Collector
	limiter        *rate.Limiter

	prom     *sinks.Prometheus
	sinkList []sinks.Sink

	cache *dedupCache

	mu       sync.Mutex
	podState map[types.UID]podRecord

	queue chan *corev1.Pod

	ready atomic.Bool

	aiWarnOnce sync.Once

	lastLogSkips atomic.Uint64

	// Bound listener addresses, published for tests that use :0.
	addrMu           sync.Mutex
	boundHealthAddr  string
	boundMetricsAddr string

	closeSinksOnce sync.Once
}

// New builds a controller. It creates every sink up front (so a bad Loki URL
// fails at startup rather than on the first crash) but starts nothing: all
// goroutines, informers and listeners belong to Run.
//
// summarizer may be nil (AI disabled). out receives the stdout sink's
// newline-delimited JSON; a nil out discards it.
func New(client kubernetes.Interface, cfg Config, summarizer *ai.Summarizer, out io.Writer) (*Controller, error) {
	if client == nil {
		return nil, errors.New("watch: nil kubernetes client")
	}
	if out == nil {
		out = io.Discard
	}
	cfg = cfg.withDefaults()

	c := &Controller{
		client:     client,
		cfg:        cfg,
		summarizer: summarizer,
		now:        cfg.clock,
		podState:   make(map[types.UID]podRecord),
		queue:      make(chan *corev1.Pod, queueSize),
	}

	c.limiter = rate.NewLimiter(cfg.LogRateLimit, logBurst)
	c.logCollector = collect.New(client, collect.Options{
		PreviousLogLines:   cfg.PreviousLines,
		CollectLogs:        cfg.CollectLogs,
		LogRateLimiter:     c.limiter,
		InitStuckThreshold: cfg.InitStuckThreshold,
		CollectNode:        true,
		Now:                cfg.clock,
	})
	c.noLogCollector = collect.New(client, collect.Options{
		PreviousLogLines:   cfg.PreviousLines,
		CollectLogs:        false,
		InitStuckThreshold: cfg.InitStuckThreshold,
		CollectNode:        true,
		Now:                cfg.clock,
	})

	// The stdout sink is always present (spec §7.1): it is what makes the
	// tool useful with any log pipeline without configuring anything.
	c.sinkList = []sinks.Sink{sinks.NewStdout(out)}
	if cfg.LokiURL != "" {
		loki, err := sinks.NewLoki(sinks.LokiConfig{URL: cfg.LokiURL})
		if err != nil {
			return nil, fmt.Errorf("watch: loki sink: %w", err)
		}
		c.sinkList = append(c.sinkList, loki)
	}

	// The Prometheus sink is always constructed, on its own registry: the
	// counter must count every crash whether or not anyone scrapes it, and
	// MetricsAddr only decides whether an endpoint exposes it.
	c.prom = sinks.NewPrometheus(nil)

	c.cache = newDedupCache(c.now, cfg.DedupTTL, cfg.ReemitInterval)

	return c, nil
}

// Run starts the controller and blocks until ctx is canceled (or, with
// leader election enabled, until the lease is lost). It always shuts the HTTP
// servers down and closes the sinks before returning.
func (c *Controller) Run(ctx context.Context) error {
	servers, err := c.startServers(ctx)
	if err != nil {
		return err
	}
	defer func() {
		shutdownServers(ctx, servers)
		c.closeSinks(ctx)
	}()

	if c.cfg.LeaderElect {
		return c.runLeaderElected(ctx)
	}
	return c.runWork(ctx)
}

// runWork wires the informers, waits for their caches, and runs the worker
// loops until ctx is canceled. With leader election enabled this is the body
// that only ever runs while holding the lease.
func (c *Controller) runWork(ctx context.Context) error {
	factories, err := c.buildFactories()
	if err != nil {
		return err
	}

	for _, f := range factories {
		informer := f.Core().V1().Pods().Informer()
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if pod, ok := obj.(*corev1.Pod); ok {
					c.observe(pod)
				}
			},
			UpdateFunc: func(_, newObj any) {
				if pod, ok := newObj.(*corev1.Pod); ok {
					c.observe(pod)
				}
			},
			DeleteFunc: func(obj any) {
				c.forgetPod(podFromDeleteEvent(obj))
			},
		}); err != nil {
			return fmt.Errorf("watch: register pod event handler: %w", err)
		}
	}

	for _, f := range factories {
		f.Start(ctx.Done())
	}
	for _, f := range factories {
		f.WaitForCacheSync(ctx.Done())
	}
	if ctx.Err() != nil {
		return nil //nolint:nilerr // canceled before we ever became ready: a clean stop
	}

	c.ready.Store(true)
	defer c.ready.Store(false)
	slog.Info("crashcause watch started",
		"namespaces", namespacesForLog(c.cfg.Namespaces),
		"selector", c.cfg.LabelSelector,
		"collect_logs", c.cfg.CollectLogs,
	)

	var wg sync.WaitGroup
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.worker(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.sweepLoop(ctx)
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.logSkipLoop(ctx)
	}()

	<-ctx.Done()
	wg.Wait()
	return nil
}

// buildFactories returns one informer factory per watched namespace, or a
// single cluster-wide factory when no namespace allowlist is configured.
// Scoping at the factory level (rather than filtering in the handler) means
// the controller only ever needs list/watch in the namespaces it was granted.
func (c *Controller) buildFactories() ([]informers.SharedInformerFactory, error) {
	selector := strings.TrimSpace(c.cfg.LabelSelector)
	opts := []informers.SharedInformerOption{
		informers.WithTweakListOptions(func(lo *metav1.ListOptions) {
			if selector != "" {
				lo.LabelSelector = selector
			}
		}),
	}

	if len(c.cfg.Namespaces) == 0 {
		return []informers.SharedInformerFactory{
			informers.NewSharedInformerFactoryWithOptions(c.client, informerResync, opts...),
		}, nil
	}

	seen := make(map[string]struct{}, len(c.cfg.Namespaces))
	var out []informers.SharedInformerFactory
	for _, ns := range c.cfg.Namespaces {
		ns = strings.TrimSpace(ns)
		if ns == "" {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		nsOpts := append(append([]informers.SharedInformerOption{}, opts...), informers.WithNamespace(ns))
		out = append(out, informers.NewSharedInformerFactoryWithOptions(c.client, informerResync, nsOpts...))
	}
	if len(out) == 0 {
		return nil, errors.New("watch: --namespaces contained no usable namespace")
	}
	return out, nil
}

// observe is the informer-side half of trigger detection: it records the pod's
// fingerprint and enqueues collection only when the fingerprint changed AND
// the new state is worth an API call. It never blocks the informer.
func (c *Controller) observe(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	fp := podFingerprint(pod)

	c.mu.Lock()
	rec, known := c.podState[pod.UID]
	unchanged := known && rec.fingerprint == fp
	rec.fingerprint = fp
	c.podState[pod.UID] = rec
	c.mu.Unlock()

	if unchanged {
		return
	}
	if !warrantsTrigger(pod, c.now(), c.cfg.InitStuckThreshold) {
		return
	}
	c.enqueue(pod)
}

// enqueue hands a pod to a worker without ever blocking the informer
// goroutine. A full queue drops the item: the pod's next status change
// re-triggers, and blocking here would stall event delivery for every pod.
func (c *Controller) enqueue(pod *corev1.Pod) {
	select {
	case c.queue <- pod:
	default:
		slog.Warn("crashcause watch queue full, dropping pod update",
			"namespace", pod.Namespace, "pod", pod.Name)
	}
}

func (c *Controller) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case pod := <-c.queue:
			c.processPod(ctx, pod)
		}
	}
}

// processPod collects, classifies and (subject to dedup) emits for one pod.
//
// It is the whole business logic of watch mode and is deliberately callable
// synchronously, which is what makes the dedup behavior testable without a
// running informer.
func (c *Controller) processPod(ctx context.Context, pod *corev1.Pod) {
	if pod == nil {
		return
	}
	collector := c.collectorFor(pod.Namespace)
	inputs, err := collector.ForPodObject(ctx, pod, "")
	if err != nil {
		slog.Warn("crashcause collect failed",
			"namespace", pod.Namespace, "pod", pod.Name, "error", err)
		return
	}

	for i := range inputs {
		in := inputs[i]
		diagnoses := engine.Classify(in)
		if len(diagnoses) == 0 {
			// Nothing wrong, or ordinary lifecycle churn (exit 143 during a
			// rolling update). Emitting anything here would turn every deploy
			// into a stream of false findings (decision log entry 7).
			continue
		}
		// Watch mode reports the PRIMARY diagnosis only; the full ranked list
		// is an inspect --verbose concern, not a stream of log lines.
		d := diagnoses[0]

		// Remember which workload this pod belongs to, whether or not the
		// diagnosis is emitted, so pod deletion can retire the right series.
		c.recordWorkload(pod.UID, d.Namespace, d.Owner, d.Pod)

		// Counters count every crash, always, before any dedup decision
		// (spec §5.2 / §7.2).
		c.prom.IncCrash(d.Namespace, d.Owner, d.Cause)

		key := emissionKey{
			workload:  workloadKeyFor(d.Namespace, d.Owner, d.Pod),
			container: d.Container,
			cause:     d.Cause,
		}
		if !c.cache.observe(key, d.Owner) {
			continue
		}

		c.maybeSummarize(ctx, &d, in)
		c.emit(ctx, d)
	}
}

// collectorFor picks the logs-enabled or logs-disabled collector for a
// namespace. When logs are disabled globally both collectors behave
// identically; the allowlist only narrows further.
func (c *Controller) collectorFor(namespace string) *collect.Collector {
	if !c.cfg.CollectLogs {
		return c.noLogCollector
	}
	if !namespaceAllowed(c.cfg.LogNamespaces, namespace) {
		return c.noLogCollector
	}
	return c.logCollector
}

// maybeSummarize attaches an AI summary when every gate passes: a summarizer
// is configured, the namespace is on the AI allowlist, the cause is one the
// AI layer can actually help with, and the diagnosis is genuinely being
// emitted (suppressed diagnoses never reach a provider).
func (c *Controller) maybeSummarize(ctx context.Context, d *engine.Diagnosis, in engine.Inputs) {
	if c.summarizer == nil {
		return
	}
	if !aiNamespaceAllowed(c.cfg.AINamespaces, d.Namespace) {
		return
	}
	if d.Cause != engine.CauseAppExitNonzero && d.Cause != engine.CauseUnknown {
		return
	}

	summary, err := c.summarizer.Summarize(ctx, ai.Request{
		Cause:    string(d.Cause),
		Evidence: d.Evidence,
		LogTail:  in.LogTail,
		Image:    in.Image,
	})
	if err != nil {
		// AI never fails a diagnosis (spec §6). Warn once so a persistently
		// broken provider does not produce a line per crash.
		c.aiWarnOnce.Do(func() {
			slog.Warn("crashcause AI summary unavailable; emitting rules-only diagnoses", "error", err)
		})
		return
	}
	if summary = strings.TrimSpace(summary); summary != "" {
		d.AISummary = &summary
	}
}

// emit writes one diagnosis to every log-style sink, sequentially and with a
// bounded per-sink deadline. A sink failure is logged and otherwise ignored:
// a telemetry destination being down must never take the controller with it.
func (c *Controller) emit(ctx context.Context, d engine.Diagnosis) {
	for _, s := range c.sinkList {
		emitCtx, cancel := context.WithTimeout(ctx, emitTimeout)
		err := s.Emit(emitCtx, d)
		cancel()
		if err != nil {
			slog.Warn("crashcause sink emit failed", "sink", s.Name(), "error", err)
		}
	}
}

// recordWorkload attaches the resolved workload identity to a pod's record.
func (c *Controller) recordWorkload(uid types.UID, namespace string, owner engine.Owner, podName string) {
	wk := workloadKeyFor(namespace, owner, podName)
	c.mu.Lock()
	rec := c.podState[uid]
	rec.workload = wk
	rec.ownerKnown = true
	c.podState[uid] = rec
	c.mu.Unlock()
}

// forgetPod drops a deleted pod's bookkeeping and, when it was the last
// tracked pod of its workload, retires that workload's dedup keys and metric
// series. A workload whose replacement pod is already known keeps both, so a
// ReplicaSet rollout does not reset dedup state.
func (c *Controller) forgetPod(pod *corev1.Pod) {
	if pod == nil {
		return
	}
	c.mu.Lock()
	rec, known := c.podState[pod.UID]
	delete(c.podState, pod.UID)
	stillPresent := false
	if known && rec.ownerKnown {
		for _, other := range c.podState {
			if other.ownerKnown && other.workload == rec.workload {
				stillPresent = true
				break
			}
		}
	}
	c.mu.Unlock()

	if !known || !rec.ownerKnown || stillPresent {
		return
	}
	if owner, ok := c.cache.forgetWorkload(rec.workload); ok {
		c.prom.ForgetSeries(rec.workload.namespace, owner)
	}
}

// podFromDeleteEvent unwraps the tombstone the informer delivers when a
// delete was observed only as a cache resync gap.
func podFromDeleteEvent(obj any) *corev1.Pod {
	switch v := obj.(type) {
	case *corev1.Pod:
		return v
	case toolscache.DeletedFinalStateUnknown:
		if pod, ok := v.Obj.(*corev1.Pod); ok {
			return pod
		}
	}
	return nil
}

// sweepLoop evicts expired dedup keys on a ticker. Eviction is what bounds
// both the cache and the /metrics series set over weeks of workload churn.
func (c *Controller) sweepLoop(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.sweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sweepDedup()
		}
	}
}

// sweepDedup runs one eviction pass and retires the metric series of every
// workload that lost its last key.
func (c *Controller) sweepDedup() {
	for _, f := range c.cache.sweep() {
		c.prom.ForgetSeries(f.namespace, f.owner)
	}
}

// logSkipLoop folds the collector's rate-limiter skip counter into the
// Prometheus counter periodically, so the limiter itself needs no dependency
// on the metrics sink.
func (c *Controller) logSkipLoop(ctx context.Context) {
	ticker := time.NewTicker(logSkipPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.reportLogSkips()
			return
		case <-ticker.C:
			c.reportLogSkips()
		}
	}
}

// reportLogSkips reports the delta since the last poll.
func (c *Controller) reportLogSkips() {
	total := c.logCollector.LogFetchesSkipped()
	prev := c.lastLogSkips.Swap(total)
	if total > prev {
		c.prom.AddLogFetchesSkipped(total - prev)
	}
}

// closeSinks flushes and closes every sink exactly once, under a bounded
// deadline so an unreachable Loki cannot hold shutdown hostage.
//
// ctx is derived from the caller's context via context.WithoutCancel for the
// same reason as shutdownServers: Run's deferred call happens exactly when
// ctx has already been canceled, and an already-dead context would abort the
// flush instead of honoring shutdownTimeout.
func (c *Controller) closeSinks(ctx context.Context) {
	c.closeSinksOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		for _, s := range c.sinkList {
			if err := s.Close(ctx); err != nil {
				slog.Warn("crashcause sink close failed", "sink", s.Name(), "error", err)
			}
			if l, ok := s.(sinks.LokiSink); ok {
				if dropped := l.Dropped(); dropped > 0 {
					slog.Warn("crashcause loki sink dropped entries", "dropped", dropped)
				}
			}
		}
	})
}

// namespacesForLog renders the namespace allowlist for the startup log line.
func namespacesForLog(ns []string) string {
	if len(ns) == 0 {
		return "(all)"
	}
	return strings.Join(ns, ",")
}
