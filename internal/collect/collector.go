// Package collect gathers the Kubernetes inputs that the pure classification
// engine needs in order to diagnose a crashed container.
//
// Everything in this package talks to the cluster exclusively through a
// kubernetes.Interface, and reads the current time exclusively through
// Options.Now, so a Collector can be driven end-to-end by
// k8s.io/client-go/kubernetes/fake plus a frozen clock.
//
// Collection is deliberately forgiving: only failing to read the pod itself
// (or naming a container that does not exist) is an error. Events, logs and
// node conditions are best-effort — when they are unavailable the collector
// degrades and the engine simply sees less evidence.
package collect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/ryckakas/crashcause/internal/engine"

	"golang.org/x/time/rate"
)

const (
	defaultPreviousLogLines   int64 = 60
	defaultInitStuckThreshold       = 10 * time.Minute
)

// ErrContainerNotFound is returned (wrapped) when the requested container
// filter names a container that does not exist in the pod at all.
var ErrContainerNotFound = errors.New("container not found in pod")

var errNilPod = errors.New("nil pod")

var waitingReasonsWarrantingDiagnosis = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
}

// Options configures a Collector.
type Options struct {
	// PreviousLogLines is the number of trailing log lines to keep per
	// container. Values <= 0 are replaced with the default (60) by New.
	PreviousLogLines int64
	// CollectLogs enables container log collection. When false no log API call
	// is ever made and every Inputs reports LogsUnavailable.
	CollectLogs bool
	// LogRateLimiter throttles pods/log fetches. nil means unlimited (inspect
	// mode); watch mode passes a 10/min limiter.
	LogRateLimiter *rate.Limiter
	// InitStuckThreshold is how long a Running init container must have been
	// running before it warrants diagnosis. Values <= 0 are replaced with the
	// default (10m) by New.
	InitStuckThreshold time.Duration
	// CollectNode enables fetching node conditions for the pod's node. The zero
	// value is false; use DefaultOptions for the documented default of true.
	CollectNode bool
	// Now is the injectable clock. nil is replaced with time.Now by New.
	Now func() time.Time
}

// DefaultOptions returns the documented defaults: 60 log lines, log collection
// on, node condition collection on, a 10 minute init-stuck threshold, no rate
// limiter and the real clock.
func DefaultOptions() Options {
	return Options{
		PreviousLogLines:   defaultPreviousLogLines,
		CollectLogs:        true,
		LogRateLimiter:     nil,
		InitStuckThreshold: defaultInitStuckThreshold,
		CollectNode:        true,
		Now:                time.Now,
	}
}

// Collector turns pods into engine.Inputs. It is safe for concurrent use.
type Collector struct {
	client kubernetes.Interface
	opts   Options

	logSkips atomic.Uint64

	ownerMu    sync.Mutex
	ownerCache map[types.UID]ownerCacheEntry
}

// New returns a Collector using the given client, applying documented defaults
// to any unset Options field that has one.
func New(client kubernetes.Interface, opts Options) *Collector {
	if opts.PreviousLogLines <= 0 {
		opts.PreviousLogLines = defaultPreviousLogLines
	}
	if opts.InitStuckThreshold <= 0 {
		opts.InitStuckThreshold = defaultInitStuckThreshold
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Collector{
		client:     client,
		opts:       opts,
		ownerCache: make(map[types.UID]ownerCacheEntry),
	}
}

// LogFetchesSkipped reports how many log fetches were skipped because the rate
// limiter denied them.
func (c *Collector) LogFetchesSkipped() uint64 {
	return c.logSkips.Load()
}

func (c *Collector) now() time.Time {
	return c.opts.Now()
}

// ForPod fetches the named pod and collects inputs for it. Only the pod GET
// failing (or an unknown containerFilter) is reported as an error.
func (c *Collector) ForPod(ctx context.Context, namespace, pod, containerFilter string) ([]engine.Inputs, error) {
	p, err := c.client.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get pod %s/%s: %w", namespace, pod, err)
	}
	return c.ForPodObject(ctx, p, containerFilter)
}

// target is one container (or one synthetic pod-level entry) worth diagnosing.
type target struct {
	name   string
	kind   engine.ContainerKind
	status *corev1.ContainerStatus // nil for a pod-level synthetic entry
	// skipLogs suppresses log collection for entries whose container provably
	// never produced any (an unschedulable pod).
	skipLogs bool
}

