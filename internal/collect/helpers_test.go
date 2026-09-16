package collect

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// IMPORTANT — how the fake clientset handles pod logs.
//
// FakePods.GetLogs records an action (verb "get", resource "pods", subresource
// "log") at CALL time and always streams the literal body "fake logs"; the
// content is a constant of the fake, not of anything under test. These tests
// therefore assert log PLUMBING only — whether a log action was recorded at
// all, and the *corev1.PodLogOptions it carried (Previous, TailLines,
// Container) — and never assert on log CONTENT.
//
// The fake also ignores field selectors on List. Event fixtures live in the
// same namespace as the pod and the tests rely on the collector's client-side
// filtering; every event fixture set includes an event for a DIFFERENT pod to
// prove that filter actually runs.

const (
	testNamespace = "prod"
	testPodName   = "web-6d4f-abcde"
	testContainer = "app"
	testNodeName  = "node-1"
	testPodUID    = "pod-uid-1"
	testImage     = "registry.example.com/app:1.2.3"
)

var fixedNow = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

func testOptions() Options {
	opts := DefaultOptions()
	opts.Now = func() time.Time { return fixedNow }
	return opts
}

func newClient(t *testing.T, objs ...runtime.Object) *fake.Clientset {
	t.Helper()
	return fake.NewClientset(objs...)
}

func crashedStatus(name string) corev1.ContainerStatus {
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
				StartedAt:  metav1.NewTime(fixedNow.Add(-30 * time.Minute)),
				FinishedAt: metav1.NewTime(fixedNow.Add(-5 * time.Minute)),
			},
		},
	}
}

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

func crashedPod(t *testing.T) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{crashedStatus(testContainer)}
	return pod
}

func healthyPod(t *testing.T) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  testContainer,
		Image: testImage,
		Ready: true,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(fixedNow.Add(-2 * time.Hour)),
			},
		},
	}}
	return pod
}

func pendingPod(t *testing.T) *corev1.Pod {
	t.Helper()
	pod := basePod(t)
	pod.Spec.NodeName = ""
	pod.Status.Phase = corev1.PodPending
	pod.Status.QOSClass = ""
	return pod
}

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

// otherPodEvent is an event in the SAME namespace for a DIFFERENT pod. It must
// never appear in collected events — the fake ignores field selectors, so this
// only gets filtered out by the collector's own client-side filter.
func otherPodEvent() *corev1.Event {
	return &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "noise-event", Namespace: testNamespace},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "some-other-pod",
			Namespace: testNamespace,
			UID:       "other-uid",
		},
		Type:          corev1.EventTypeWarning,
		Reason:        "Unhealthy",
		Message:       "belongs to another pod",
		Count:         1,
		LastTimestamp: metav1.NewTime(fixedNow),
	}
}

func testNode(memory, disk, pid corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: testNodeName},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeMemoryPressure, Status: memory},
				{Type: corev1.NodeDiskPressure, Status: disk},
				{Type: corev1.NodePIDPressure, Status: pid},
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func controllerRef(kind, name, uid string) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		Kind:       kind,
		Name:       name,
		UID:        types.UID(uid),
		Controller: &controller,
	}
}

func matchingActions(t *testing.T, cs *fake.Clientset, verb, res, subresource string) []k8stesting.Action {
	t.Helper()
	var out []k8stesting.Action
	for _, a := range cs.Actions() {
		if a.GetVerb() != verb || a.GetResource().Resource != res || a.GetSubresource() != subresource {
			continue
		}
		out = append(out, a)
	}
	return out
}

func logActions(t *testing.T, cs *fake.Clientset) []*corev1.PodLogOptions {
	t.Helper()
	var out []*corev1.PodLogOptions
	for _, a := range cs.Actions() {
		if a.GetResource().Resource != "pods" || a.GetSubresource() != "log" {
			continue
		}
		generic, ok := a.(k8stesting.GenericActionImpl)
		if !ok {
			t.Fatalf("log action has unexpected type %T", a)
		}
		opts, ok := generic.Value.(*corev1.PodLogOptions)
		if !ok {
			t.Fatalf("log action Value has unexpected type %T", generic.Value)
		}
		out = append(out, opts)
	}
	return out
}

func countActions(t *testing.T, cs *fake.Clientset, verb, res, subresource string) int {
	t.Helper()
	return len(matchingActions(t, cs, verb, res, subresource))
}
