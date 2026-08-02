package collect

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/ryckakas/crashcause/internal/engine"
)

// ownerCacheTTL bounds how long a resolved owner is reused. Watch mode
// resolves the same owner for every crash of every pod of a workload, so the
// cache turns O(crashes) API calls into O(workloads) per TTL window.
const ownerCacheTTL = 10 * time.Minute

// Owner kinds handled specially during resolution.
const (
	kindReplicaSet  = "ReplicaSet"
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
	kindJob         = "Job"
	kindCronJob     = "CronJob"
)

// ownerCacheEntry is a resolved owner plus its expiry. Failed lookups are
// cached too, so a missing RBAC permission does not cause a GET per crash.
type ownerCacheEntry struct {
	owner   engine.Owner
	expires time.Time
}

// resolveOwner resolves the pod's controller to a human-meaningful workload.
// It returns the resolved owner plus the RAW kind from the ownerReference
// (before Deployment/CronJob resolution), which the caller needs for the
// best-effort OwnerRolling signal.
func (c *Collector) resolveOwner(ctx context.Context, pod *corev1.Pod) (engine.Owner, string) {
	ref := podOwnerRef(pod)
	if ref == nil {
		return engine.Owner{}, ""
	}
	if cached, ok := c.cachedOwner(ref.UID); ok {
		return cached, ref.Kind
	}

	owner := engine.Owner{Kind: ref.Kind, Name: ref.Name}
	switch ref.Kind {
	case kindReplicaSet:
		// A ReplicaSet name is an implementation detail; report the Deployment
		// when there is one. On any error keep the ReplicaSet.
		rs, err := c.client.AppsV1().ReplicaSets(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			if dep := controllerOwnerRef(rs.OwnerReferences); dep != nil && dep.Kind == kindDeployment {
				owner = engine.Owner{Kind: kindDeployment, Name: dep.Name}
			}
		}
	case kindJob:
		// Keep the Job identity but record the CronJob so sinks can normalise
		// the per-run Job names into one stable series.
		job, err := c.client.BatchV1().Jobs(pod.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err == nil {
			if cj := controllerOwnerRef(job.OwnerReferences); cj != nil && cj.Kind == kindCronJob {
				owner.CronJobName = cj.Name
			}
		}
	}

	c.storeOwner(ref.UID, owner)
	return owner, ref.Kind
}

// cachedOwner returns a cached owner when present and unexpired.
func (c *Collector) cachedOwner(uid types.UID) (engine.Owner, bool) {
	if uid == "" {
		return engine.Owner{}, false
	}
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	entry, ok := c.ownerCache[uid]
	if !ok || !entry.expires.After(c.now()) {
		return engine.Owner{}, false
	}
	return entry.owner, true
}

// storeOwner caches a resolution (successful or degraded) under the owner UID.
func (c *Collector) storeOwner(uid types.UID, owner engine.Owner) {
	if uid == "" {
		return
	}
	c.ownerMu.Lock()
	defer c.ownerMu.Unlock()
	c.ownerCache[uid] = ownerCacheEntry{owner: owner, expires: c.now().Add(ownerCacheTTL)}
}

// podOwnerRef picks the controlling ownerReference, falling back to the first
// reference when nothing is marked as the controller.
func podOwnerRef(pod *corev1.Pod) *metav1.OwnerReference {
	if len(pod.OwnerReferences) == 0 {
		return nil
	}
	if ref := controllerOwnerRef(pod.OwnerReferences); ref != nil {
		return ref
	}
	return &pod.OwnerReferences[0]
}

// controllerOwnerRef returns the reference marked Controller=true, if any.
func controllerOwnerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if ref := &refs[i]; ref.Controller != nil && *ref.Controller {
			return ref
		}
	}
	return nil
}
