package inspect

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ryckakas/crashcause/internal/engine"
)

// header is the exact separator the human renderer puts between the "<ns>/<pod>
// container <name>" identity and the cause: see renderContainer in render.go.
const headerSep = " — "

func TestRunOOMKilledPodHuman(t *testing.T) {
	now := time.Now()
	cs := newClient(t, oomKilledPod(t, now))

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace: testNamespace,
		Pod:       testPodName,
		Out:       &out,
		ErrOut:    &errOut,
	}

	code := Run(context.Background(), cs, opts)

	if code != ExitDiagnosed {
		t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
	}
	got := out.String()

	if !strings.Contains(got, "oom_killed") {
		t.Errorf("stdout missing %q:\n%s", "oom_killed", got)
	}
	wantHeader := testNamespace + "/" + testPodName + " container " + testContainer + headerSep
	if !strings.Contains(got, wantHeader) {
		t.Errorf("stdout missing header %q:\n%s", wantHeader, got)
	}
	if !strings.Contains(got, "Evidence:") {
		t.Errorf("stdout missing an Evidence: section:\n%s", got)
	}
	if !strings.Contains(got, "Next steps:") {
		t.Errorf("stdout missing a Next steps: section:\n%s", got)
	}
	if errOut.String() != "" {
		t.Errorf("stderr = %q, want empty", errOut.String())
	}
}

func TestRunOOMKilledPodJSON(t *testing.T) {
	now := time.Now()
	cs := newClient(t, oomKilledPod(t, now))

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace: testNamespace,
		Pod:       testPodName,
		Output:    OutputJSON,
		Out:       &out,
		ErrOut:    &errOut,
	}

	code := Run(context.Background(), cs, opts)

	if code != ExitDiagnosed {
		t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
	}

	var report Report
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("stdout did not unmarshal as a Report: %v\nstdout=%s", err, out.String())
	}
	if report.Pod != testPodName {
		t.Errorf("Pod = %q, want %q", report.Pod, testPodName)
	}
	if report.Namespace != testNamespace {
		t.Errorf("Namespace = %q, want %q", report.Namespace, testNamespace)
	}
	if len(report.Diagnoses) != 1 {
		t.Fatalf("got %d diagnoses, want 1: %+v", len(report.Diagnoses), report.Diagnoses)
	}
	if report.Diagnoses[0].Cause != engine.CauseOOMKilled {
		t.Errorf("Cause = %q, want %q", report.Diagnoses[0].Cause, engine.CauseOOMKilled)
	}
	if strings.Contains(out.String(), "Evidence:") {
		t.Errorf("JSON stdout must contain no human prose, found 'Evidence:':\n%s", out.String())
	}
}

// Fixture (probeFailurePod): a currently-Running app container with a
// harmless exit-0 LAST termination (RestartCount>0, RestartPolicy OnFailure
// so completed_restart_loop does not also fire — see the fixture's comment
// for why the collector needs *some* terminated state to even hand the
// container to the engine), plus two Unhealthy events — one mentioning
// "Liveness", one mentioning "Startup" — and one "Killing" event.
//
// Per internal/engine/rules.go:
//   - probeLivenessRule matches on livenessProbeContext: Unhealthy+"Liveness"
//     events non-empty, and (Killing events non-empty OR RestartCount>0).
//   - probeStartupRule matches on the equivalent startupProbeContext.
//   - Neither rule excludes the other (unlike sigkill_unattributed, which
//     probeKillMatched suppresses whenever a probe kill is attributable — the
//     rules-doc-suggested "exit 137 + liveness Unhealthy/Killing" combination
//     does NOT yield two diagnoses because that combination makes
//     probeKillMatched true, which is exactly the condition that turns off
//     sigkillUnattributedRule. It falls back to a single diagnosis:
//     probe_liveness_failure.)
//   - oom_killed, sigkill_unattributed and app_exit_nonzero all decline
//     (exit code 0, no OOMKilled reason, no signal 9); sigkill_after_grace
//     declines (pod is not Deleting); completed_restart_loop declines
//     (RestartPolicy is OnFailure, not Always).
//
// This yields exactly two diagnoses: probe_liveness_failure and
// probe_startup_failure, both ConfidenceHigh (a Killing event is present).

