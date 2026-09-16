package collect

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/ryckakas/crashcause/internal/engine"
)

// Kubelet defaults applied when a probe leaves the field at zero. These mirror
// the API server's defaulting so the engine reasons about the values the
// kubelet actually uses, not the (possibly empty) values the user typed.
const (
	defaultProbePeriodSeconds    int32 = 10
	defaultProbeFailureThreshold int32 = 3
	defaultProbeTimeoutSeconds   int32 = 1
)

// containerSpecView is the subset of a container's spec the collector needs,
// unified across regular, init and ephemeral containers.
type containerSpecView struct {
	image     string
	resources corev1.ResourceRequirements
	liveness  *corev1.Probe
	readiness *corev1.Probe
	startup   *corev1.Probe
}

func findContainerSpec(pod *corev1.Pod, name string) *containerSpecView {
	if name == "" {
		return nil
	}
	for i := range pod.Spec.Containers {
		if c := &pod.Spec.Containers[i]; c.Name == name {
			return &containerSpecView{
				image:     c.Image,
				resources: c.Resources,
				liveness:  c.LivenessProbe,
				readiness: c.ReadinessProbe,
				startup:   c.StartupProbe,
			}
		}
	}
	for i := range pod.Spec.InitContainers {
		if c := &pod.Spec.InitContainers[i]; c.Name == name {
			return &containerSpecView{
				image:     c.Image,
				resources: c.Resources,
				liveness:  c.LivenessProbe,
				readiness: c.ReadinessProbe,
				startup:   c.StartupProbe,
			}
		}
	}
	for i := range pod.Spec.EphemeralContainers {
		if c := &pod.Spec.EphemeralContainers[i]; c.Name == name {
			common := c.EphemeralContainerCommon
			return &containerSpecView{
				image:     common.Image,
				resources: common.Resources,
				liveness:  common.LivenessProbe,
				readiness: common.ReadinessProbe,
				startup:   common.StartupProbe,
			}
		}
	}
	return nil
}

func probeSpec(p *corev1.Probe) engine.ProbeSpec {
	if p == nil {
		return engine.ProbeSpec{}
	}
	out := engine.ProbeSpec{
		Defined:             true,
		FailureThreshold:    p.FailureThreshold,
		PeriodSeconds:       p.PeriodSeconds,
		InitialDelaySeconds: p.InitialDelaySeconds,
		TimeoutSeconds:      p.TimeoutSeconds,
	}
	if out.PeriodSeconds == 0 {
		out.PeriodSeconds = defaultProbePeriodSeconds
	}
	if out.FailureThreshold == 0 {
		out.FailureThreshold = defaultProbeFailureThreshold
	}
	if out.TimeoutSeconds == 0 {
		out.TimeoutSeconds = defaultProbeTimeoutSeconds
	}
	return out
}

func resourceMap(list corev1.ResourceList) map[string]string {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]string, len(list))
	for name, quantity := range list {
		out[string(name)] = quantity.String()
	}
	return out
}
