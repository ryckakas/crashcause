package collect

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ryckakas/crashcause/internal/engine"
)

// nodeConditions reads the pressure conditions of the pod's node. Node access
// is a severable privilege (the Helm chart can omit it) and an unscheduled pod
// has no node at all, so any failure simply yields Known=false and collection
// continues.
func (c *Collector) nodeConditions(ctx context.Context, nodeName string) engine.NodeConditions {
	if !c.opts.CollectNode || nodeName == "" {
		return engine.NodeConditions{}
	}
	node, err := c.client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
	if err != nil {
		return engine.NodeConditions{}
	}

	out := engine.NodeConditions{Known: true}
	for i := range node.Status.Conditions {
		cond := &node.Status.Conditions[i]
		if cond.Status != corev1.ConditionTrue {
			continue
		}
		switch cond.Type {
		case corev1.NodeMemoryPressure:
			out.MemoryPressure = true
		case corev1.NodeDiskPressure:
			out.DiskPressure = true
		case corev1.NodePIDPressure:
			out.PIDPressure = true
		case corev1.NodeReady, corev1.NodeNetworkUnavailable:
			// Not a pressure condition; nothing to record.
		}
	}
	return out
}
