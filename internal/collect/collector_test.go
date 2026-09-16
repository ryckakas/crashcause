package collect

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ryckakas/crashcause/internal/engine"
)

var errInjected = errors.New("forbidden")

// denyReactor makes the matching verb/resource pair fail with errInjected, so
// tests can prove that a best-effort lookup degrades instead of failing the
// whole collection.
func denyReactor(k8stesting.Action) (bool, runtime.Object, error) {
	return true, nil, errInjected
}

func TestForPodCrashedAppContainer(t *testing.T) {
	pod := crashedPod(t)
	// Deliberately inserted newest-first so the ascending sort has work to do.
	backOff := podEvent("ev-backoff", corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container", 12, fixedNow.Add(-2*time.Minute))
	unhealthy := podEvent("ev-unhealthy", corev1.EventTypeWarning, "Unhealthy", "Liveness probe failed", 3, fixedNow.Add(-20*time.Minute))
	cs := newClient(t, pod, backOff, unhealthy, otherPodEvent(), testNode(corev1.ConditionFalse, corev1.ConditionFalse, corev1.ConditionFalse))

	c := New(cs, testOptions())
	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1", len(got))
	}
	in := got[0]

	t.Run("identity", func(t *testing.T) {
		if in.Pod != testPodName || in.Namespace != testNamespace || in.Container != testContainer {
			t.Errorf("identity = %s/%s/%s", in.Namespace, in.Pod, in.Container)
		}
		if in.Kind != engine.KindApp {
			t.Errorf("Kind = %q, want %q", in.Kind, engine.KindApp)
		}
		if in.Image != testImage {
			t.Errorf("Image = %q, want %q", in.Image, testImage)
		}
		if in.RestartCount != 5 {
			t.Errorf("RestartCount = %d, want 5", in.RestartCount)
		}
		if in.QOSClass != string(corev1.PodQOSBurstable) {
			t.Errorf("QOSClass = %q", in.QOSClass)
		}
		if in.RestartPolicy != string(corev1.RestartPolicyAlways) {
			t.Errorf("RestartPolicy = %q", in.RestartPolicy)
		}
		if in.PodPhase != string(corev1.PodRunning) {
			t.Errorf("PodPhase = %q", in.PodPhase)
		}
		if !in.Now.Equal(fixedNow) {
			t.Errorf("Now = %v, want %v", in.Now, fixedNow)
		}
		if in.InitStuckThreshold != defaultInitStuckThreshold {
			t.Errorf("InitStuckThreshold = %v", in.InitStuckThreshold)
		}
		if in.Deleting || in.OwnerRolling || in.ActiveDeadlineExceeded {
			t.Errorf("unexpected lifecycle flags: %+v", in)
		}
	})

	t.Run("last termination", func(t *testing.T) {
		lt := in.LastTermination
		if !lt.Present {
			t.Fatal("LastTermination.Present = false")
		}
		if lt.ExitCode != 137 || lt.Signal != 9 {
			t.Errorf("exit/signal = %d/%d, want 137/9", lt.ExitCode, lt.Signal)
		}
		if lt.Reason != "OOMKilled" || lt.Message != "container was killed" {
			t.Errorf("reason/message = %q/%q", lt.Reason, lt.Message)
		}
		if want := fixedNow.Add(-30 * time.Minute); !lt.StartedAt.Equal(want) {
			t.Errorf("StartedAt = %v, want %v", lt.StartedAt, want)
		}
		if want := fixedNow.Add(-5 * time.Minute); !lt.FinishedAt.Equal(want) {
			t.Errorf("FinishedAt = %v, want %v", lt.FinishedAt, want)
		}
		if in.CurrentTermination.Present {
			t.Error("CurrentTermination.Present = true, want false")
		}
		if !in.Waiting.Present || in.Waiting.Reason != "CrashLoopBackOff" {
			t.Errorf("Waiting = %+v", in.Waiting)
		}
		if in.Running.Present || in.RunningDuration != 0 {
			t.Errorf("Running = %+v, duration %v", in.Running, in.RunningDuration)
		}
	})

	t.Run("resources and probes", func(t *testing.T) {
		if got, want := in.Requests["memory"], "128Mi"; got != want {
			t.Errorf("Requests[memory] = %q, want %q", got, want)
		}
		if got, want := in.Requests["cpu"], "100m"; got != want {
			t.Errorf("Requests[cpu] = %q, want %q", got, want)
		}
		if got, want := in.Limits["memory"], "256Mi"; got != want {
			t.Errorf("Limits[memory] = %q, want %q", got, want)
		}
		want := engine.ProbeSpec{
			Defined:             true,
			FailureThreshold:    7,
			PeriodSeconds:       10,
			InitialDelaySeconds: 15,
			TimeoutSeconds:      1,
		}
		if in.Liveness != want {
			t.Errorf("Liveness = %+v, want %+v", in.Liveness, want)
		}
		if in.Readiness.Defined || in.Startup.Defined {
			t.Errorf("undefined probes reported as defined: %+v %+v", in.Readiness, in.Startup)
		}
	})

	t.Run("events sorted ascending and filtered", func(t *testing.T) {
		if len(in.Events) != 2 {
			t.Fatalf("got %d events, want 2: %+v", len(in.Events), in.Events)
		}
		if in.Events[0].Reason != "Unhealthy" || in.Events[1].Reason != "BackOff" {
			t.Errorf("events not sorted ascending by LastSeen: %+v", in.Events)
		}
		if in.Events[0].LastSeen.After(in.Events[1].LastSeen) {
			t.Errorf("LastSeen out of order: %v then %v", in.Events[0].LastSeen, in.Events[1].LastSeen)
		}
		if in.Events[1].Count != 12 || in.Events[1].Type != corev1.EventTypeWarning {
			t.Errorf("event fields lost: %+v", in.Events[1])
		}
		if in.Events[0].FirstSeen.IsZero() {
			t.Error("FirstSeen not populated")
		}
	})

	t.Run("previous log fetch", func(t *testing.T) {
		if in.LogsUnavailable {
			t.Error("LogsUnavailable = true, want false")
		}
		// Content is the fake's constant body, so only plumbing is asserted.
		if len(in.LogTail) == 0 {
			t.Error("LogTail is empty, want the fake's body")
		}
		reqs := logActions(t, cs)
		if len(reqs) != 1 {
			t.Fatalf("got %d log actions, want 1", len(reqs))
		}
		if !reqs[0].Previous {
			t.Error("Previous = false, want true (lastState.terminated present)")
		}
		if reqs[0].TailLines == nil || *reqs[0].TailLines != defaultPreviousLogLines {
			t.Errorf("TailLines = %v, want %d", reqs[0].TailLines, defaultPreviousLogLines)
		}
		if reqs[0].Container != testContainer {
			t.Errorf("Container = %q, want %q", reqs[0].Container, testContainer)
		}
	})
}

