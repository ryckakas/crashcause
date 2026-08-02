package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ryckakas/crashcause/internal/engine"
)

// Defaults and tuning constants for the Loki sink (spec §7.3).
//
// lokiRetryBackoff is deliberately short and a constant rather than a knob:
// a crash-diagnosis stream is low volume, a failed push is not worth a long
// exponential ladder, and keeping it small keeps the unit tests fast.
const (
	lokiPushPath = "/loki/api/v1/push"

	lokiDefaultBatchSize = 10
	lokiDefaultBatchWait = 2 * time.Second
	lokiDefaultTimeout   = 10 * time.Second

	// lokiRetryBackoff is the pause between the initial push and the single
	// retry. There is exactly one retry: Loki being down must never turn into
	// unbounded memory growth or backpressure on the diagnosis pipeline.
	lokiRetryBackoff = 250 * time.Millisecond

	// lokiMinBufferCap is the floor for the internal entry buffer, so that a
	// tiny BatchSize still absorbs a reasonable burst of diagnoses.
	lokiMinBufferCap = 100

	// lokiBodySnippetMax bounds how much of an error response body is kept in
	// the held error, so a misconfigured endpoint returning an HTML page does
	// not blow up memory or log output.
	lokiBodySnippetMax = 256
)

// errLokiPushRejected is the sentinel behind every non-2xx push response, so
// callers can use errors.Is instead of string matching.
var errLokiPushRejected = errors.New("sinks: loki rejected push")

// Environment variables read once, at NewLoki time, for authentication.
//
// Credentials are taken from the environment ONLY — never from LokiConfig,
// never from CLI flags — so they cannot end up in shell history, in a
// serialised config, or in this process's own diagnostic output.
const (
	lokiEnvUsername    = "LOKI_USERNAME"
	lokiEnvPassword    = "LOKI_PASSWORD"
	lokiEnvBearerToken = "LOKI_BEARER_TOKEN"
)

// LokiConfig configures the Loki push sink.
//
// Authentication is NOT part of this struct on purpose. Credentials are read
// from the environment when NewLoki is called:
//
//	LOKI_BEARER_TOKEN         -> sent as "Authorization: Bearer <token>"
//	LOKI_USERNAME + LOKI_PASSWORD -> sent as HTTP basic auth
//
// If LOKI_BEARER_TOKEN is set (and non-empty) it WINS: the basic-auth
// variables are ignored entirely. Credential values are never logged, never
// echoed in errors, and never written to a Loki line.
type LokiConfig struct {
	// URL is the Loki base URL with no path, e.g. "http://loki:3100".
	// Required; the sink appends /loki/api/v1/push itself.
	URL string

	// TenantID, when non-empty, is sent as the X-Scope-OrgID header for
	// multi-tenant Loki deployments. Optional.
	TenantID string

	// BatchSize is the number of entries that triggers an immediate push.
	// Zero or negative falls back to 10.
	BatchSize int

	// BatchWait is how long a partially full batch waits before being pushed
	// anyway, measured from the first pending entry. Zero or negative falls
	// back to 2s.
	BatchWait time.Duration

	// Timeout bounds a single HTTP push (the retry gets its own budget).
	// Zero or negative falls back to 10s.
	Timeout time.Duration
}

// LokiSink is the concrete behaviour of the sink returned by NewLoki: the
// standard Sink contract plus observability into what the sink had to throw
// away. Callers that want the drop counter (watch mode surfaces it) can type
// assert the Sink returned by NewLoki to this interface.
type LokiSink interface {
	Sink

	// Dropped returns the total number of Diagnosis entries the sink threw
	// away: entries dropped because the internal buffer was full, plus every
	// entry in a batch that still failed after its single retry.
	Dropped() uint64

	// LastError returns the most recent push failure (status code plus a
	// truncated response snippet), or nil if no push has failed. It never
	// contains credentials.
	LastError() error
}

