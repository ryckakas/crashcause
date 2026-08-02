package watch

import (
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// waitingReasonsWarrantingDiagnosis mirrors the set the collector uses: the
// exact state.waiting reasons that are worth an API round trip on their own.
// It is duplicated here (rather than imported) because collect keeps it
// unexported, and because this copy answers a different question — "is this
// update worth collecting for?" — which must be answerable without any I/O.
var waitingReasonsWarrantingDiagnosis = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerConfigError": true,
}

// podFingerprint summarizes everything about a pod that can make it newly
// interesting: per-container restart counts and state kinds, plus the pod
// phase/reason and whether it is terminating.
//
// Two updates with the same fingerprint describe the same observed situation,
// so the second one is dropped without any API call. This is what keeps an
// informer resync (which re-delivers every pod every 10 minutes) and the
// stream of unrelated status writes Kubernetes performs (conditions, IPs,
// readiness gates) from turning into collection storms.
func podFingerprint(pod *corev1.Pod) string {
	var b strings.Builder
	b.WriteString(string(pod.Status.Phase))
	b.WriteByte('|')
	b.WriteString(pod.Status.Reason)
	b.WriteByte('|')
	if pod.DeletionTimestamp != nil {
		b.WriteString("deleting")
	}
	b.WriteByte('|')
	writeStatusFingerprints(&b, pod.Status.InitContainerStatuses)
	b.WriteByte('|')
	writeStatusFingerprints(&b, pod.Status.ContainerStatuses)
	b.WriteByte('|')
	writeStatusFingerprints(&b, pod.Status.EphemeralContainerStatuses)
	return b.String()
}

func writeStatusFingerprints(b *strings.Builder, statuses []corev1.ContainerStatus) {
	for i := range statuses {
		st := &statuses[i]
		b.WriteString(st.Name)
		b.WriteByte(':')
		b.WriteString(strconv.Itoa(int(st.RestartCount)))
		b.WriteByte(':')
		switch {
		case st.State.Waiting != nil:
			b.WriteString("w=")
			b.WriteString(st.State.Waiting.Reason)
		case st.State.Terminated != nil:
			t := st.State.Terminated
			b.WriteString("t=")
			b.WriteString(strconv.Itoa(int(t.ExitCode)))
			b.WriteByte('/')
			b.WriteString(t.Reason)
			b.WriteByte('/')
			b.WriteString(strconv.FormatInt(t.FinishedAt.Unix(), 10))
		case st.State.Running != nil:
			b.WriteString("r=")
			b.WriteString(strconv.FormatInt(st.State.Running.StartedAt.Unix(), 10))
		default:
			b.WriteString("none")
		}
		if t := st.LastTerminationState.Terminated; t != nil {
			b.WriteString(":l=")
			b.WriteString(strconv.Itoa(int(t.ExitCode)))
			b.WriteByte('/')
			b.WriteString(t.Reason)
			b.WriteByte('/')
			b.WriteString(strconv.FormatInt(t.FinishedAt.Unix(), 10))
		}
		b.WriteByte(';')
	}
}

// warrantsTrigger reports whether a pod is in a state worth collecting for.
//
// It is intentionally a superset of what the engine will actually diagnose:
// its job is to filter out the overwhelming majority of pod updates (healthy
// pods, readiness flaps, scheduled-and-running Pending pods) at zero API cost,
// while never suppressing something the engine could have classified. The
// collector and the engine make the real decision afterwards, and both are
// allowed to conclude "nothing to report".
func warrantsTrigger(pod *corev1.Pod, now time.Time, initStuckThreshold time.Duration) bool {
	if pod == nil {
		return false
	}
	// Evicted is a pod-level status with no interesting container state.
	if pod.Status.Reason == "Evicted" {
		return true
	}
	// An unscheduled Pending pod is the unschedulable candidate; the
	// FailedScheduling event that proves it is only visible after collection.
	// Requiring an empty NodeName keeps ordinary "scheduled, still pulling"
	// Pending pods out of the trigger path.
	if pod.Status.Phase == corev1.PodPending && pod.Spec.NodeName == "" {
		return true
	}

	if containerWarrantsTrigger(pod.Status.ContainerStatuses, false, now, initStuckThreshold) {
		return true
	}
	if containerWarrantsTrigger(pod.Status.InitContainerStatuses, true, now, initStuckThreshold) {
		return true
	}
	return containerWarrantsTrigger(pod.Status.EphemeralContainerStatuses, false, now, initStuckThreshold)
}

func containerWarrantsTrigger(statuses []corev1.ContainerStatus, isInit bool, now time.Time, initStuckThreshold time.Duration) bool {
	for i := range statuses {
		st := &statuses[i]
		if t := st.State.Terminated; t != nil && (t.ExitCode != 0 || st.RestartCount > 0) {
			return true
		}
		if t := st.LastTerminationState.Terminated; t != nil && (t.ExitCode != 0 || st.RestartCount > 0) {
			return true
		}
		if w := st.State.Waiting; w != nil && waitingReasonsWarrantingDiagnosis[w.Reason] {
			return true
		}
		if isInit && st.State.Running != nil {
			if now.Sub(st.State.Running.StartedAt.Time) > initStuckThreshold {
				return true
			}
		}
	}
	return false
}