func TestLogRateLimiterSkipsFetches(t *testing.T) {
	cs := newClient(t, crashedPod(t), otherPodEvent())
	opts := testOptions()
	// Burst 0 denies every reservation.
	opts.LogRateLimiter = rate.NewLimiter(rate.Limit(0), 0)
	c := New(cs, opts)

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1 (diagnosis must survive the rate limiter)", len(got))
	}
	if !got[0].LogsUnavailable {
		t.Error("LogsUnavailable = false, want true")
	}
	if len(got[0].LogTail) != 0 {
		t.Errorf("LogTail = %v, want empty", got[0].LogTail)
	}
	if n := countActions(t, cs, "get", "pods", "log"); n != 0 {
		t.Errorf("got %d log actions, want 0", n)
	}
	if n := c.LogFetchesSkipped(); n != 1 {
		t.Errorf("LogFetchesSkipped() = %d, want 1", n)
	}

	if _, err := c.ForPod(context.Background(), testNamespace, testPodName, ""); err != nil {
		t.Fatalf("second ForPod: %v", err)
	}
	if n := c.LogFetchesSkipped(); n != 2 {
		t.Errorf("LogFetchesSkipped() = %d after two calls, want 2 (counter must be monotonic)", n)
	}
}

func TestCollectLogsDisabled(t *testing.T) {
	cs := newClient(t, crashedPod(t), otherPodEvent())
	opts := testOptions()
	opts.CollectLogs = false
	c := New(cs, opts)

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1", len(got))
	}
	if !got[0].LogsUnavailable {
		t.Error("LogsUnavailable = false, want true")
	}
	if len(got[0].LogTail) != 0 {
		t.Errorf("LogTail = %v, want empty", got[0].LogTail)
	}
	for _, a := range cs.Actions() {
		if a.GetSubresource() == "log" {
			t.Fatalf("log action recorded with CollectLogs=false: %+v", a)
		}
	}
	if n := c.LogFetchesSkipped(); n != 0 {
		t.Errorf("LogFetchesSkipped() = %d, want 0 (disabled is not rate limited)", n)
	}
}

