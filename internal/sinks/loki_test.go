package sinks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ryckakas/crashcause/internal/engine"
)

// ---------------------------------------------------------------------------
// fixtures
// ---------------------------------------------------------------------------

// lokiWireStream / lokiWirePush mirror the on-the-wire push payload
// independently of the production types, so a change to the production
// structs cannot silently change what the tests believe the wire looks like.
type lokiWireStream struct {
	Stream map[string]string `json:"stream"`
	Values [][]string        `json:"values"`
}

type lokiWirePush struct {
	Streams []lokiWireStream `json:"streams"`
}

type lokiCapture struct {
	push        lokiWirePush
	raw         []byte
	contentType string
	authz       string
	tenant      string
	basicUser   string
	basicPass   string
	basicOK     bool
	path        string
}

type lokiTestServer struct {
	srv    *httptest.Server
	pushes chan lokiCapture
	status atomic.Int64
	count  atomic.Int64
	// closing releases any handler parked on the caller's gate. Without it a
	// gated test that fails an assertion never reaches its own close(gate),
	// httptest's Close waits forever on the in-flight request, and a fast
	// failure turns into a ten-minute test timeout.
	closing chan struct{}
}

// lokiNewTestServer starts an httptest server that decodes each push and
// publishes it on a buffered channel. gate, when non-nil, blocks the handler
// until it is closed (used to wedge the flusher and force buffer overflow).
func lokiNewTestServer(t *testing.T, gate <-chan struct{}) *lokiTestServer {
	t.Helper()
	ts := &lokiTestServer{
		pushes:  make(chan lokiCapture, 256),
		closing: make(chan struct{}),
	}
	ts.status.Store(http.StatusNoContent)

	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusInternalServerError)
			return
		}
		c := lokiCapture{
			raw:         raw,
			contentType: r.Header.Get("Content-Type"),
			authz:       r.Header.Get("Authorization"),
			tenant:      r.Header.Get("X-Scope-OrgID"),
			path:        r.URL.Path,
		}
		c.basicUser, c.basicPass, c.basicOK = r.BasicAuth()
		if err := json.Unmarshal(raw, &c.push); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		ts.count.Add(1)
		select {
		case ts.pushes <- c:
		default:
		}
		if gate != nil {
			select {
			case <-gate:
			case <-ts.closing:
			}
		}
		w.WriteHeader(int(ts.status.Load()))
	}))
	// LIFO: this runs BEFORE srv.Close below, so parked handlers are released
	// first and Close never waits on an in-flight request.
	t.Cleanup(ts.srv.Close)
	t.Cleanup(func() { close(ts.closing) })
	return ts
}

// lokiWaitPush waits for the next push or fails the test. Every wait in this
// file is bounded so the suite can never hang.
func lokiWaitPush(t *testing.T, ts *lokiTestServer, timeout time.Duration) lokiCapture {
	t.Helper()
	select {
	case c := <-ts.pushes:
		if c.path != lokiPushPath {
			t.Fatalf("push path = %q, want %q", c.path, lokiPushPath)
		}
		if c.contentType != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", c.contentType)
		}
		return c
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for a Loki push", timeout)
		return lokiCapture{}
	}
}

func lokiExpectNoPush(t *testing.T, ts *lokiTestServer, within time.Duration) {
	t.Helper()
	select {
	case c := <-ts.pushes:
		t.Fatalf("unexpected push: %s", c.raw)
	case <-time.After(within):
	}
}

func lokiTestDiagnosis(ns string, cause engine.CauseCode, ts time.Time) engine.Diagnosis {
	return engine.Diagnosis{
		Cause:       cause,
		Confidence:  engine.ConfidenceHigh,
		Explanation: "container was killed because it exceeded its memory limit <128Mi>",
		Evidence:    []string{"lastState.terminated.reason=OOMKilled"},
		NextSteps:   []string{"raise the memory limit"},
		Container:   "api",
		Pod:         "api-7c9f5b6d4-abcde",
		Namespace:   ns,
		Owner:       engine.Owner{Kind: "Deployment", Name: "api"},
		Timestamp:   ts,
	}
}

