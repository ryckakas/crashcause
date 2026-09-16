package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
)

// exitCode / exitCodeFor are the only mechanism by which a mode's exit code
// (0 diagnosed, 2 nothing-to-diagnose, 1 error) survives cobra's error-only
// RunE signature, so the mapping is asserted directly.

func TestExitCodeWrapsOnlyNonZero(t *testing.T) {
	if err := exitCode(0); err != nil {
		t.Fatalf("exitCode(0) = %v, want nil", err)
	}
	for _, code := range []int{1, 2} {
		err := exitCode(code)
		var coded exitCodeError
		if !errors.As(err, &coded) {
			t.Fatalf("exitCode(%d) = %v, want an exitCodeError", code, err)
		}
		if coded.code != code {
			t.Errorf("exitCode(%d) carried code %d", code, coded.code)
		}
	}
}

func TestExitCodeForMapping(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCode  int
		wantPrint string
	}{
		{name: "success", err: nil, wantCode: 0},
		{name: "nothing to diagnose is passed through silently", err: exitCode(2), wantCode: 2},
		{name: "reported error is passed through silently", err: exitCode(1), wantCode: 1},
		{
			name:      "wrapped exit code still unwraps",
			err:       fmt.Errorf("running inspect: %w", exitCode(2)),
			wantCode:  2,
			wantPrint: "",
		},
		{
			name:      "unreported error is printed and becomes exit 1",
			err:       errors.New("loading kubeconfig: no configuration"),
			wantCode:  1,
			wantPrint: "error: loading kubeconfig: no configuration",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stderr bytes.Buffer
			got := exitCodeFor(tc.err, &stderr)
			if got != tc.wantCode {
				t.Errorf("exitCodeFor() = %d, want %d", got, tc.wantCode)
			}
			out := stderr.String()
			switch {
			case tc.wantPrint == "" && out != "":
				t.Errorf("stderr = %q, want nothing printed", out)
			case tc.wantPrint != "" && !strings.Contains(out, tc.wantPrint):
				t.Errorf("stderr = %q, want it to contain %q", out, tc.wantPrint)
			}
		})
	}
}

func kubeconfigFile(t *testing.T, namespace string) string {
	t.Helper()
	nsLine := ""
	if namespace != "" {
		nsLine = "\n    namespace: " + namespace
	}
	content := `apiVersion: v1
kind: Config
clusters:
- name: test-cluster
  cluster:
    server: https://kubernetes.invalid:6443
contexts:
- name: test-context
  context:
    cluster: test-cluster
    user: test-user` + nsLine + `
current-context: test-context
users:
- name: test-user
  user: {}
`
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

func TestResolveNamespacePrecedence(t *testing.T) {
	cases := []struct {
		name        string
		flag        string
		contextNS   string
		wantNS      string
		description string
	}{
		{name: "flag wins", flag: "flag-ns", contextNS: "context-ns", wantNS: "flag-ns"},
		{name: "context namespace is used when no flag", flag: "", contextNS: "context-ns", wantNS: "context-ns"},
		{name: "default when neither is set", flag: "", contextNS: "", wantNS: "default"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			flags := genericclioptions.NewConfigFlags(true)
			kubeconfig := kubeconfigFile(t, tc.contextNS)
			flags.KubeConfig = &kubeconfig
			flags.Namespace = &tc.flag

			if got := resolveNamespace(flags); got != tc.wantNS {
				t.Errorf("resolveNamespace() = %q, want %q", got, tc.wantNS)
			}
		})
	}
}

// The inspect command must expose exactly the flag names the spec and the
// kubectl-plugin conventions promise; renaming one silently breaks scripts.
func TestInspectCommandFlags(t *testing.T) {
	cmd := newInspectCmd()

	for _, name := range []string{
		"namespace", "container", "output", "previous-lines",
		"init-stuck-threshold", "verbose", "ai", "kubeconfig", "context", "cluster", "user",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("inspect is missing the --%s flag", name)
		}
	}
	if f := cmd.Flags().ShorthandLookup("n"); f == nil || f.Name != "namespace" {
		t.Error("inspect is missing the -n shorthand for --namespace")
	}
	if f := cmd.Flags().ShorthandLookup("c"); f == nil || f.Name != "container" {
		t.Error("inspect is missing the -c shorthand for --container")
	}
}