func TestPendingUnschedulablePod(t *testing.T) {
	pod := pendingPod(t)
	ev := podEvent("ev-sched", corev1.EventTypeWarning, "FailedScheduling", "0/3 nodes are available: insufficient memory", 4, fixedNow.Add(-time.Minute))
	cs := newClient(t, pod, ev, otherPodEvent())
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want exactly 1 pod-level entry", len(got))
	}
	in := got[0]
	if in.PodPhase != string(corev1.PodPending) {
		t.Errorf("PodPhase = %q, want Pending", in.PodPhase)
	}
	if in.Container != testContainer {
		t.Errorf("Container = %q, want the first spec container %q", in.Container, testContainer)
	}
	if in.Kind != engine.KindApp {
		t.Errorf("Kind = %q, want app", in.Kind)
	}
	if in.Image != testImage {
		t.Errorf("Image = %q, want the spec image %q", in.Image, testImage)
	}
	var found bool
	for _, e := range in.Events {
		if e.Reason == "FailedScheduling" {
			found = true
		}
		if e.Message == "belongs to another pod" {
			t.Error("event for a different pod leaked through the client-side filter")
		}
	}
	if !found {
		t.Errorf("FailedScheduling event missing: %+v", in.Events)
	}
	if !in.LogsUnavailable {
		t.Error("LogsUnavailable = false, want true for a pod that never started")
	}
	if n := countActions(t, cs, "get", "pods", "log"); n != 0 {
		t.Errorf("got %d log actions, want 0 for an unschedulable pod", n)
	}
}

func TestOwnerResolution(t *testing.T) {
	replicaSet := func(ownedByDeployment bool) *appsv1.ReplicaSet {
		rs := &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "web-6d4f", Namespace: testNamespace, UID: "rs-uid"},
		}
		if ownedByDeployment {
			rs.OwnerReferences = []metav1.OwnerReference{controllerRef(kindDeployment, "web", "dep-uid")}
		}
		return rs
	}

	t.Run("replicaset resolves to deployment", func(t *testing.T) {
		pod := crashedPod(t)
		pod.OwnerReferences = []metav1.OwnerReference{controllerRef(kindReplicaSet, "web-6d4f", "rs-uid")}
		cs := newClient(t, pod, replicaSet(true))
		c := New(cs, testOptions())

		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		want := engine.Owner{Kind: kindDeployment, Name: "web"}
		if got[0].Owner != want {
			t.Errorf("Owner = %+v, want %+v", got[0].Owner, want)
		}
	})

	t.Run("replicaset get failure falls back", func(t *testing.T) {
		pod := crashedPod(t)
		pod.OwnerReferences = []metav1.OwnerReference{controllerRef(kindReplicaSet, "web-6d4f", "rs-uid")}
		cs := newClient(t, pod, replicaSet(true))
		cs.PrependReactor("get", "replicasets", denyReactor)
		c := New(cs, testOptions())

		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod must not fail when owner resolution fails: %v", err)
		}
		want := engine.Owner{Kind: kindReplicaSet, Name: "web-6d4f"}
		if got[0].Owner != want {
			t.Errorf("Owner = %+v, want %+v", got[0].Owner, want)
		}
	})

	t.Run("job owned by cronjob", func(t *testing.T) {
		pod := crashedPod(t)
		pod.OwnerReferences = []metav1.OwnerReference{controllerRef(kindJob, "nightly-28900", "job-uid")}
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "nightly-28900",
				Namespace:       testNamespace,
				UID:             "job-uid",
				OwnerReferences: []metav1.OwnerReference{controllerRef(kindCronJob, "nightly", "cj-uid")},
			},
		}
		cs := newClient(t, pod, job)
		c := New(cs, testOptions())

		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		want := engine.Owner{Kind: kindJob, Name: "nightly-28900", CronJobName: "nightly"}
		if got[0].Owner != want {
			t.Errorf("Owner = %+v, want %+v", got[0].Owner, want)
		}
	})

	t.Run("no owner references", func(t *testing.T) {
		cs := newClient(t, crashedPod(t))
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if (got[0].Owner != engine.Owner{}) {
			t.Errorf("Owner = %+v, want zero", got[0].Owner)
		}
	})

	t.Run("resolution is cached across calls", func(t *testing.T) {
		pod := crashedPod(t)
		pod.OwnerReferences = []metav1.OwnerReference{controllerRef(kindReplicaSet, "web-6d4f", "rs-uid")}
		cs := newClient(t, pod, replicaSet(true))
		c := New(cs, testOptions())

		for i := 0; i < 2; i++ {
			if _, err := c.ForPod(context.Background(), testNamespace, testPodName, ""); err != nil {
				t.Fatalf("ForPod #%d: %v", i, err)
			}
		}
		if n := countActions(t, cs, "get", "replicasets", ""); n != 1 {
			t.Errorf("got %d replicasets GETs across two calls, want 1", n)
		}
	})
}