// ForPodObject collects inputs from an already-fetched pod. It never re-GETs
// the pod. It returns a nil slice and a nil error when nothing about the pod
// warrants diagnosis; callers map that to "no findings".
func (c *Collector) ForPodObject(ctx context.Context, pod *corev1.Pod, containerFilter string) ([]engine.Inputs, error) {
	if pod == nil {
		return nil, fmt.Errorf("collect inputs: %w", errNilPod)
	}
	if containerFilter != "" && !podHasContainer(pod, containerFilter) {
		return nil, fmt.Errorf("%q in pod %s/%s: %w", containerFilter, pod.Namespace, pod.Name, ErrContainerNotFound)
	}

	now := c.now()
	targets := c.containerTargets(pod, containerFilter, now)

	// Cheap pre-check: a healthy, non-Pending pod with no status reason costs
	// exactly one API call (the pod GET) — no events, no logs, no node.
	if len(targets) == 0 && pod.Status.Phase != corev1.PodPending && pod.Status.Reason == "" {
		return nil, nil
	}

	events := c.collectEvents(ctx, pod)

	if len(targets) == 0 {
		t, ok := podLevelTarget(pod, containerFilter, events)
		if !ok {
			return nil, nil
		}
		// Deliberate choice: a pod-level problem (unschedulable / evicted) is
		// a property of the POD, not of each container, so we emit exactly ONE
		// synthetic entry per pod rather than one per container. Fanning it out
		// per container would produce N identical diagnoses for one incident.
		targets = []target{t}
	}

	node := c.nodeConditions(ctx, pod.Spec.NodeName)
	owner, rawOwnerKind := c.resolveOwner(ctx, pod)

	out := make([]engine.Inputs, 0, len(targets))
	for _, t := range targets {
		out = append(out, c.buildInputs(ctx, pod, t, events, node, owner, rawOwnerKind, now))
	}
	return out, nil
}

func (c *Collector) containerTargets(pod *corev1.Pod, filter string, now time.Time) []target {
	var out []target
	add := func(statuses []corev1.ContainerStatus, kind engine.ContainerKind) {
		for i := range statuses {
			st := &statuses[i]
			if filter != "" && st.Name != filter {
				continue
			}
			if !c.warrantsDiagnosis(st, kind, now) {
				continue
			}
			out = append(out, target{name: st.Name, kind: kind, status: st})
		}
	}
	add(pod.Status.ContainerStatuses, engine.KindApp)
	add(pod.Status.InitContainerStatuses, engine.KindInit)
	add(pod.Status.EphemeralContainerStatuses, engine.KindEphemeral)
	return out
}

func (c *Collector) warrantsDiagnosis(st *corev1.ContainerStatus, kind engine.ContainerKind, now time.Time) bool {
	if t := st.State.Terminated; t != nil && (t.ExitCode != 0 || st.RestartCount > 0) {
		return true
	}
	if t := st.LastTerminationState.Terminated; t != nil && (t.ExitCode != 0 || st.RestartCount > 0) {
		return true
	}
	if w := st.State.Waiting; w != nil && waitingReasonsWarrantingDiagnosis[w.Reason] {
		return true
	}
	if kind == engine.KindInit && st.State.Running != nil {
		if now.Sub(st.State.Running.StartedAt.Time) > c.opts.InitStuckThreshold {
			return true
		}
	}
	return false
}

func podLevelTarget(pod *corev1.Pod, filter string, events []engine.Event) (target, bool) {
	evicted := pod.Status.Reason == "Evicted"
	unschedulable := pod.Status.Phase == corev1.PodPending && hasFailedScheduling(events)
	if !evicted && !unschedulable {
		return target{}, false
	}

	name := filter
	if name == "" {
		switch {
		case len(pod.Spec.Containers) > 0:
			name = pod.Spec.Containers[0].Name
		case len(pod.Spec.InitContainers) > 0:
			name = pod.Spec.InitContainers[0].Name
		}
	}

	return target{
		name: name,
		kind: engine.KindApp,
		// A Pending pod's container never started, so there are no logs to
		// read — asking for them would be a guaranteed-404 API call.
		skipLogs: pod.Status.Phase == corev1.PodPending,
	}, true
}