// lokiNewSink builds a sink pointed at ts and registers a bounded Close.
func lokiNewSink(t *testing.T, ts *lokiTestServer, cfg LokiConfig) LokiSink {
	t.Helper()
	cfg.URL = ts.srv.URL
	s, err := NewLoki(cfg)
	if err != nil {
		t.Fatalf("NewLoki: %v", err)
	}
	ls, ok := s.(LokiSink)
	if !ok {
		t.Fatalf("NewLoki returned %T, which does not implement LokiSink", s)
	}
	return ls
}

func lokiCloseSink(t *testing.T, s Sink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// lokiAssertLabels checks the stream carries EXACTLY {app, namespace, cause}.
func lokiAssertLabels(t *testing.T, got map[string]string, ns, cause string) {
	t.Helper()
	if len(got) != 3 {
		t.Fatalf("stream labels = %v, want exactly 3 labels (app, namespace, cause)", got)
	}
	want := map[string]string{"app": "crashcause", "namespace": ns, "cause": cause}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("label %q = %q, want %q (all labels: %v)", k, got[k], v, got)
		}
	}
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestLokiNewValidatesURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"unparsable", "http://[::1]:namedport"},
		{"no scheme", "loki:3100"},
		{"no host", "http://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewLoki(LokiConfig{URL: tc.url}); err == nil {
				t.Fatalf("NewLoki(%q) = nil error, want error", tc.url)
			}
		})
	}
}

// TestLokiAcceptsFullPushURL pins the URL tolerance: the chart docs and the
// --loki-url help show the FULL push URL, so passing one must not produce a
// doubled .../push/loki/api/v1/push path (which 404s and drops every batch).
func TestLokiAcceptsFullPushURL(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	s, err := NewLoki(LokiConfig{URL: ts.srv.URL + lokiPushPath, BatchSize: 1})
	if err != nil {
		t.Fatalf("NewLoki: %v", err)
	}
	defer lokiCloseSink(t, s)

	got := s.(*lokiSink).pushURL
	if !strings.HasSuffix(got, lokiPushPath) || strings.Count(got, lokiPushPath) != 1 {
		t.Fatalf("pushURL = %q, want exactly one %q suffix", got, lokiPushPath)
	}

	d := lokiTestDiagnosis("prod", engine.CauseOOMKilled, time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC))
	if err := s.Emit(context.Background(), d); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	// lokiWaitPush asserts the request path is exactly lokiPushPath.
	lokiWaitPush(t, ts, 3*time.Second)
}

func TestLokiName(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	// Zero config exercises the documented defaults (10 / 2s / 10s).
	s := lokiNewSink(t, ts, LokiConfig{})
	defer lokiCloseSink(t, s)

	if got := s.Name(); got != "loki" {
		t.Fatalf("Name() = %q, want %q", got, "loki")
	}
	if got := s.Dropped(); got != 0 {
		t.Fatalf("Dropped() = %d, want 0", got)
	}
	if err := s.LastError(); err != nil {
		t.Fatalf("LastError() = %v, want nil", err)
	}
}