func TestNodeConditions(t *testing.T) {
	t.Run("pressure mapped", func(t *testing.T) {
		cs := newClient(t, crashedPod(t), testNode(corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionFalse))
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		want := engine.NodeConditions{Known: true, MemoryPressure: true}
		if got[0].Node != want {
			t.Errorf("Node = %+v, want %+v", got[0].Node, want)
		}
	})

	t.Run("node get failure degrades", func(t *testing.T) {
		cs := newClient(t, crashedPod(t), testNode(corev1.ConditionTrue, corev1.ConditionTrue, corev1.ConditionTrue))
		cs.PrependReactor("get", "nodes", denyReactor)
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("node failure must not fail collection: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d inputs, want 1", len(got))
		}
		if got[0].Node.Known {
			t.Errorf("Node.Known = true, want false: %+v", got[0].Node)
		}
	})

	t.Run("collection disabled", func(t *testing.T) {
		cs := newClient(t, crashedPod(t), testNode(corev1.ConditionTrue, corev1.ConditionFalse, corev1.ConditionFalse))
		opts := testOptions()
		opts.CollectNode = false
		c := New(cs, opts)
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if got[0].Node.Known {
			t.Error("Node.Known = true with CollectNode=false")
		}
		if n := countActions(t, cs, "get", "nodes", ""); n != 0 {
			t.Errorf("got %d nodes GETs, want 0", n)
		}
	})
}

func TestContainerFilter(t *testing.T) {
	multiPod := func(t *testing.T) *corev1.Pod {
		t.Helper()
		pod := crashedPod(t)
		pod.Spec.Containers = append(pod.Spec.Containers, appContainerSpec("sidecar"))
		pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, crashedStatus("sidecar"))
		return pod
	}

	t.Run("no filter returns both containers", func(t *testing.T) {
		cs := newClient(t, multiPod(t))
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d inputs, want 2", len(got))
		}
	})

	t.Run("filter narrows to one container", func(t *testing.T) {
		cs := newClient(t, multiPod(t))
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "sidecar")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d inputs, want 1", len(got))
		}
		if got[0].Container != "sidecar" {
			t.Errorf("Container = %q, want sidecar", got[0].Container)
		}
	})

	t.Run("known but healthy container yields nothing", func(t *testing.T) {
		pod := multiPod(t)
		// "sidecar" exists in the spec but has no status and no problem.
		pod.Status.ContainerStatuses = pod.Status.ContainerStatuses[:1]
		cs := newClient(t, pod)
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "sidecar")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d inputs, want 0: %+v", len(got), got)
		}
	})

	t.Run("filter applies to the pod-level entry", func(t *testing.T) {
		pod := pendingPod(t)
		pod.Spec.Containers = append(pod.Spec.Containers, appContainerSpec("sidecar"))
		ev := podEvent("ev-sched", corev1.EventTypeWarning, "FailedScheduling", "no nodes", 1, fixedNow)
		cs := newClient(t, pod, ev)
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "sidecar")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d inputs, want 1", len(got))
		}
		if got[0].Container != "sidecar" {
			t.Errorf("Container = %q, want the filtered name", got[0].Container)
		}
	})

	t.Run("unknown container is an error", func(t *testing.T) {
		cs := newClient(t, multiPod(t))
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "nope")
		if !errors.Is(err, ErrContainerNotFound) {
			t.Fatalf("err = %v, want ErrContainerNotFound", err)
		}
		if got != nil {
			t.Errorf("inputs = %+v, want nil", got)
		}
	})
}

