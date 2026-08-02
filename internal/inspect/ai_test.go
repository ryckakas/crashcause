package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ryckakas/crashcause/internal/ai"
)

// --- 6. AI success path -------------------------------------------------------

func TestRunAISuccess(t *testing.T) {
	now := time.Now()
	cs := newClient(t, appExitPod(t, now))

	const cannedSummary = "The process exited on its own with a stack trace; check the deployment's config."
	provider := &fakeProvider{summary: cannedSummary}
	summarizer := ai.NewSummarizer(provider, nil)

	t.Run("json has ai_summary", func(t *testing.T) {
		var out, errOut bytes.Buffer
		opts := Options{
			Namespace:  testNamespace,
			Pod:        testPodName,
			Output:     OutputJSON,
			Summarizer: summarizer,
			Out:        &out,
			ErrOut:     &errOut,
		}
		code := Run(context.Background(), cs, opts)
		if code != ExitDiagnosed {
			t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
		}
		var report Report
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if len(report.Diagnoses) != 1 {
			t.Fatalf("got %d diagnoses, want 1: %+v", len(report.Diagnoses), report.Diagnoses)
		}
		d := report.Diagnoses[0]
		if d.AISummary == nil || *d.AISummary != cannedSummary {
			t.Errorf("AISummary = %v, want %q", d.AISummary, cannedSummary)
		}
	})

	t.Run("human shows the AI summary banner", func(t *testing.T) {
		var out, errOut bytes.Buffer
		opts := Options{
			Namespace:  testNamespace,
			Pod:        testPodName,
			Summarizer: summarizer,
			Out:        &out,
			ErrOut:     &errOut,
		}
		code := Run(context.Background(), cs, opts)
		if code != ExitDiagnosed {
			t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
		}
		got := out.String()
		if !strings.Contains(got, "AI summary (generated):") {
			t.Errorf("stdout missing the AI summary banner:\n%s", got)
		}
		if !strings.Contains(got, cannedSummary) {
			t.Errorf("stdout missing the canned summary text:\n%s", got)
		}
	})

	t.Run("provider received the container's cause and image", func(t *testing.T) {
		if len(provider.calls) == 0 {
			t.Fatal("provider was never called")
		}
		req := provider.calls[len(provider.calls)-1]
		if req.Cause != "app_exit_nonzero" {
			t.Errorf("Request.Cause = %q, want %q", req.Cause, "app_exit_nonzero")
		}
		if req.Image != testImage {
			t.Errorf("Request.Image = %q, want %q", req.Image, testImage)
		}
	})
}

// --- 7. AI failure path -------------------------------------------------------

func TestRunAIFailure(t *testing.T) {
	now := time.Now()
	cs := newClient(t, appExitPod(t, now))

	provider := &fakeProvider{err: errors.New("provider unreachable")}
	summarizer := ai.NewSummarizer(provider, nil)

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace:  testNamespace,
		Pod:        testPodName,
		Summarizer: summarizer,
		Out:        &out,
		ErrOut:     &errOut,
	}
	code := Run(context.Background(), cs, opts)

	if code != ExitDiagnosed {
		t.Fatalf("exit code = %d, want %d (AI failure must not change the exit code); stderr=%q", code, ExitDiagnosed, errOut.String())
	}
	if !strings.Contains(out.String(), "app_exit_nonzero") {
		t.Errorf("stdout must still render the diagnosis:\n%s", out.String())
	}

	errLines := splitNonEmptyLines(errOut.String())
	if len(errLines) != 1 {
		t.Fatalf("stderr has %d lines, want exactly 1: %q", len(errLines), errOut.String())
	}
	if !strings.HasPrefix(errLines[0], "AI summary unavailable:") {
		t.Errorf("stderr line = %q, want it to start with %q", errLines[0], "AI summary unavailable:")
	}
}

// --- 8. AI gate closed for non-eligible causes -------------------------------

func TestRunAIGateClosedForOOM(t *testing.T) {
	now := time.Now()
	cs := newClient(t, oomKilledPod(t, now))

	provider := &fakeProvider{failIfCalled: t}
	summarizer := ai.NewSummarizer(provider, nil)

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace:  testNamespace,
		Pod:        testPodName,
		Output:     OutputJSON,
		Summarizer: summarizer,
		Out:        &out,
		ErrOut:     &errOut,
	}
	code := Run(context.Background(), cs, opts)

	if code != ExitDiagnosed {
		t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
	}
	if len(provider.calls) != 0 {
		t.Errorf("provider was called %d times, want 0 (oom_killed is not AI-eligible)", len(provider.calls))
	}

	var report Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(report.Diagnoses) != 1 {
		t.Fatalf("got %d diagnoses, want 1: %+v", len(report.Diagnoses), report.Diagnoses)
	}
	if report.Diagnoses[0].AISummary != nil {
		t.Errorf("AISummary = %v, want nil", *report.Diagnoses[0].AISummary)
	}
	// ai_summary is a nullable field per the documented JSON contract: it must
	// stay present (as null), not vanish, when no summary was generated.
	if !strings.Contains(out.String(), `"ai_summary": null`) {
		t.Errorf("stdout missing the nullable ai_summary field:\n%s", out.String())
	}
}

// splitNonEmptyLines splits s on newlines and drops empty trailing/leading
// lines, so a buffer ending in "\n" counts as one line, not two.
func splitNonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
