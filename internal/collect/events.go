package collect

import (
	"context"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"

	"github.com/ryckakas/crashcause/internal/engine"
)

// collectEvents lists the pod's events. Event collection never fails
// collection: on a persistent API error the pod is simply diagnosed without
// event evidence.
//
// Every event for the pod is kept — deliberately no reason whitelist. A
// whitelist would silently drop reasons that future rules (or the AI layer)
// need, and the engine already ignores events it does not care about.
func (c *Collector) collectEvents(ctx context.Context, pod *corev1.Pod) []engine.Event {
	selector := fields.Set{
		"involvedObject.name":      pod.Name,
		"involvedObject.namespace": pod.Namespace,
	}.AsSelector().String()

	list, err := c.client.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{FieldSelector: selector})
	if err != nil {
		// Some API servers / proxies reject field selectors on events; retry
		// once unfiltered and lean on the client-side filter below.
		list, err = c.client.CoreV1().Events(pod.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil
		}
	}

	out := make([]engine.Event, 0, len(list.Items))
	for i := range list.Items {
		ev := &list.Items[i]
		// Always filter client-side: the fake clientset (and some real
		// servers) ignore field selectors entirely.
		if ev.InvolvedObject.Name != pod.Name {
			continue
		}
		if ev.InvolvedObject.UID != "" && pod.UID != "" && ev.InvolvedObject.UID != pod.UID {
			continue
		}
		out = append(out, engine.Event{
			Type:      ev.Type,
			Reason:    ev.Reason,
			Message:   ev.Message,
			Count:     ev.Count,
			FirstSeen: eventTime(ev.FirstTimestamp, ev),
			LastSeen:  eventTime(ev.LastTimestamp, ev),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.LastSeen.Equal(b.LastSeen) {
			return a.LastSeen.Before(b.LastSeen)
		}
		if a.Reason != b.Reason {
			return a.Reason < b.Reason
		}
		return a.Message < b.Message
	})
	return out
}

// eventTime falls back to the newer EventTime field when the legacy
// timestamp is unset (events recorded by the modern events client only set
// EventTime).
func eventTime(ts metav1.Time, ev *corev1.Event) time.Time {
	if !ts.Time.IsZero() {
		return ts.Time
	}
	return ev.EventTime.Time
}