func TestHealthyPodProducesNothing(t *testing.T) {
	cs := newClient(t, healthyPod(t), otherPodEvent(), testNode(corev1.ConditionTrue, corev1.ConditionTrue, corev1.ConditionTrue))
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d inputs, want 0: %+v", len(got), got)
	}
	actions := cs.Actions()
	if len(actions) != 1 {
		t.Fatalf("got %d actions, want exactly the pod GET: %+v", len(actions), actions)
	}
	a := actions[0]
	if a.GetVerb() != "get" || a.GetResource().Resource != "pods" || a.GetSubresource() != "" {
		t.Errorf("unexpected action %+v", a)
	}
}

func TestInitContainerStuck(t *testing.T) {
	initPod := func(t *testing.T, runningFor time.Duration) *corev1.Pod {
		t.Helper()
		pod := basePod(t)
		pod.Status.Phase = corev1.PodPending
		pod.Spec.InitContainers = []corev1.Container{appContainerSpec("wait-for-db")}
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
			Name:  "wait-for-db",
			Image: testImage,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(fixedNow.Add(-runningFor)),
				},
			},
		}}
		return pod
	}

	t.Run("running past the threshold", func(t *testing.T) {
		cs := newClient(t, initPod(t, 30*time.Minute))
		opts := testOptions()
		opts.InitStuckThreshold = 10 * time.Minute
		c := New(cs, opts)

		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d inputs, want 1", len(got))
		}
		in := got[0]
		if in.Kind != engine.KindInit {
			t.Errorf("Kind = %q, want init", in.Kind)
		}
		if in.Container != "wait-for-db" {
			t.Errorf("Container = %q", in.Container)
		}
		if !in.Running.Present {
			t.Error("Running.Present = false")
		}
		if in.RunningDuration != 30*time.Minute {
			t.Errorf("RunningDuration = %v, want 30m", in.RunningDuration)
		}
		if in.InitStuckThreshold != 10*time.Minute {
			t.Errorf("InitStuckThreshold = %v", in.InitStuckThreshold)
		}
		// A stuck-but-running init container has no previous incarnation.
		reqs := logActions(t, cs)
		if len(reqs) != 1 {
			t.Fatalf("got %d log actions, want 1", len(reqs))
		}
		if reqs[0].Previous {
			t.Error("Previous = true, want false for a running init container")
		}
	})

	t.Run("running under the threshold", func(t *testing.T) {
		cs := newClient(t, initPod(t, time.Minute))
		opts := testOptions()
		opts.InitStuckThreshold = 10 * time.Minute
		c := New(cs, opts)

		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got %d inputs, want 0: %+v", len(got), got)
		}
	})
}

func TestWaitingWithNoPreviousReadsCurrentLog(t *testing.T) {
	pod := basePod(t)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  testContainer,
		Image: testImage,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "CreateContainerConfigError",
				Message: `secret "db" not found`,
			},
		},
	}}
	cs := newClient(t, pod)
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1", len(got))
	}
	if got[0].LastTermination.Present {
		t.Error("LastTermination.Present = true, want false")
	}
	if !got[0].Waiting.Present || got[0].Waiting.Reason != "CreateContainerConfigError" {
		t.Errorf("Waiting = %+v", got[0].Waiting)
	}
	reqs := logActions(t, cs)
	if len(reqs) != 1 {
		t.Fatalf("got %d log actions, want 1", len(reqs))
	}
	if reqs[0].Previous {
		t.Error("Previous = true, want false when there is no lastState.terminated")
	}
}

func TestEvictedPod(t *testing.T) {
	pod := basePod(t)
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	pod.Status.Message = "The node was low on resource: memory."
	cs := newClient(t, pod, otherPodEvent())
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want exactly 1 pod-level entry", len(got))
	}
	in := got[0]
	if in.PodReason != "Evicted" {
		t.Errorf("PodReason = %q, want Evicted", in.PodReason)
	}
	if in.PodPhase != string(corev1.PodFailed) {
		t.Errorf("PodPhase = %q, want Failed", in.PodPhase)
	}
	if in.PodMessage == "" {
		t.Error("PodMessage not propagated")
	}
	if in.Container != testContainer || in.Kind != engine.KindApp {
		t.Errorf("synthetic entry = %q/%q", in.Container, in.Kind)
	}
	// Unlike the Pending case, an evicted pod's container did run, so a
	// current-log read is attempted.
	reqs := logActions(t, cs)
	if len(reqs) != 1 {
		t.Fatalf("got %d log actions, want 1", len(reqs))
	}
	if reqs[0].Previous {
		t.Error("Previous = true, want false for the evicted synthetic entry")
	}
}