func hasFailedScheduling(events []engine.Event) bool {
	for i := range events {
		if events[i].Reason == "FailedScheduling" {
			return true
		}
	}
	return false
}

func podHasContainer(pod *corev1.Pod, name string) bool {
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return true
		}
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return true
		}
	}
	for i := range pod.Spec.EphemeralContainers {
		if pod.Spec.EphemeralContainers[i].Name == name {
			return true
		}
	}
	statusLists := [][]corev1.ContainerStatus{
		pod.Status.ContainerStatuses,
		pod.Status.InitContainerStatuses,
		pod.Status.EphemeralContainerStatuses,
	}
	for _, list := range statusLists {
		for i := range list {
			if list[i].Name == name {
				return true
			}
		}
	}
	return false
}

func (c *Collector) buildInputs(
	ctx context.Context,
	pod *corev1.Pod,
	t target,
	events []engine.Event,
	node engine.NodeConditions,
	owner engine.Owner,
	rawOwnerKind string,
	now time.Time,
) engine.Inputs {
	in := engine.Inputs{
		Pod:                    pod.Name,
		Namespace:              pod.Namespace,
		Container:              t.name,
		Kind:                   t.kind,
		PodPhase:               string(pod.Status.Phase),
		PodReason:              pod.Status.Reason,
		PodMessage:             pod.Status.Message,
		QOSClass:               string(pod.Status.QOSClass),
		RestartPolicy:          string(pod.Spec.RestartPolicy),
		Deleting:               pod.DeletionTimestamp != nil,
		ActiveDeadlineExceeded: pod.Status.Reason == "DeadlineExceeded",
		Events:                 events,
		Node:                   node,
		Owner:                  owner,
		Now:                    now,
		InitStuckThreshold:     c.opts.InitStuckThreshold,
	}
	if pod.DeletionTimestamp != nil {
		in.DeletionTime = pod.DeletionTimestamp.Time
	}
	// Best effort, and deliberately cheap: properly detecting "the owning
	// Deployment is mid-rollout" would need extra GETs of the Deployment plus
	// its ReplicaSet generation bookkeeping on every crash. Instead we treat a
	// terminating pod owned by a rollout-capable controller as rolling; the
	// engine's lifecycle-noise guard already keys on Deleting, so this only
	// adds context rather than driving classification.
	switch rawOwnerKind {
	case kindReplicaSet, kindStatefulSet, kindDaemonSet:
		in.OwnerRolling = in.Deleting
	}

	if st := t.status; st != nil {
		in.Image = st.Image
		in.RestartCount = st.RestartCount
		in.LastTermination = terminationState(st.LastTerminationState.Terminated)
		in.CurrentTermination = terminationState(st.State.Terminated)
		if w := st.State.Waiting; w != nil {
			in.Waiting = engine.WaitingState{Present: true, Reason: w.Reason, Message: w.Message}
		}
		if r := st.State.Running; r != nil {
			in.Running = engine.RunningState{Present: true, StartedAt: r.StartedAt.Time}
			if d := now.Sub(r.StartedAt.Time); d > 0 {
				in.RunningDuration = d
			}
		}
	}

	if spec := findContainerSpec(pod, t.name); spec != nil {
		if in.Image == "" {
			in.Image = spec.image
		}
		in.Requests = resourceMap(spec.resources.Requests)
		in.Limits = resourceMap(spec.resources.Limits)
		in.Liveness = probeSpec(spec.liveness)
		in.Startup = probeSpec(spec.startup)
		in.Readiness = probeSpec(spec.readiness)
	}

	if t.skipLogs {
		in.LogsUnavailable = true
	} else {
		in.LogTail, in.LogsUnavailable = c.logTail(ctx, pod, t.name, in.LastTermination.Present)
	}
	return in
}

func terminationState(t *corev1.ContainerStateTerminated) engine.TerminationState {
	if t == nil {
		return engine.TerminationState{}
	}
	return engine.TerminationState{
		Present:    true,
		ExitCode:   t.ExitCode,
		Signal:     t.Signal,
		Reason:     t.Reason,
		Message:    t.Message,
		StartedAt:  t.StartedAt.Time,
		FinishedAt: t.FinishedAt.Time,
	}
}