func TestLokiHappyPathSinglePush(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	s := lokiNewSink(t, ts, LokiConfig{BatchSize: 3, BatchWait: 30 * time.Second, TenantID: "tenant-a"})
	defer lokiCloseSink(t, s)

	base := time.Date(2026, 8, 2, 12, 0, 0, 123456789, time.UTC)
	summary := "the container's heap grew past the limit"

	for i := 0; i < 3; i++ {
		d := lokiTestDiagnosis("prod", engine.CauseOOMKilled, base.Add(time.Duration(i)*time.Second))
		if i == 2 {
			d.AISummary = &summary
		}
		if err := s.Emit(context.Background(), d); err != nil {
			t.Fatalf("Emit(%d): %v", i, err)
		}
	}

	c := lokiWaitPush(t, ts, 3*time.Second)
	if c.tenant != "tenant-a" {
		t.Errorf("X-Scope-OrgID = %q, want %q", c.tenant, "tenant-a")
	}
	if len(c.push.Streams) != 1 {
		t.Fatalf("streams = %d, want 1: %s", len(c.push.Streams), c.raw)
	}
	st := c.push.Streams[0]
	lokiAssertLabels(t, st.Stream, "prod", string(engine.CauseOOMKilled))
	if len(st.Values) != 3 {
		t.Fatalf("values = %d, want 3: %s", len(st.Values), c.raw)
	}

	for i, v := range st.Values {
		if len(v) != 2 {
			t.Fatalf("value %d has %d elements, want 2", i, len(v))
		}
		wantTS := strconv.FormatInt(base.Add(time.Duration(i)*time.Second).UnixNano(), 10)
		if v[0] != wantTS {
			t.Errorf("value %d timestamp = %q, want %q", i, v[0], wantTS)
		}
		if _, err := strconv.ParseInt(v[0], 10, 64); err != nil {
			t.Errorf("value %d timestamp %q is not a decimal integer string: %v", i, v[0], err)
		}
		var got engine.Diagnosis
		if err := json.Unmarshal([]byte(v[1]), &got); err != nil {
			t.Fatalf("value %d line does not unmarshal into a Diagnosis: %v (%q)", i, err, v[1])
		}
		if got.Cause != engine.CauseOOMKilled || got.Pod != "api-7c9f5b6d4-abcde" || got.Namespace != "prod" {
			t.Errorf("value %d decoded = %+v, want the emitted diagnosis", i, got)
		}
		if i == 2 {
			if got.AISummary == nil || *got.AISummary != summary {
				t.Errorf("value %d ai_summary = %v, want %q", i, got.AISummary, summary)
			}
		} else if got.AISummary != nil {
			t.Errorf("value %d ai_summary = %q, want absent", i, *got.AISummary)
		}
	}

	lokiExpectNoPush(t, ts, 150*time.Millisecond)
}

func TestLokiFlushesOnBatchSize(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	s := lokiNewSink(t, ts, LokiConfig{BatchSize: 3, BatchWait: 30 * time.Second})
	defer lokiCloseSink(t, s)

	now := time.Now()
	for i := 0; i < 2; i++ {
		if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseAppExitNonzero, now)); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	// Below BatchSize and far below BatchWait: nothing may be pushed yet.
	lokiExpectNoPush(t, ts, 300*time.Millisecond)

	if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseAppExitNonzero, now)); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	c := lokiWaitPush(t, ts, 2*time.Second)
	if len(c.push.Streams) != 1 || len(c.push.Streams[0].Values) != 3 {
		t.Fatalf("want 1 stream with 3 values, got %s", c.raw)
	}
}

func TestLokiFlushesOnBatchWait(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	s := lokiNewSink(t, ts, LokiConfig{BatchSize: 100, BatchWait: 50 * time.Millisecond})
	defer lokiCloseSink(t, s)

	if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseEvicted, time.Now())); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	c := lokiWaitPush(t, ts, 2*time.Second)
	if len(c.push.Streams) != 1 || len(c.push.Streams[0].Values) != 1 {
		t.Fatalf("want 1 stream with 1 value, got %s", c.raw)
	}
	lokiAssertLabels(t, c.push.Streams[0].Stream, "prod", string(engine.CauseEvicted))
}