func TestDeletingPod(t *testing.T) {
	deletedAt := fixedNow.Add(-time.Minute)

	t.Run("owned by a replicaset is rolling", func(t *testing.T) {
		pod := crashedPod(t)
		ts := metav1.NewTime(deletedAt)
		pod.DeletionTimestamp = &ts
		pod.OwnerReferences = []metav1.OwnerReference{controllerRef(kindReplicaSet, "web-6d4f", "rs-uid")}
		cs := newClient(t, pod)
		c := New(cs, testOptions())

		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d inputs, want 1", len(got))
		}
		in := got[0]
		if !in.Deleting {
			t.Error("Deleting = false, want true")
		}
		if !in.DeletionTime.Equal(deletedAt) {
			t.Errorf("DeletionTime = %v, want %v", in.DeletionTime, deletedAt)
		}
		if !in.OwnerRolling {
			t.Error("OwnerRolling = false, want true")
		}
	})

	t.Run("without an owner is not rolling", func(t *testing.T) {
		pod := crashedPod(t)
		ts := metav1.NewTime(deletedAt)
		pod.DeletionTimestamp = &ts
		cs := newClient(t, pod)
		c := New(cs, testOptions())

		got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
		if err != nil {
			t.Fatalf("ForPod: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d inputs, want 1", len(got))
		}
		if !got[0].Deleting {
			t.Error("Deleting = false, want true")
		}
		if got[0].OwnerRolling {
			t.Error("OwnerRolling = true, want false without an owner")
		}
	})
}

func TestForPodObjectSkipsPodGet(t *testing.T) {
	pod := crashedPod(t)
	ev := podEvent("ev-backoff", corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container", 2, fixedNow)
	// The pod itself is deliberately NOT seeded: an informer already has it.
	cs := newClient(t, ev, otherPodEvent())
	c := New(cs, testOptions())

	got, err := c.ForPodObject(context.Background(), pod, "")
	if err != nil {
		t.Fatalf("ForPodObject: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1", len(got))
	}
	if n := countActions(t, cs, "get", "pods", ""); n != 0 {
		t.Errorf("got %d pod GETs, want 0", n)
	}
	if len(got[0].Events) != 1 || got[0].Events[0].Reason != "BackOff" {
		t.Errorf("events not collected for an informer-supplied pod: %+v", got[0].Events)
	}
}

func TestForPodErrors(t *testing.T) {
	t.Run("missing pod", func(t *testing.T) {
		cs := newClient(t)
		c := New(cs, testOptions())
		got, err := c.ForPod(context.Background(), testNamespace, "ghost", "")
		if err == nil {
			t.Fatal("err = nil, want a pod GET failure")
		}
		if got != nil {
			t.Errorf("inputs = %+v, want nil", got)
		}
	})

	t.Run("pod get denied", func(t *testing.T) {
		cs := newClient(t, crashedPod(t))
		cs.PrependReactor("get", "pods", denyReactor)
		c := New(cs, testOptions())
		if _, err := c.ForPod(context.Background(), testNamespace, testPodName, ""); !errors.Is(err, errInjected) {
			t.Fatalf("err = %v, want the injected error wrapped with %%w", err)
		}
	})

	t.Run("nil pod object", func(t *testing.T) {
		c := New(newClient(t), testOptions())
		got, err := c.ForPodObject(context.Background(), nil, "")
		if err == nil {
			t.Fatal("err = nil, want an error for a nil pod")
		}
		if got != nil {
			t.Errorf("inputs = %+v, want nil", got)
		}
	})
}

func TestEventListFailureDegrades(t *testing.T) {
	cs := newClient(t, crashedPod(t))
	cs.PrependReactor("list", "events", denyReactor)
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("event failure must not fail collection: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1", len(got))
	}
	if len(got[0].Events) != 0 {
		t.Errorf("Events = %+v, want none", got[0].Events)
	}
	// The collector retries once without the field selector before giving up.
	if n := countActions(t, cs, "list", "events", ""); n != 2 {
		t.Errorf("got %d event LISTs, want 2 (selector + unfiltered retry)", n)
	}
}

