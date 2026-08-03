package watch

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/ryckakas/crashcause/internal/engine"
)

// IMPORTANT — what these tests do and do not depend on.
//
// The controller is driven SYNCHRONOUSLY through processPod wherever possible:
// dedup, metric lifecycle and lifecycle-noise suppression are pure functions of
// the pod objects handed in, so no informer timing is involved and the
// assertions are exact ("exactly one line") rather than eventual. Only
// TestControllerInformerSmoke starts a real informer.
//
// The owning ReplicaSet is deliberately NOT seeded into the fake clientset: the
// collector's owner resolution then fails its ReplicaSet GET and degrades to
// Owner{Kind:"ReplicaSet", Name:"api-5d4f"}, which is stable and identical for
// every pod carrying the same ownerReference (including the ReplicaSet twin).
// That is exactly the workload identity the dedup cache is keyed on.
//
// Log output is never asserted on: slog writes to stderr and carries no
// contract. Emissions are asserted on the writer passed to New.

const (
	testNamespace = "prod"
	testContainer = "app"
	testOwnerKind = "ReplicaSet"
	testOwnerName = "api-5d4f"
	testOwnerUID  = types.UID("rs-uid-1")
	testPodName   = "api-5d4f-aaaaa"
	testPodUID    = types.UID("pod-uid-a")
	testTwinName  = "api-5d4f-bbbbb"
	testTwinUID   = types.UID("pod-uid-b")
	testNodeName  = "node-1"
	testImage     = "registry.example.com/api:1.2.3"

	diagnosesMetric = "crashcause_diagnoses_total"
)

// testBaseTime is where every injected clock starts.
var testBaseTime = time.Date(2026, 3, 4, 12, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Clock
// ---------------------------------------------------------------------------

// fakeClock is a manually advanced clock. It is mutex guarded because the
// controller reads it from informer, worker and sweep goroutines.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(start time.Time) *fakeClock {
	return &fakeClock{now: start}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// ---------------------------------------------------------------------------
// Writer
// ---------------------------------------------------------------------------

// lockedWriter is a mutex-guarded io.Writer. A bare bytes.Buffer would race:
// the stdout sink writes from worker goroutines while the test reads.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// lines returns every non-empty line written so far.
func (w *lockedWriter) lines() []string {
	var out []string
	for _, l := range strings.Split(w.String(), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// diagnoses decodes every emitted line into a Diagnosis.
func (w *lockedWriter) diagnoses(t *testing.T) []engine.Diagnosis {
	t.Helper()
	lines := w.lines()
	out := make([]engine.Diagnosis, 0, len(lines))
	for _, l := range lines {
		var d engine.Diagnosis
		if err := json.Unmarshal([]byte(l), &d); err != nil {
			t.Fatalf("emitted line is not a Diagnosis JSON object: %v\nline: %s", err, l)
		}
		out = append(out, d)
	}
	return out
}

// ---------------------------------------------------------------------------
// Controller construction
// ---------------------------------------------------------------------------

// newTestController builds a controller on a fake clientset with the injected
// clock wired into Config.clock (and therefore into the collector, the engine's
// Inputs.Now and the dedup cache).
func newTestController(t *testing.T, clk *fakeClock, cfg Config, objs ...runtime.Object) (*Controller, *fake.Clientset, *lockedWriter) {
	t.Helper()
	client := fake.NewClientset(objs...)
	if clk != nil {
		cfg.clock = clk.Now
	}
	if cfg.sweepInterval <= 0 {
		cfg.sweepInterval = time.Minute
	}
	out := &lockedWriter{}
	c, err := New(client, cfg, nil, out)
	if err != nil {
		t.Fatalf("watch.New: %v", err)
	}
	return c, client, out
}

// dedupConfig is the configuration every dedup/metrics test starts from: no log
// collection (so the fake clientset sees the minimum possible traffic) and
// explicit, small durations that survive withDefaults.
func dedupConfig(ttl, reemit time.Duration) Config {
	return Config{
		CollectLogs:    false,
		HealthAddr:     "",
		MetricsAddr:    "",
		DedupTTL:       ttl,
		ReemitInterval: reemit,
		sweepInterval:  time.Minute,
	}
}

// ---------------------------------------------------------------------------
// Pod fixtures
// ---------------------------------------------------------------------------

func boolPtr(b bool) *bool { return &b }

// replicaSetRef is the controlling ownerReference shared by a pod and its
// ReplicaSet twin: same UID and name, so both resolve to the same workload.
func replicaSetRef() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "apps/v1",
		Kind:       testOwnerKind,
		Name:       testOwnerName,
		UID:        testOwnerUID,
		Controller: boolPtr(true),
	}
}

// basePod is a scheduled, ReplicaSet-owned pod with no container statuses.
func basePod(name string, uid types.UID) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testNamespace,
			UID:             uid,
			OwnerReferences: []metav1.OwnerReference{replicaSetRef()},
		},
		Spec: corev1.PodSpec{
			NodeName:      testNodeName,
			RestartPolicy: corev1.RestartPolicyAlways,
			Containers: []corev1.Container{{
				Name:  testContainer,
				Image: testImage,
			}},
		},
		Status: corev1.PodStatus{
			Phase:    corev1.PodRunning,
			QOSClass: corev1.PodQOSBurstable,
		},
	}
}

