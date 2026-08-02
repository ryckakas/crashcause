package watch

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestPodFingerprintStability: two updates describing the same observed
// situation must fingerprint identically, so an informer resync (which
// re-delivers every pod every 10 minutes) costs nothing.
func TestPodFingerprintStability(t *testing.T) {
	pod := crashLoopPod(testPodName, testPodUID, 3)
	same := pod.DeepCopy()

	if podFingerprint(pod) != podFingerprint(same) {
		t.Fatalf("identical pods fingerprinted differently:\n%s\n%s",
			podFingerprint(pod), podFingerprint(same))
	}

	// Fields that change constantly on a healthy pod (IP, conditions,
	// readiness) must not move the fingerprint.
	noise := pod.DeepCopy()
	noise.Status.PodIP = "10.1.2.3"
	noise.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	noise.Status.ContainerStatuses[0].Ready = false
	noise.ResourceVersion = "999"
	if podFingerprint(pod) != podFingerprint(noise) {
		t.Fatalf("status noise changed the fingerprint:\n%s\n%s",
			podFingerprint(pod), podFingerprint(noise))
	}
}

func TestPodFingerprintChanges(t *testing.T) {
	base := crashLoopPod(testPodName, testPodUID, 3)

	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{
			name:   "restart count bump",
			mutate: func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 4 },
		},
		{
			name: "waiting reason change",
			mutate: func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].State.Waiting.Reason = "ImagePullBackOff"
			},
		},
		{
			name: "last termination exit code change",
			mutate: func(p *corev1.Pod) {
				p.Status.ContainerStatuses[0].LastTerminationState.Terminated.ExitCode = 137
			},
		},
		{
			name:   "phase change",
			mutate: func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		},
		{
			name:   "pod reason change",
			mutate: func(p *corev1.Pod) { p.Status.Reason = "Evicted" },
		},
		{
			name: "deletion started",
			mutate: func(p *corev1.Pod) {
				now := metav1.NewTime(testBaseTime)
				p.DeletionTimestamp = &now
			},
		},
		{
			name: "an init container appears",
			mutate: func(p *corev1.Pod) {
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{{
					Name:  "init",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(testBaseTime)}},
				}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mutated := base.DeepCopy()
			tc.mutate(mutated)
			if podFingerprint(base) == podFingerprint(mutated) {
				t.Fatalf("fingerprint did not change:\n%s", podFingerprint(base))
			}
		})
	}
}

// TestWarrantsTrigger is the zero-API-cost filter's truth table. It is
// deliberately a SUPERSET of what the engine will diagnose: false negatives hide
// crashes, false positives only cost one collection.
func TestWarrantsTrigger(t *testing.T) {
	const threshold = 10 * time.Minute

	initStatus := func(started time.Time) corev1.ContainerStatus {
		return corev1.ContainerStatus{
			Name:  "init-db",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(started)}},
		}
	}

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "nil pod",
			pod:  nil,
			want: false,
		},
		{
			name: "healthy running pod",
			pod:  healthyPod(testPodName, testPodUID),
			want: false,
		},
		{
			name: "crashloopbackoff",
			pod:  crashLoopPod(testPodName, testPodUID, 3),
			want: true,
		},
		{
			name: "evicted",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.Phase = corev1.PodFailed
				p.Status.Reason = "Evicted"
				return p
			}(),
			want: true,
		},
		{
			name: "unscheduled pending pod",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Spec.NodeName = ""
				p.Status.Phase = corev1.PodPending
				return p
			}(),
			want: true,
		},
		{
			name: "scheduled pending pod is ordinary startup",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.Phase = corev1.PodPending
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  testContainer,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}}
				return p
			}(),
			want: false,
		},
		{
			name: "image pull backoff",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:  testContainer,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
				}}
				return p
			}(),
			want: true,
		},
		{
			name: "init container stuck beyond the threshold",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.Phase = corev1.PodPending
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{initStatus(testBaseTime.Add(-11 * time.Minute))}
				return p
			}(),
			want: true,
		},
		{
			name: "init container running within the threshold",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.Phase = corev1.PodPending
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{initStatus(testBaseTime.Add(-time.Minute))}
				return p
			}(),
			want: false,
		},
		{
			name: "app container running long is not stuck",
			pod: func() *corev1.Pod {
				p := healthyPod(testPodName, testPodUID)
				p.Status.ContainerStatuses[0].State.Running.StartedAt = metav1.NewTime(testBaseTime.Add(-48 * time.Hour))
				return p
			}(),
			want: false,
		},
		{
			name: "terminated with a non-zero exit",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: testContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
						Reason:   "Error",
					}},
				}}
				return p
			}(),
			want: true,
		},
		{
			name: "terminated cleanly with no restarts",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.Phase = corev1.PodSucceeded
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: testContainer,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
						Reason:   "Completed",
					}},
				}}
				return p
			}(),
			want: false,
		},
		{
			name: "terminated cleanly but restarting in a loop",
			pod: func() *corev1.Pod {
				p := basePod(testPodName, testPodUID)
				p.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name:         testContainer,
					RestartCount: 4,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
						Reason:   "Completed",
					}},
				}}
				return p
			}(),
			want: true,
		},
		{
			name: "ephemeral debug container crashed",
			pod: func() *corev1.Pod {
				p := healthyPod(testPodName, testPodUID)
				p.Status.EphemeralContainerStatuses = []corev1.ContainerStatus{{
					Name: "debugger",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 2,
						Reason:   "Error",
					}},
				}}
				return p
			}(),
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := warrantsTrigger(tc.pod, testBaseTime, threshold); got != tc.want {
				t.Fatalf("warrantsTrigger = %v, want %v", got, tc.want)
			}
		})
	}
}
