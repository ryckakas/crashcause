package inspect

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestRunHealthyPodNothingToDiagnose(t *testing.T) {
	now := time.Now()

	t.Run("non-verbose", func(t *testing.T) {
		cs := newClient(t, healthyPod(t, now))
		var out, errOut bytes.Buffer
		opts := Options{
			Namespace: testNamespace,
			Pod:       testPodName,
			Out:       &out,
			ErrOut:    &errOut,
		}

		code := Run(context.Background(), cs, opts)

		if code != ExitNothingToDiagnose {
			t.Fatalf("exit code = %d, want %d", code, ExitNothingToDiagnose)
		}
		if !strings.Contains(out.String(), "nothing to diagnose") {
			t.Errorf("stdout = %q, want it to contain %q", out.String(), "nothing to diagnose")
		}
		if strings.Contains(out.String(), "checked:") {
			t.Errorf("stdout = %q, want no verbose 'checked:' line without --verbose", out.String())
		}
	})

	t.Run("verbose", func(t *testing.T) {
		cs := newClient(t, healthyPod(t, now))
		var out, errOut bytes.Buffer
		opts := Options{
			Namespace: testNamespace,
			Pod:       testPodName,
			Verbose:   true,
			Out:       &out,
			ErrOut:    &errOut,
		}

		code := Run(context.Background(), cs, opts)

		if code != ExitNothingToDiagnose {
			t.Fatalf("exit code = %d, want %d", code, ExitNothingToDiagnose)
		}
		if !strings.Contains(out.String(), "nothing to diagnose") {
			t.Errorf("stdout = %q, want it to contain %q", out.String(), "nothing to diagnose")
		}
		if !strings.Contains(out.String(), "checked:") {
			t.Errorf("stdout = %q, want a verbose 'checked:' line", out.String())
		}
	})
}

func TestRunMissingPod(t *testing.T) {
	cs := newClient(t)

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace: testNamespace,
		Pod:       "ghost",
		Out:       &out,
		ErrOut:    &errOut,
	}

	code := Run(context.Background(), cs, opts)

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(errOut.String(), "not found") {
		t.Errorf("stderr = %q, want it to contain %q", errOut.String(), "not found")
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestRunUnknownContainerFilter(t *testing.T) {
	now := time.Now()
	cs := newClient(t, oomKilledPod(t, now))

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace: testNamespace,
		Pod:       testPodName,
		Container: "ghost-container",
		Out:       &out,
		ErrOut:    &errOut,
	}

	code := Run(context.Background(), cs, opts)

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(errOut.String(), `"ghost-container"`) {
		t.Errorf("stderr = %q, want it to name the missing container", errOut.String())
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}

func TestRunInvalidOutput(t *testing.T) {
	now := time.Now()
	cs := newClient(t, oomKilledPod(t, now))

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace: testNamespace,
		Pod:       testPodName,
		Output:    "yaml",
		Out:       &out,
		ErrOut:    &errOut,
	}

	code := Run(context.Background(), cs, opts)

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	if !strings.Contains(errOut.String(), "yaml") {
		t.Errorf("stderr = %q, want it to mention the bad output value", errOut.String())
	}
}

func TestRunNoPodName(t *testing.T) {
	cs := newClient(t)

	var out, errOut bytes.Buffer
	opts := Options{
		Namespace: testNamespace,
		Out:       &out,
		ErrOut:    &errOut,
	}

	code := Run(context.Background(), cs, opts)

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}