// lokiEntry is one already-marshalled log line plus the two label values that
// decide which Loki stream it belongs to. Marshalling happens in Emit so the
// background goroutine only does I/O, and so a malformed Diagnosis is caught
// on the caller's side.
type lokiEntry struct {
	namespace string
	cause     string
	timestamp string // Unix nanoseconds, decimal, as Loki requires
	line      string
}

// lokiStream is one element of the Loki push payload's "streams" array.
type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// lokiPushBody is the JSON document POSTed to /loki/api/v1/push.
type lokiPushBody struct {
	Streams []lokiStream `json:"streams"`
}

type lokiSink struct {
	pushURL   string
	tenantID  string
	batchSize int
	batchWait time.Duration
	timeout   time.Duration
	client    *http.Client

	// Credentials, resolved once at construction. Never logged.
	bearerToken string
	basicUser   string
	basicPass   string
	useBasic    bool

	ch   chan lokiEntry
	quit chan struct{}
	done chan struct{}

	// baseCtx is cancelled by Close when the caller's context expires, so an
	// in-flight push cannot outlive Close and leak the goroutine.
	baseCtx    context.Context
	baseCancel context.CancelFunc

	closeOnce sync.Once
	closeErr  error

	dropped atomic.Uint64

	errMu   sync.Mutex
	lastErr error
}

// NewLoki returns a Sink that batches diagnoses and pushes them to Loki's
// HTTP push API using plain net/http (no Loki SDK, no extra dependencies).
//
// Credentials are read from the environment at this moment, and only here:
//
//	LOKI_BEARER_TOKEN         -> "Authorization: Bearer <token>"
//	LOKI_USERNAME + LOKI_PASSWORD -> HTTP basic auth
//
// A non-empty LOKI_BEARER_TOKEN takes precedence over the basic-auth pair.
// Credential values are never logged or included in returned errors.
//
// It returns an error only for configuration that can never work (missing or
// unparsable URL). Everything after construction is best-effort: Loki being
// unreachable degrades into dropped entries counted by Dropped(), never into
// an error surfaced to the diagnosis pipeline.
func NewLoki(cfg LokiConfig) (Sink, error) {
	raw := strings.TrimSpace(cfg.URL)
	if raw == "" {
		return nil, fmt.Errorf("sinks: loki: %w", errors.New("URL is required"))
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("sinks: loki: parse URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("sinks: loki: parse URL %q: %w", raw,
			errors.New("scheme must be http or https"))
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("sinks: loki: parse URL %q: %w", raw,
			errors.New("missing host"))
	}

	batchSize := cfg.BatchSize
	if batchSize <= 0 {
		batchSize = lokiDefaultBatchSize
	}
	batchWait := cfg.BatchWait
	if batchWait <= 0 {
		batchWait = lokiDefaultBatchWait
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = lokiDefaultTimeout
	}

	// Buffer capacity: ten batches, with a floor. Big enough to ride out a
	// slow push, small enough that a dead Loki cannot grow memory without
	// bound — past this point Emit drops rather than blocks.
	bufCap := batchSize * 10
	if bufCap < lokiMinBufferCap {
		bufCap = lokiMinBufferCap
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())

	s := &lokiSink{
		pushURL:   strings.TrimRight(parsed.String(), "/") + lokiPushPath,
		tenantID:  strings.TrimSpace(cfg.TenantID),
		batchSize: batchSize,
		batchWait: batchWait,
		timeout:   timeout,
		client:    &http.Client{Timeout: timeout},

		ch:   make(chan lokiEntry, bufCap),
		quit: make(chan struct{}),
		done: make(chan struct{}),

		baseCtx:    baseCtx,
		baseCancel: baseCancel,
	}

	if token := strings.TrimSpace(os.Getenv(lokiEnvBearerToken)); token != "" {
		// Bearer wins over basic auth when both are configured.
		s.bearerToken = token
	} else if user := os.Getenv(lokiEnvUsername); user != "" {
		s.basicUser = user
		s.basicPass = os.Getenv(lokiEnvPassword)
		s.useBasic = true
	}

	go s.run()

	return s, nil
}