func TestLokiGroupsStreamsByNamespaceAndCause(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	s := lokiNewSink(t, ts, LokiConfig{BatchSize: 3, BatchWait: 30 * time.Second})
	defer lokiCloseSink(t, s)

	now := time.Now()
	emit := func(ns string, cause engine.CauseCode) {
		t.Helper()
		if err := s.Emit(context.Background(), lokiTestDiagnosis(ns, cause, now)); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	emit("prod", engine.CauseOOMKilled)
	emit("staging", engine.CauseAppExitNonzero)
	emit("prod", engine.CauseOOMKilled)

	c := lokiWaitPush(t, ts, 3*time.Second)
	if len(c.push.Streams) != 2 {
		t.Fatalf("streams = %d, want 2 (one per namespace/cause pair): %s", len(c.push.Streams), c.raw)
	}

	byNS := map[string]lokiWireStream{}
	for _, st := range c.push.Streams {
		byNS[st.Stream["namespace"]] = st
	}
	prod, ok := byNS["prod"]
	if !ok {
		t.Fatalf("missing prod stream: %s", c.raw)
	}
	lokiAssertLabels(t, prod.Stream, "prod", string(engine.CauseOOMKilled))
	if len(prod.Values) != 2 {
		t.Errorf("prod values = %d, want 2", len(prod.Values))
	}
	staging, ok := byNS["staging"]
	if !ok {
		t.Fatalf("missing staging stream: %s", c.raw)
	}
	lokiAssertLabels(t, staging.Stream, "staging", string(engine.CauseAppExitNonzero))
	if len(staging.Values) != 1 {
		t.Errorf("staging values = %d, want 1", len(staging.Values))
	}
}

func TestLokiRetriesOnceThenDropsBatch(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	ts.status.Store(http.StatusInternalServerError)

	s := lokiNewSink(t, ts, LokiConfig{BatchSize: 2, BatchWait: 30 * time.Second, Timeout: 2 * time.Second})
	defer lokiCloseSink(t, s)

	now := time.Now()
	for i := 0; i < 2; i++ {
		if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseOOMKilled, now)); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	lokiWaitPush(t, ts, 3*time.Second) // initial attempt
	lokiWaitPush(t, ts, 3*time.Second) // single retry

	// No third attempt: exactly one retry, then the batch is given up on.
	lokiExpectNoPush(t, ts, 600*time.Millisecond)
	if got := ts.count.Load(); got != 2 {
		t.Fatalf("requests = %d, want exactly 2 (initial + one retry)", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	for s.Dropped() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := s.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d, want 2 (the whole failed batch)", got)
	}
	if s.LastError() == nil {
		t.Error("LastError() = nil, want the push failure")
	}

	// The sink must stay usable after a failed batch.
	ts.status.Store(http.StatusNoContent)
	for i := 0; i < 2; i++ {
		if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseOOMKilled, now)); err != nil {
			t.Fatalf("Emit after failure: %v", err)
		}
	}
	c := lokiWaitPush(t, ts, 3*time.Second)
	if len(c.push.Streams) != 1 || len(c.push.Streams[0].Values) != 2 {
		t.Fatalf("want 1 stream with 2 values after recovery, got %s", c.raw)
	}
	if got := s.Dropped(); got != 2 {
		t.Fatalf("Dropped() = %d after a successful batch, want it to stay 2", got)
	}
}

func TestLokiEmitNeverBlocksAndDropsOnFullBuffer(t *testing.T) {
	t.Parallel()

	gate := make(chan struct{})
	ts := lokiNewTestServer(t, gate)
	// BatchSize 10 -> buffer capacity max(100, 100) = 100.
	s := lokiNewSink(t, ts, LokiConfig{BatchSize: 10, BatchWait: 30 * time.Second, Timeout: 10 * time.Second})

	// Wedge the flusher inside the first push, then flood the buffer.
	if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseOOMKilled, time.Now())); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// The property under test is that Emit returns instead of waiting for the
	// wedged flusher — which stays wedged for the rest of the test, so a
	// blocking Emit would hang indefinitely. Assert that with a watchdog on
	// the whole batch rather than a per-call latency bound: a non-blocking
	// channel send can still be descheduled for 100ms+ when the race detector
	// and a parallel package run saturate every core, which made the old
	// per-call assertion flaky (and, because it fired before close(gate), it
	// deadlocked cleanup and turned a failure into a 10-minute timeout).
	const total = 1000
	emitted := make(chan error, 1)
	go func() {
		for i := 0; i < total; i++ {
			if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseOOMKilled, time.Now())); err != nil {
				emitted <- fmt.Errorf("Emit(%d): %w", i, err)
				return
			}
		}
		emitted <- nil
	}()

	select {
	case err := <-emitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		// Comfortably beyond any scheduler stall, and far below the 10s
		// push Timeout x the number of wedged pushes a blocking Emit implies.
		t.Fatalf("Emit blocked: %d emits did not finish while the flusher was wedged", total)
	}
	if got := s.Dropped(); got == 0 {
		t.Fatalf("Dropped() = 0, want > 0 after emitting %d entries into a 100-slot buffer", total)
	}

	close(gate) // release the wedged push so Close can drain quickly
	lokiCloseSink(t, s)

	// Emit after Close must neither panic nor block.
	if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseOOMKilled, time.Now())); err != nil {
		t.Fatalf("Emit after Close: %v", err)
	}
}