// crashLoopPod is the canonical crashing fixture: CrashLoopBackOff waiting with
// a previous non-zero exit, which the engine classifies as app_exit_nonzero.
func crashLoopPod(name string, uid types.UID, restarts int32) *corev1.Pod {
	pod := basePod(name, uid)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         testContainer,
		Image:        testImage,
		RestartCount: restarts,
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
				StartedAt:  metav1.NewTime(testBaseTime.Add(-30 * time.Minute)),
				FinishedAt: metav1.NewTime(testBaseTime.Add(-time.Duration(restarts) * time.Minute)),
			},
		},
	}}
	return pod
}

// oomPod is the same workload crashing for a DIFFERENT reason: the kubelet
// attributed an OOM kill, so the engine's primary cause becomes oom_killed.
func oomPod(name string, uid types.UID, restarts int32) *corev1.Pod {
	pod := crashLoopPod(name, uid, restarts)
	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{
		ExitCode:   137,
		Signal:     9,
		Reason:     "OOMKilled",
		StartedAt:  metav1.NewTime(testBaseTime.Add(-30 * time.Minute)),
		FinishedAt: metav1.NewTime(testBaseTime.Add(-time.Duration(restarts) * time.Minute)),
	}
	return pod
}

// terminatingPod is ordinary rolling-update churn: the pod is being deleted and
// its container took SIGTERM (exit 143). The engine must report nothing at all.
func terminatingPod(name string, uid types.UID) *corev1.Pod {
	pod := basePod(name, uid)
	deleted := metav1.NewTime(testBaseTime.Add(-10 * time.Second))
	pod.DeletionTimestamp = &deleted
	term := &corev1.ContainerStateTerminated{
		ExitCode:   143,
		Signal:     15,
		Reason:     "Error",
		StartedAt:  metav1.NewTime(testBaseTime.Add(-time.Hour)),
		FinishedAt: metav1.NewTime(testBaseTime.Add(-10 * time.Second)),
	}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:                 testContainer,
		Image:                testImage,
		RestartCount:         1,
		State:                corev1.ContainerState{Terminated: term},
		LastTerminationState: corev1.ContainerState{Terminated: term.DeepCopy()},
	}}
	return pod
}

// healthyPod is a plain running pod: nothing about it warrants a trigger.
func healthyPod(name string, uid types.UID) *corev1.Pod {
	pod := basePod(name, uid)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  testContainer,
		Image: testImage,
		Ready: true,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.NewTime(testBaseTime.Add(-2 * time.Hour)),
			},
		},
	}}
	return pod
}

// testWorkloadKey is the workload every fixture above belongs to.
func testWorkloadKey() workloadKey {
	return workloadKey{namespace: testNamespace, owner: testOwnerKind + "/" + testOwnerName}
}