// Name identifies the sink in logs and error messages.
func (s *lokiSink) Name() string { return "loki" }

// Emit queues d for the next batch. It NEVER blocks: the Diagnosis is
// marshalled inline and handed to the background flusher through a buffered
// channel with a non-blocking send.
//
// If the buffer is full (Loki is slow or down), the entry is DROPPED and the
// Dropped() counter is incremented, and Emit still returns nil. That is
// deliberate: dropping is the designed backpressure behaviour of a telemetry
// sink, not a failure of the caller's diagnosis, and returning an error here
// would make the watch loop treat a Loki outage as a diagnosis error. Use
// Dropped() to surface the loss instead.
//
// ctx is unused because Emit performs no I/O and cannot block.
func (s *lokiSink) Emit(_ context.Context, d engine.Diagnosis) error {
	// After Close the sink stops accepting entries. Checking quit here also
	// means the entry channel is never closed, so a racing Emit can never
	// panic with "send on closed channel".
	select {
	case <-s.quit:
		s.dropped.Add(1)
		return nil
	default:
	}

	line, err := lokiMarshalLine(d)
	if err != nil {
		return fmt.Errorf("sinks: loki marshal diagnosis: %w", err)
	}

	ts := d.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}

	entry := lokiEntry{
		namespace: d.Namespace,
		cause:     string(d.Cause),
		timestamp: strconv.FormatInt(ts.UnixNano(), 10),
		line:      line,
	}

	select {
	case s.ch <- entry:
	case <-s.quit:
		s.dropped.Add(1)
	default:
		s.dropped.Add(1)
	}
	return nil
}

// Dropped returns the total number of entries lost: buffer overflows plus
// batches that failed even after their retry. It is race-free (atomic load)
// and safe to call concurrently with Emit and Close.
func (s *lokiSink) Dropped() uint64 { return s.dropped.Load() }

// LastError returns the most recent push failure, or nil. It carries the HTTP
// status and a truncated body snippet for diagnosis, never any credential.
func (s *lokiSink) LastError() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

// Close stops accepting entries, flushes whatever is still pending, and waits
// for the background goroutine to exit. It is idempotent (sync.Once) so a
// double Close neither panics nor double-flushes.
//
// If ctx expires before the drain finishes, Close cancels the in-flight push
// and returns the wrapped context error rather than hanging: a shutdown must
// never be held hostage by an unreachable Loki.
func (s *lokiSink) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		close(s.quit)
		select {
		case <-s.done:
			s.baseCancel()
		case <-ctx.Done():
			// Unblock the flusher's HTTP call so the goroutine terminates
			// instead of leaking, then report why we gave up.
			s.baseCancel()
			<-s.done
			s.closeErr = fmt.Errorf("sinks: loki close: %w", ctx.Err())
		}
	})
	return s.closeErr
}

// run is the single background goroutine: it accumulates entries and flushes
// them when the batch is full, when BatchWait has elapsed since the first
// pending entry, or when Close asks it to drain.
func (s *lokiSink) run() {
	defer close(s.done)

	pending := make([]lokiEntry, 0, s.batchSize)

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false

	disarm := func() {
		if armed && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		armed = false
	}

	add := func(e lokiEntry) {
		pending = append(pending, e)
		if len(pending) == 1 {
			// The deadline runs from the FIRST pending entry, so a partially
			// full batch is still delivered within BatchWait.
			timer.Reset(s.batchWait)
			armed = true
		}
		if len(pending) >= s.batchSize {
			disarm()
			s.flush(pending)
			pending = pending[:0]
		}
	}

	for {
		select {
		case e := <-s.ch:
			add(e)
		case <-timer.C:
			armed = false
			s.flush(pending)
			pending = pending[:0]
		case <-s.quit:
			drained := false
			for !drained {
				select {
				case e := <-s.ch:
					add(e)
				default:
					drained = true
				}
			}
			disarm()
			s.flush(pending)
			return
		}
	}
}