func TestLokiCloseFlushesPendingAndIsIdempotent(t *testing.T) {
	t.Parallel()

	ts := lokiNewTestServer(t, nil)
	// Neither BatchSize nor BatchWait can fire: only Close can flush this.
	s := lokiNewSink(t, ts, LokiConfig{BatchSize: 500, BatchWait: time.Hour})

	now := time.Now()
	if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseUnschedulable, now)); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	c := lokiWaitPush(t, ts, 2*time.Second)
	if len(c.push.Streams) != 1 || len(c.push.Streams[0].Values) != 1 {
		t.Fatalf("want the pending entry flushed on Close, got %s", c.raw)
	}
	lokiAssertLabels(t, c.push.Streams[0].Stream, "prod", string(engine.CauseUnschedulable))

	// Double Close must be safe and must not push again.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if err := s.Close(ctx2); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	lokiExpectNoPush(t, ts, 150*time.Millisecond)
}

func TestLokiAuthFromEnvironment(t *testing.T) {
	// No t.Parallel(): t.Setenv forbids it.

	emitOne := func(t *testing.T, s Sink) {
		t.Helper()
		if err := s.Emit(context.Background(), lokiTestDiagnosis("prod", engine.CauseOOMKilled, time.Now())); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	t.Run("bearer token", func(t *testing.T) {
		t.Setenv(lokiEnvBearerToken, "s3cr3t-token")
		ts := lokiNewTestServer(t, nil)
		s := lokiNewSink(t, ts, LokiConfig{BatchSize: 1, BatchWait: 50 * time.Millisecond})
		defer lokiCloseSink(t, s)

		emitOne(t, s)
		c := lokiWaitPush(t, ts, 2*time.Second)
		if c.authz != "Bearer s3cr3t-token" {
			t.Fatalf("Authorization = %q, want %q", c.authz, "Bearer s3cr3t-token")
		}
	})

	t.Run("basic auth", func(t *testing.T) {
		t.Setenv(lokiEnvUsername, "loki-user")
		t.Setenv(lokiEnvPassword, "loki-pass")
		ts := lokiNewTestServer(t, nil)
		s := lokiNewSink(t, ts, LokiConfig{BatchSize: 1, BatchWait: 50 * time.Millisecond})
		defer lokiCloseSink(t, s)

		emitOne(t, s)
		c := lokiWaitPush(t, ts, 2*time.Second)
		if !c.basicOK {
			t.Fatalf("basic auth not used; Authorization = %q", c.authz)
		}
		if c.basicUser != "loki-user" || c.basicPass != "loki-pass" {
			t.Fatalf("basic auth = %q/%q, want loki-user/loki-pass", c.basicUser, c.basicPass)
		}
	})

	t.Run("bearer wins over basic", func(t *testing.T) {
		t.Setenv(lokiEnvBearerToken, "winning-token")
		t.Setenv(lokiEnvUsername, "loki-user")
		t.Setenv(lokiEnvPassword, "loki-pass")
		ts := lokiNewTestServer(t, nil)
		s := lokiNewSink(t, ts, LokiConfig{BatchSize: 1, BatchWait: 50 * time.Millisecond})
		defer lokiCloseSink(t, s)

		emitOne(t, s)
		c := lokiWaitPush(t, ts, 2*time.Second)
		if c.authz != "Bearer winning-token" {
			t.Fatalf("Authorization = %q, want the bearer token to win", c.authz)
		}
		if c.basicOK {
			t.Fatal("basic auth was used even though LOKI_BEARER_TOKEN was set")
		}
	})

	t.Run("no credentials", func(t *testing.T) {
		t.Setenv(lokiEnvBearerToken, "")
		t.Setenv(lokiEnvUsername, "")
		t.Setenv(lokiEnvPassword, "")
		ts := lokiNewTestServer(t, nil)
		s := lokiNewSink(t, ts, LokiConfig{BatchSize: 1, BatchWait: 50 * time.Millisecond})
		defer lokiCloseSink(t, s)

		emitOne(t, s)
		c := lokiWaitPush(t, ts, 2*time.Second)
		if c.authz != "" {
			t.Fatalf("Authorization = %q, want no header when no credentials are set", c.authz)
		}
	})
}