// testOwner is the engine.Owner the collector degrades to for these fixtures.
func testOwner() engine.Owner {
	return engine.Owner{Kind: testOwnerKind, Name: testOwnerName}
}

// ---------------------------------------------------------------------------
// /metrics scraping
// ---------------------------------------------------------------------------

// promSample is one parsed line of Prometheus exposition text.
type promSample struct {
	labels map[string]string
	value  float64
}

// scrapeMetrics renders the controller's own registry exactly as the /metrics
// endpoint would.
func scrapeMetrics(t *testing.T, c *Controller) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.prom.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), "GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("scrape /metrics: status %d", rec.Code)
	}
	return rec.Body.String()
}

// parseSamples extracts every sample of one metric from exposition text. Label
// values in these tests never contain commas or escapes, so a split-based
// parser is sufficient and keeps the suite free of extra dependencies.
func parseSamples(t *testing.T, body, metric string) []promSample {
	t.Helper()
	var out []promSample
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if open := strings.Index(line, "{"); open >= 0 {
			if line[:open] != metric {
				continue
			}
			end := strings.LastIndex(line, "}")
			if end < open {
				t.Fatalf("malformed exposition line: %q", line)
			}
			out = append(out, promSample{
				labels: parseLabels(t, line[open+1:end]),
				value:  parseValue(t, line[end+1:]),
			})
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != metric {
			continue
		}
		out = append(out, promSample{labels: map[string]string{}, value: parseValue(t, fields[1])})
	}
	return out
}

func parseLabels(t *testing.T, s string) map[string]string {
	t.Helper()
	labels := make(map[string]string)
	if strings.TrimSpace(s) == "" {
		return labels
	}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.Index(pair, "=")
		if eq < 0 {
			t.Fatalf("malformed label pair: %q", pair)
		}
		labels[pair[:eq]] = strings.Trim(pair[eq+1:], `"`)
	}
	return labels
}

func parseValue(t *testing.T, s string) float64 {
	t.Helper()
	fields := strings.Fields(s)
	if len(fields) == 0 {
		t.Fatalf("exposition line has no value: %q", s)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		t.Fatalf("parse sample value %q: %v", fields[0], err)
	}
	return v
}

// diagnosesSamples returns every crashcause_diagnoses_total sample currently
// exposed.
func diagnosesSamples(t *testing.T, c *Controller) []promSample {
	t.Helper()
	return parseSamples(t, scrapeMetrics(t, c), diagnosesMetric)
}

// workloadSamples narrows the samples to the canonical test workload.
func workloadSamples(t *testing.T, c *Controller) []promSample {
	t.Helper()
	var out []promSample
	for _, s := range diagnosesSamples(t, c) {
		if s.labels["namespace"] == testNamespace &&
			s.labels["owner_kind"] == testOwnerKind &&
			s.labels["owner_name"] == testOwnerName {
			out = append(out, s)
		}
	}
	return out
}

// causeCount returns the counter value for one cause of the canonical workload.
func causeCount(t *testing.T, c *Controller, cause engine.CauseCode) (float64, bool) {
	t.Helper()
	for _, s := range workloadSamples(t, c) {
		if s.labels["cause"] == string(cause) {
			return s.value, true
		}
	}
	return 0, false
}

// requireCauseCount asserts the exact counter value for one cause.
func requireCauseCount(t *testing.T, c *Controller, cause engine.CauseCode, want float64) {
	t.Helper()
	got, ok := causeCount(t, c, cause)
	if !ok {
		t.Fatalf("no %s series for cause %q; scrape:\n%s", diagnosesMetric, cause, scrapeMetrics(t, c))
	}
	if got != want {
		t.Fatalf("%s{cause=%q} = %v, want %v; scrape:\n%s", diagnosesMetric, cause, got, want, scrapeMetrics(t, c))
	}
}

// requireLineCount asserts exactly n emissions have been written.
func requireLineCount(t *testing.T, w *lockedWriter, n int) {
	t.Helper()
	if got := len(w.lines()); got != n {
		t.Fatalf("emitted %d lines, want %d:\n%s", got, n, w.String())
	}
}

// waitFor polls cond until it holds or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}
