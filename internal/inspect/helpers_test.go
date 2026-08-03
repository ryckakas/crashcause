package inspect

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ryckakas/crashcause/internal/ai"
)

// Fixtures in this file mirror internal/collect/helpers_test.go's style, with
// one deliberate difference: inspect.Run always uses the real clock (there is
// no clock injection through inspect.Options), so every fixture that needs a
// timestamp takes an explicit `now time.Time` built from time.Now() by the
// calling test, rather than a package-level frozen clock.

const (
	testNamespace  = "prod"
	testPodName    = "web-6d4f-abcde"
	testContainer  = "app"
	testContainer2 = "sidecar"
	testNodeName   = "node-1"
	testPodUID     = "pod-uid-1"
	testImage      = "registry.example.com/app:1.2.3"
)

// newClient builds a fake clientset seeded with the given objects.
func newClient(t *testing.T, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	return fake.NewClientset(objs...)
}

// appContainerSpec is a container spec with resources and a liveness probe
// set, matching the fixtures below.
func appContainerSpec(name string) corev1.Container {
	return corev1.Container{
		Name:  name,
		Image: testImage,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("128Mi"),
				corev1.ResourceCPU:    resource.MustParse("100m"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		LivenessProbe: &corev1.Probe{FailureThreshold: 7, InitialDelaySeconds: 15},
	}
}

// basePod is a minimal scheduled pod with a single app container and no
// container statuses.
func basePod(t *testing.T) *corev1.Pod {
	t.Helper()
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      testPodName,
			Namespace: testNamespace,
			UID:       testPodUID,
		},
		Spec: corev1.PodSpec{
			NodeName:      testNodeName,
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers:    []corev1.Container{appContainerSpec(testContainer)},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
		},
	}
}

// oomKilledStatus is a container status for a container that OOMed and is
// now backing off: lastState.terminated exit 137 / OOMKilled, restartCount 5.
// This trips engine.CauseOOMKilled.
func oomKilledStatus(name string, now time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         name,
		Image:        testImage,
		RestartCount: 5,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "CrashLoopBackOff",
				Message: "back-off 5m0s restarting failed container",
			},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   137,
				Signal:     9,
				Reason:     "OOMKilled",
				Message:    "container was killed",
				StartedAt:  metav1.NewTime(now.Add(-30 * time.Minute)),
				FinishedAt: metav1.NewTime(now.Add(-5 * time.Minute)),
			},
		},
	}
}

// crashedStatus is an alias kept for readability at call sites that just want
// "the canonical crashing container status" without naming the cause.
func crashedStatus(name string, now time.Time) corev1.ContainerStatus {
	return oomKilledStatus(name, now)
}

// oomKilledPod is the canonical OOM fixture: one app container in
// CrashLoopBackOff after an OOMKill.
func oomKilledPod(t *testing.T, now time.Time) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{oomKilledStatus(testContainer, now)}
	return pod
}

// healthyPod is a running pod that warrants no diagnosis at all.
func healthyPod(t *testing.T, now time.Time) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  testContainer,
		Image: testImage,
		Ready: true,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(now.Add(-2 * time.Hour)),
			},
		},
	}}
	return pod
}

// appExitStatus is a container status that trips ONLY engine.CauseAppExitNonzero:
// a plain exit code 1 with reason "Error", no OOMKilled reason, no signal, no
// probe/image/eviction/config/volume signal of any kind — the AI gate (spec
// §6) only opens for app_exit_nonzero and unknown.
func appExitStatus(name string, now time.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:         name,
		Image:        testImage,
		RestartCount: 2,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{
				Reason:  "CrashLoopBackOff",
				Message: "back-off 5m0s restarting failed container",
			},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   1,
				Reason:     "Error",
				Message:    "application error",
				StartedAt:  metav1.NewTime(now.Add(-10 * time.Minute)),
				FinishedAt: metav1.NewTime(now.Add(-5 * time.Minute)),
			},
		},
	}
}

// appExitPod is the canonical app_exit_nonzero fixture: single app container,
// plain exit 1, no Kubernetes-side cause.
func appExitPod(t *testing.T, now time.Time) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{appExitStatus(testContainer, now)}
	return pod
}

// probeFailurePod is currently running, but warrants diagnosis (per
// collect.warrantsDiagnosis) because of a harmless exit-0 LAST termination
// with a nonzero restart count — a plain "currently Running, RestartCount>0"
// status is invisible to the collector, since it only looks at
// State.Terminated / LastTerminationState.Terminated / Waiting. RestartPolicy
// is deliberately NOT "Always", so the exit-0 last termination does not also
// trip completedRestartLoopRule (which requires RestartPolicy=Always). See
// TestRunVerboseSecondaryMatches for exactly which two rules this trips and
// why.
func probeFailurePod(t *testing.T, now time.Time) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         testContainer,
		Image:        testImage,
		RestartCount: 4,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(now.Add(-2 * time.Minute)),
			},
		},
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:   0,
				Reason:     "Completed",
				StartedAt:  metav1.NewTime(now.Add(-10 * time.Minute)),
				FinishedAt: metav1.NewTime(now.Add(-3 * time.Minute)),
			},
		},
	}}
	return pod
}

// podEvent builds an event for the canonical pod.
func podEvent(name, evType, reason, message string, count int32, last time.Time) *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      testPodName,
			Namespace: testNamespace,
			UID:       testPodUID,
		},
		Type:           evType,
		Reason:         reason,
		Message:        message,
		Count:          count,
		FirstTimestamp: metav1.NewTime(last.Add(-10 * time.Minute)),
		LastTimestamp:  metav1.NewTime(last),
	}
}

// multiContainerCrashedPod has two app containers, both OOM-killed.
func multiContainerCrashedPod(t *testing.T, now time.Time) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Spec.Containers = append(pod.Spec.Containers, appContainerSpec(testContainer2))
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{
		oomKilledStatus(testContainer, now),
		oomKilledStatus(testContainer2, now),
	}
	return pod
}

// fakeProvider is a minimal ai.Provider for tests: it records every request it
// receives and returns a canned summary or a canned error. If failIfCalled is
// set, Summarize fails the test outright — used to prove the AI gate is
// closed for non-eligible causes (the provider must never be invoked).
type fakeProvider struct {
	summary      string
	err          error
	failIfCalled *testing.T
	calls        []ai.Request
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) Summarize(_ context.Context, req ai.Request) (string, error) {
	f.calls = append(f.calls, req)
	if f.failIfCalled != nil {
		f.failIfCalled.Helper()
		f.failIfCalled.Errorf("provider.Summarize called unexpectedly with %+v", req)
	}
	if f.err != nil {
		return "", f.err
	}
	return f.summary, nil
}

var _ ai.Provider = (*fakeProvider)(nil)