func TestRunVerboseSecondaryMatches(t *testing.T) {
	now := time.Now()
	pod := probeFailurePod(t, now)
	liveness := podEvent("ev-liveness", "Warning", "Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500", 5, now.Add(-3*time.Minute))
	startup := podEvent("ev-startup", "Warning", "Unhealthy", "Startup probe failed: dial tcp timeout", 5, now.Add(-4*time.Minute))
	killing := podEvent("ev-killing", "Normal", "Killing", "Container app failed liveness probe, will be restarted", 1, now.Add(-2*time.Minute))
	cs := newClient(t, pod, liveness, startup, killing)

	baseOpts := func(out, errOut *bytes.Buffer) Options {
		return Options{
			Namespace: testNamespace,
			Pod:       testPodName,
			Out:       out,
			ErrOut:    errOut,
		}
	}

	t.Run("non-verbose human has no also-matched line", func(t *testing.T) {
		var out, errOut bytes.Buffer
		code := Run(context.Background(), cs, baseOpts(&out, &errOut))
		if code != ExitDiagnosed {
			t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
		}
		if strings.Contains(out.String(), "also matched:") {
			t.Errorf("non-verbose stdout must not contain 'also matched:':\n%s", out.String())
		}
	})

	t.Run("verbose human has exactly one also-matched line", func(t *testing.T) {
		var out, errOut bytes.Buffer
		opts := baseOpts(&out, &errOut)
		opts.Verbose = true
		code := Run(context.Background(), cs, opts)
		if code != ExitDiagnosed {
			t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
		}
		got := out.String()
		n := strings.Count(got, "also matched:")
		if n != 1 {
			t.Errorf("stdout contains %d 'also matched:' lines, want exactly 1:\n%s", n, got)
		}
	})

	var nonVerboseCount, verboseCount int

	t.Run("non-verbose JSON has one diagnosis", func(t *testing.T) {
		var out, errOut bytes.Buffer
		opts := baseOpts(&out, &errOut)
		opts.Output = OutputJSON
		if code := Run(context.Background(), cs, opts); code != ExitDiagnosed {
			t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
		}
		var report Report
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if len(report.Diagnoses) != 1 {
			t.Fatalf("non-verbose JSON has %d diagnoses, want 1: %+v", len(report.Diagnoses), report.Diagnoses)
		}
		nonVerboseCount = len(report.Diagnoses)
	})

	t.Run("verbose JSON has both probe diagnoses", func(t *testing.T) {
		var out, errOut bytes.Buffer
		opts := baseOpts(&out, &errOut)
		opts.Output = OutputJSON
		opts.Verbose = true
		if code := Run(context.Background(), cs, opts); code != ExitDiagnosed {
			t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
		}
		var report Report
		if err := json.Unmarshal(out.Bytes(), &report); err != nil {
			t.Fatalf("json unmarshal: %v", err)
		}
		if len(report.Diagnoses) != 2 {
			t.Fatalf("verbose JSON has %d diagnoses, want 2: %+v", len(report.Diagnoses), report.Diagnoses)
		}
		verboseCount = len(report.Diagnoses)
		causes := map[engine.CauseCode]bool{}
		for _, d := range report.Diagnoses {
			causes[d.Cause] = true
		}
		if !causes[engine.CauseProbeLiveness] || !causes[engine.CauseProbeStartup] {
			t.Errorf("verbose diagnoses = %+v, want probe_liveness_failure and probe_startup_failure", report.Diagnoses)
		}
	})

	if nonVerboseCount == 0 || verboseCount == 0 {
		t.Fatal("subtests above must have run and set both counts")
	}
	if verboseCount <= nonVerboseCount {
		t.Errorf("verbose diagnoses (%d) not strictly greater than non-verbose (%d)", verboseCount, nonVerboseCount)
	}
}

func TestRunMultiContainerBothCrashing(t *testing.T) {
	now := time.Now()
	cs := newClient(t, multiContainerCrashedPod(t, now))

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace: testNamespace,
		Pod:       testPodName,
		Out:       &out,
		ErrOut:    &errOut,
	}
	code := Run(context.Background(), cs, opts)
	if code != ExitDiagnosed {
		t.Fatalf("exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, testContainer) {
		t.Errorf("stdout missing container %q:\n%s", testContainer, got)
	}
	if !strings.Contains(got, testContainer2) {
		t.Errorf("stdout missing container %q:\n%s", testContainer2, got)
	}

	var jsonOut, jsonErr bytes.Buffer
	jsonOpts := opts
	jsonOpts.Output = OutputJSON
	jsonOpts.Out = &jsonOut
	jsonOpts.ErrOut = &jsonErr
	if code := Run(context.Background(), cs, jsonOpts); code != ExitDiagnosed {
		t.Fatalf("json exit code = %d, want %d; stderr=%q", code, ExitDiagnosed, jsonErr.String())
	}
	var report Report
	if err := json.Unmarshal(jsonOut.Bytes(), &report); err != nil {
		t.Fatalf("json unmarshal: %v", err)
	}
	if len(report.Diagnoses) != 2 {
		t.Fatalf("got %d diagnoses, want 2: %+v", len(report.Diagnoses), report.Diagnoses)
	}
}