// flush pushes one batch, retrying exactly once after a short backoff. A batch
// that still fails is counted as dropped: the sink never blocks the pipeline
// and never grows without bound waiting for Loki to come back.
func (s *lokiSink) flush(batch []lokiEntry) {
	if len(batch) == 0 {
		return
	}

	body, err := lokiEncodeBatch(batch)
	if err != nil {
		s.dropBatch(batch, fmt.Errorf("sinks: loki encode batch: %w", err))
		return
	}

	if err := s.push(body); err != nil {
		select {
		case <-time.After(lokiRetryBackoff):
		case <-s.baseCtx.Done():
			s.dropBatch(batch, err)
			return
		}
		if retryErr := s.push(body); retryErr != nil {
			s.dropBatch(batch, retryErr)
		}
	}
}

func (s *lokiSink) dropBatch(batch []lokiEntry, err error) {
	s.dropped.Add(uint64(len(batch)))
	s.errMu.Lock()
	s.lastErr = err
	s.errMu.Unlock()
}

// push performs a single POST of an already-encoded payload.
func (s *lokiSink) push(body []byte) error {
	ctx, cancel := context.WithTimeout(s.baseCtx, s.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sinks: loki build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.tenantID != "" {
		req.Header.Set("X-Scope-OrgID", s.tenantID)
	}
	switch {
	case s.bearerToken != "":
		req.Header.Set("Authorization", "Bearer "+s.bearerToken)
	case s.useBasic:
		req.SetBasicAuth(s.basicUser, s.basicPass)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("sinks: loki push: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Keep a bounded snippet for diagnosis, then drain the rest so the
	// connection can be reused by the next push.
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, lokiBodySnippetMax))
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("sinks: loki push: %w: status %d: %s",
			errLokiPushRejected, resp.StatusCode, lokiSnippet(snippet))
	}
	return nil
}

// lokiEncodeBatch groups entries into one streams[] element per distinct
// (namespace, cause) pair, preserving emission order inside each stream.
//
// The label set is EXACTLY {app, namespace, cause} and nothing else. Loki
// indexes labels, so every additional label multiplies stream cardinality —
// pod names, confidence and AI text are high-cardinality and stay in the log
// line, where they are still searchable but not indexed.
func lokiEncodeBatch(batch []lokiEntry) ([]byte, error) {
	type key struct{ namespace, cause string }

	index := make(map[key]int, len(batch))
	payload := lokiPushBody{Streams: make([]lokiStream, 0, len(batch))}

	for _, e := range batch {
		k := key{namespace: e.namespace, cause: e.cause}
		i, ok := index[k]
		if !ok {
			i = len(payload.Streams)
			index[k] = i
			payload.Streams = append(payload.Streams, lokiStream{
				Stream: map[string]string{
					"app":       "crashcause",
					"namespace": e.namespace,
					"cause":     e.cause,
				},
			})
		}
		payload.Streams[i].Values = append(payload.Streams[i].Values, [2]string{e.timestamp, e.line})
	}

	out, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal push body: %w", err)
	}
	return out, nil
}

// lokiMarshalLine renders the full Diagnosis (including ai_summary when set)
// as the Loki log line. HTML escaping is off: explanations and log evidence
// routinely contain <, > and &, and escaping them only hurts readability in
// Grafana without making the JSON any more valid.
func lokiMarshalLine(d engine.Diagnosis) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return "", fmt.Errorf("encode diagnosis: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// lokiSnippet flattens an error-response snippet to a single readable line.
func lokiSnippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	if s == "" {
		return "<empty body>"
	}
	return s
}