func TestEventTimeFallback(t *testing.T) {
	pod := crashedPod(t)
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "ev-modern", Namespace: testNamespace},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Name: testPodName, Namespace: testNamespace, UID: testPodUID,
		},
		Type:      corev1.EventTypeWarning,
		Reason:    "Killing",
		EventTime: metav1.NewMicroTime(fixedNow.Add(-time.Minute)),
	}
	cs := newClient(t, pod, ev)
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got[0].Events) != 1 {
		t.Fatalf("got %d events, want 1", len(got[0].Events))
	}
	want := fixedNow.Add(-time.Minute)
	if !got[0].Events[0].LastSeen.Equal(want) {
		t.Errorf("LastSeen = %v, want the EventTime fallback %v", got[0].Events[0].LastSeen, want)
	}
	if !got[0].Events[0].FirstSeen.Equal(want) {
		t.Errorf("FirstSeen = %v, want the EventTime fallback %v", got[0].Events[0].FirstSeen, want)
	}
}

func TestEventContainerAttribution(t *testing.T) {
	pod := crashedPod(t)
	app := podEvent("ev-app", corev1.EventTypeWarning, "Unhealthy", "Liveness probe failed", 3, fixedNow.Add(-4*time.Minute))
	app.InvolvedObject.FieldPath = "spec.containers{app}"
	initC := podEvent("ev-init", corev1.EventTypeWarning, "BackOff", "Back-off restarting failed container", 2, fixedNow.Add(-3*time.Minute))
	initC.InvolvedObject.FieldPath = "spec.initContainers{init-db}"
	podScoped := podEvent("ev-pod", corev1.EventTypeWarning, "FailedScheduling", "0/3 nodes are available", 1, fixedNow.Add(-2*time.Minute))
	odd := podEvent("ev-odd", corev1.EventTypeWarning, "FailedMount", "MountVolume.SetUp failed", 1, fixedNow.Add(-1*time.Minute))
	odd.InvolvedObject.FieldPath = "spec.volumes{data}"
	cs := newClient(t, pod, app, initC, podScoped, odd)
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got[0].Events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got[0].Events), got[0].Events)
	}
	want := map[string]string{
		"Unhealthy":        "app",
		"BackOff":          "init-db",
		"FailedScheduling": "",
		"FailedMount":      "",
	}
	for _, ev := range got[0].Events {
		if ev.Container != want[ev.Reason] {
			t.Errorf("event %s: Container = %q, want %q", ev.Reason, ev.Container, want[ev.Reason])
		}
	}
}

func TestDefaultOptions(t *testing.T) {
	opts := DefaultOptions()
	if opts.PreviousLogLines != defaultPreviousLogLines {
		t.Errorf("PreviousLogLines = %d, want %d", opts.PreviousLogLines, defaultPreviousLogLines)
	}
	if !opts.CollectLogs || !opts.CollectNode {
		t.Errorf("CollectLogs/CollectNode = %v/%v, want true/true", opts.CollectLogs, opts.CollectNode)
	}
	if opts.InitStuckThreshold != defaultInitStuckThreshold {
		t.Errorf("InitStuckThreshold = %v, want %v", opts.InitStuckThreshold, defaultInitStuckThreshold)
	}
	if opts.LogRateLimiter != nil {
		t.Error("LogRateLimiter != nil, want nil")
	}
	if opts.Now == nil {
		t.Error("Now = nil, want the real clock")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	c := New(newClient(t), Options{})
	if c.opts.PreviousLogLines != defaultPreviousLogLines {
		t.Errorf("PreviousLogLines = %d, want %d", c.opts.PreviousLogLines, defaultPreviousLogLines)
	}
	if c.opts.InitStuckThreshold != defaultInitStuckThreshold {
		t.Errorf("InitStuckThreshold = %v, want %v", c.opts.InitStuckThreshold, defaultInitStuckThreshold)
	}
	if c.opts.Now == nil {
		t.Fatal("Now = nil, want time.Now")
	}
	if c.now().IsZero() {
		t.Error("now() returned the zero time")
	}
	if c.LogFetchesSkipped() != 0 {
		t.Error("a fresh collector reports skipped fetches")
	}
}

func TestSplitLogLines(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"single line no newline", "boom", []string{"boom"}},
		{"trailing newline dropped", "a\nb\n", []string{"a", "b"}},
		{"crlf stripped", "a\r\nb\r\n", []string{"a", "b"}},
		{"interior blank kept", "a\n\nb\n", []string{"a", "", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitLogLines(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}

func TestTailLinesOptionIsHonoured(t *testing.T) {
	cs := newClient(t, crashedPod(t))
	opts := testOptions()
	opts.PreviousLogLines = 5
	c := New(cs, opts)

	if _, err := c.ForPod(context.Background(), testNamespace, testPodName, ""); err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	reqs := logActions(t, cs)
	if len(reqs) != 1 {
		t.Fatalf("got %d log actions, want 1", len(reqs))
	}
	if reqs[0].TailLines == nil || *reqs[0].TailLines != 5 {
		t.Errorf("TailLines = %v, want 5", reqs[0].TailLines)
	}
}

func TestEphemeralContainer(t *testing.T) {
	pod := basePod(t)
	pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Name:  "debugger",
			Image: "registry.example.com/debug:1",
		},
	}}
	pod.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{crashedStatus("debugger")}
	cs := newClient(t, pod)
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1", len(got))
	}
	if got[0].Kind != engine.KindEphemeral {
		t.Errorf("Kind = %q, want ephemeral", got[0].Kind)
	}
	if got[0].Requests != nil || got[0].Limits != nil {
		t.Errorf("resources = %v/%v, want nil when the container declares none", got[0].Requests, got[0].Limits)
	}
	if got[0].Liveness.Defined {
		t.Error("Liveness.Defined = true, want false")
	}
}

