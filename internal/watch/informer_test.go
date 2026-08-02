package watch

import (
	"context"
	"net/http"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	toolscache "k8s.io/client-go/tools/cache"
)

// deletedFinalStateUnknown wraps obj in the tombstone the informer delivers
// when a deletion was only noticed as a cache-resync gap.
func deletedFinalStateUnknown(obj any) toolscache.DeletedFinalStateUnknown {
	return toolscache.DeletedFinalStateUnknown{Key: testNamespace + "/" + testPodName, Obj: obj}
}

// httpStatus performs a GET against a bound listener and returns its status
// code, or 0 when the request could not be made at all.
func httpStatus(t *testing.T, addr, path string) int {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

// TestControllerInformerSmoke is the only test that exercises the real runtime:
// informers, the worker pool, and the health/metrics listeners. Everything
// else drives processPod synchronously, because informer timing makes exact
// "one line, not two" assertions impossible.
//
// It asserts the wiring, not the policy: /healthz answers immediately,
// /readyz only flips once the caches are synced, a pod created through the
// clientset reaches the sinks, and cancelling the context stops Run cleanly.
func TestControllerInformerSmoke(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	cfg := dedupConfig(6*time.Hour, time.Hour)
	cfg.HealthAddr = "127.0.0.1:0"
	cfg.MetricsAddr = "127.0.0.1:0"
	c, client, out := newTestController(t, clk, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- c.Run(ctx) }()

	if !waitFor(t, 10*time.Second, func() bool { return c.healthAddr() != "" }) {
		t.Fatal("health listener never published a bound address")
	}
	health := c.healthAddr()
	metrics := c.metricsAddr()
	// Identical HealthAddr and MetricsAddr are a legitimate configuration: one
	// listener serves all three paths rather than failing to bind twice.
	if metrics != health {
		t.Fatalf("identical addresses bound separately: health %q, metrics %q", health, metrics)
	}

	// Liveness is unconditional: a replica that is up but not yet synced (or a
	// standby waiting for the lease) must not be restarted by the kubelet.
	if got := httpStatus(t, health, "/healthz"); got != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d", got, http.StatusOK)
	}

	// Readiness waits for the informer caches.
	if !waitFor(t, 15*time.Second, func() bool {
		return httpStatus(t, health, "/readyz") == http.StatusOK
	}) {
		t.Fatalf("GET /readyz never returned %d", http.StatusOK)
	}

	if got := httpStatus(t, metrics, "/metrics"); got != http.StatusOK {
		t.Fatalf("GET /metrics = %d, want %d", got, http.StatusOK)
	}

	pod := crashLoopPod(testPodName, testPodUID, 3)
	if _, err := client.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	if !waitFor(t, 15*time.Second, func() bool { return len(out.lines()) > 0 }) {
		t.Fatalf("no diagnosis reached the sink within the timeout; output:\n%s", out.String())
	}

	ds := out.diagnoses(t)
	if ds[0].Pod != testPodName || ds[0].Namespace != testNamespace {
		t.Fatalf("emitted %s/%s, want %s/%s", ds[0].Namespace, ds[0].Pod, testNamespace, testPodName)
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after context cancellation", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}

	// The listeners are shut down with Run.
	if got := httpStatus(t, health, "/healthz"); got != 0 {
		t.Fatalf("health listener still answering after shutdown: status %d", got)
	}
}

// TestControllerBuildFactories checks the namespace scoping decision: no
// allowlist means one cluster-wide factory, an allowlist means one factory per
// distinct namespace (so the controller only needs RBAC where it was granted).
func TestControllerBuildFactories(t *testing.T) {
	clk := newFakeClock(testBaseTime)

	t.Run("cluster wide", func(t *testing.T) {
		c, _, _ := newTestController(t, clk, dedupConfig(time.Hour, time.Hour))
		factories, err := c.buildFactories()
		if err != nil {
			t.Fatalf("buildFactories: %v", err)
		}
		if len(factories) != 1 {
			t.Fatalf("got %d factories, want 1 cluster-wide factory", len(factories))
		}
	})

	t.Run("deduplicates namespaces", func(t *testing.T) {
		cfg := dedupConfig(time.Hour, time.Hour)
		cfg.Namespaces = []string{"prod", " staging ", "prod", ""}
		c, _, _ := newTestController(t, clk, cfg)
		factories, err := c.buildFactories()
		if err != nil {
			t.Fatalf("buildFactories: %v", err)
		}
		if len(factories) != 2 {
			t.Fatalf("got %d factories, want 2 (prod, staging)", len(factories))
		}
	})

	t.Run("all namespaces blank", func(t *testing.T) {
		cfg := dedupConfig(time.Hour, time.Hour)
		cfg.Namespaces = []string{"", "   "}
		c, _, _ := newTestController(t, clk, cfg)
		if _, err := c.buildFactories(); err == nil {
			t.Fatal("an allowlist of only blank namespaces must be an error")
		}
	})
}

// TestPodFromDeleteEvent covers the tombstone unwrapping that keeps a delete
// observed only as a cache-resync gap from leaking a workload's series forever.
func TestPodFromDeleteEvent(t *testing.T) {
	pod := crashLoopPod(testPodName, testPodUID, 1)

	if got := podFromDeleteEvent(pod); got != pod {
		t.Fatal("a plain pod must be returned unchanged")
	}
	if got := podFromDeleteEvent(deletedFinalStateUnknown(pod)); got != pod {
		t.Fatal("a tombstone wrapping a pod must be unwrapped")
	}
	if got := podFromDeleteEvent("not a pod"); got != nil {
		t.Fatalf("an unknown object must yield nil, got %+v", got)
	}
	if got := podFromDeleteEvent(deletedFinalStateUnknown("not a pod")); got != nil {
		t.Fatalf("a tombstone wrapping a non-pod must yield nil, got %+v", got)
	}
}

// TestStartServersDisabled: empty addresses mean no listener at all, which is
// how inspect-style one-shot usage and tests that only drive processPod run.
func TestStartServersDisabled(t *testing.T) {
	clk := newFakeClock(testBaseTime)
	c, _, _ := newTestController(t, clk, dedupConfig(time.Hour, time.Hour))

	servers, err := c.startServers()
	if err != nil {
		t.Fatalf("startServers: %v", err)
	}
	defer shutdownServers(servers)

	if len(servers) != 0 {
		t.Fatalf("started %d servers with no addresses configured, want 0", len(servers))
	}
	if c.healthAddr() != "" || c.metricsAddr() != "" {
		t.Fatalf("bound addresses %q / %q, want both empty", c.healthAddr(), c.metricsAddr())
	}
}

func TestNamespacesForLog(t *testing.T) {
	if got := namespacesForLog(nil); got != "(all)" {
		t.Fatalf("namespacesForLog(nil) = %q, want %q", got, "(all)")
	}
	if got := namespacesForLog([]string{"prod", "staging"}); got != "prod,staging" {
		t.Fatalf("namespacesForLog = %q, want %q", got, "prod,staging")
	}
}
