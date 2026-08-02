package sinks

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ryckakas/crashcause/internal/engine"
)

// stdoutTestDiagnosis returns a fully-populated Diagnosis fixture.
func stdoutTestDiagnosis() engine.Diagnosis {
	return engine.Diagnosis{
		Cause:       engine.CauseOOMKilled,
		Confidence:  engine.ConfidenceHigh,
		Explanation: "Container was OOM-killed by the kernel (limit was 128Mi).",
		Evidence:    []string{"terminated.reason=OOMKilled", "exit code 137"},
		NextSteps:   []string{"kubectl describe pod api-7d9f -n prod"},
		Container:   "api",
		Pod:         "api-7d9f-abcde",
		Namespace:   "prod",
		Owner:       engine.Owner{Kind: "Deployment", Name: "api"},
		Timestamp:   time.Date(2026, 8, 2, 10, 30, 0, 0, time.UTC),
	}
}

func TestStdoutEmitWritesOneJSONObjectPerLine(t *testing.T) {
	var buf bytes.Buffer
	s := NewStdout(&buf)

	d := stdoutTestDiagnosis()
	if err := s.Emit(context.Background(), d); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if err := s.Emit(context.Background(), d); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), buf.String())
	}

	var got engine.Diagnosis
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("unmarshal line: %v (line=%q)", err, lines[0])
	}
	if got.Cause != d.Cause || got.Confidence != d.Confidence {
		t.Errorf("cause/confidence = %q/%q, want %q/%q", got.Cause, got.Confidence, d.Cause, d.Confidence)
	}
	if got.Pod != d.Pod || got.Namespace != d.Namespace || got.Container != d.Container {
		t.Errorf("identity = %q/%q/%q, want %q/%q/%q",
			got.Namespace, got.Pod, got.Container, d.Namespace, d.Pod, d.Container)
	}
	if got.Owner != d.Owner {
		t.Errorf("owner = %+v, want %+v", got.Owner, d.Owner)
	}
	if !got.Timestamp.Equal(d.Timestamp) {
		t.Errorf("timestamp = %v, want %v", got.Timestamp, d.Timestamp)
	}
	if len(got.Evidence) != 2 || len(got.NextSteps) != 1 {
		t.Errorf("evidence/next_steps = %v/%v", got.Evidence, got.NextSteps)
	}
}

func TestStdoutEmitTopLevelFieldsAndRFC3339(t *testing.T) {
	var buf bytes.Buffer
	if err := NewStdout(&buf).Emit(context.Background(), stdoutTestDiagnosis()); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{
		"cause", "confidence", "explanation", "evidence", "next_steps",
		"container", "pod", "namespace", "owner", "timestamp",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing top-level field %q in %s", key, buf.String())
		}
	}
	if _, ok := raw["ai_summary"]; ok {
		t.Errorf("ai_summary must be omitted when unset: %s", buf.String())
	}

	var ts string
	if err := json.Unmarshal(raw["timestamp"], &ts); err != nil {
		t.Fatalf("timestamp not a string: %v", err)
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", ts, err)
	}
}

func TestStdoutEmitIncludesAISummary(t *testing.T) {
	var buf bytes.Buffer
	d := stdoutTestDiagnosis()
	summary := "The process exhausted the heap while decoding a large payload."
	d.AISummary = &summary

	if err := NewStdout(&buf).Emit(context.Background(), d); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	var got engine.Diagnosis
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.AISummary == nil {
		t.Fatal("ai_summary missing")
	}
	if *got.AISummary != summary {
		t.Errorf("ai_summary = %q, want %q", *got.AISummary, summary)
	}
}

func TestStdoutEmitConcurrent(t *testing.T) {
	var buf bytes.Buffer
	s := NewStdout(&buf)

	const goroutines, perGoroutine = 16, 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			d := stdoutTestDiagnosis()
			for i := 0; i < perGoroutine; i++ {
				if err := s.Emit(context.Background(), d); err != nil {
					t.Errorf("Emit: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Fatalf("got %d lines, want %d", len(lines), goroutines*perGoroutine)
	}
	for i, line := range lines {
		var d engine.Diagnosis
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatalf("line %d is not valid JSON (interleaved write?): %v", i, err)
		}
	}
}

func TestStdoutNameAndClose(t *testing.T) {
	s := NewStdout(&bytes.Buffer{})
	if s.Name() != "stdout" {
		t.Errorf("Name() = %q, want %q", s.Name(), "stdout")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func TestStdoutEmitPropagatesWriterError(t *testing.T) {
	s := NewStdout(stdoutFailingWriter{})
	err := s.Emit(context.Background(), stdoutTestDiagnosis())
	if err == nil {
		t.Fatal("expected an error from a failing writer")
	}
	if !strings.Contains(err.Error(), "stdout") {
		t.Errorf("error should name the sink, got %v", err)
	}
}

type stdoutFailingWriter struct{}

func (stdoutFailingWriter) Write([]byte) (int, error) {
	return 0, errStdoutTestWrite
}

var errStdoutTestWrite = stdoutTestError("write failed")

type stdoutTestError string

func (e stdoutTestError) Error() string { return string(e) }