func TestCurrentlyTerminatedReadsCurrentLog(t *testing.T) {
	pod := basePod(t)
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "DeadlineExceeded"
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  testContainer,
		Image: testImage,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   137,
				Reason:     "Error",
				StartedAt:  metav1.NewTime(fixedNow.Add(-time.Hour)),
				FinishedAt: metav1.NewTime(fixedNow.Add(-time.Minute)),
			},
		},
	}}
	cs := newClient(t, pod)
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d inputs, want 1", len(got))
	}
	in := got[0]
	if !in.CurrentTermination.Present || in.CurrentTermination.ExitCode != 137 {
		t.Errorf("CurrentTermination = %+v", in.CurrentTermination)
	}
	if in.LastTermination.Present {
		t.Error("LastTermination.Present = true, want false")
	}
	if !in.ActiveDeadlineExceeded {
		t.Error("ActiveDeadlineExceeded = false, want true")
	}
	if in.RestartPolicy != string(corev1.RestartPolicyNever) {
		t.Errorf("RestartPolicy = %q", in.RestartPolicy)
	}
	reqs := logActions(t, cs)
	if len(reqs) != 1 {
		t.Fatalf("got %d log actions, want 1", len(reqs))
	}
	if reqs[0].Previous {
		t.Error("Previous = true, want false when there is no previous incarnation")
	}
}

func TestOwnerRefWithoutControllerFlag(t *testing.T) {
	pod := crashedPod(t)
	pod.OwnerReferences = []metav1.OwnerReference{{
		Kind: kindStatefulSet,
		Name: "db",
		UID:  "sts-uid",
	}}
	ts := metav1.NewTime(fixedNow)
	pod.DeletionTimestamp = &ts
	cs := newClient(t, pod)
	c := New(cs, testOptions())

	got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
	if err != nil {
		t.Fatalf("ForPod: %v", err)
	}
	want := engine.Owner{Kind: kindStatefulSet, Name: "db"}
	if got[0].Owner != want {
		t.Errorf("Owner = %+v, want %+v (first ref used when none is the controller)", got[0].Owner, want)
	}
	if !got[0].OwnerRolling {
		t.Error("OwnerRolling = false, want true for a deleting StatefulSet pod")
	}
}

func TestCollectorConcurrentUse(t *testing.T) {
	pod := crashedPod(t)
	pod.OwnerReferences = []metav1.OwnerReference{controllerRef(kindReplicaSet, "web-6d4f", "rs-uid")}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-6d4f", Namespace: testNamespace, UID: "rs-uid",
			OwnerReferences: []metav1.OwnerReference{controllerRef(kindDeployment, "web", "dep-uid")},
		},
	}
	cs := newClient(t, pod, rs)
	opts := testOptions()
	opts.LogRateLimiter = rate.NewLimiter(rate.Limit(0), 0)
	c := New(cs, opts)

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.ForPod(context.Background(), testNamespace, testPodName, "")
			if err != nil {
				errs <- err
				return
			}
			if len(got) != 1 {
				errs <- errors.New("unexpected input count")
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent ForPod: %v", err)
	}
	if n := c.LogFetchesSkipped(); n != workers {
		t.Errorf("LogFetchesSkipped() = %d, want %d", n, workers)
	}
}
