package inspect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

const testServer = "https://kubernetes.example.com"

// collectorWrapped mirrors how the collector reports a failed pod GET, so the
// classifier is exercised against the exact chain it sees in production.
func collectorWrapped(cause error) error {
	return fmt.Errorf("get pod %s/%s: %w", testNamespace, testPodName, cause)
}

// execPluginError reproduces the client-go chain for an expired credential
// plugin: a transport url.Error wrapping the exec plugin's own failure.
func execPluginError() error {
	return collectorWrapped(&url.Error{
		Op:  "Get",
		URL: testServer + "/api/v1/namespaces/prod/pods/web",
		Err: errors.New("getting credentials: exec: executable gke-gcloud-auth-plugin failed with exit code 1"),
	})
}

func TestExplainClusterFailure(t *testing.T) {
	cases := []struct {
		name         string
		err          error
		server       string
		wantReason   string
		wantVerbatim bool // the error must be returned untouched
	}{
		{
			name:       "credential plugin failure",
			err:        execPluginError(),
			server:     testServer,
			wantReason: "credentials could not be obtained",
		},
		{
			name:       "connection refused",
			err:        collectorWrapped(&url.Error{Op: "Get", URL: testServer, Err: errors.New("dial tcp 127.0.0.1:6443: connect: connection refused")}),
			server:     testServer,
			wantReason: "the connection was refused",
		},
		{
			name:       "unauthorized api error",
			err:        collectorWrapped(apierrors.NewUnauthorized("Unauthorized")),
			server:     testServer,
			wantReason: "401 Unauthorized",
		},
		{
			// A *net.DNSError whose text carries none of the known markers: only
			// the typed check can recognize this one.
			name:       "typed dns failure without a known marker",
			err:        collectorWrapped(&net.DNSError{Err: "server misbehaving", Name: "kubernetes.example.com"}),
			server:     testServer,
			wantReason: "server address could not be resolved",
		},
		{
			name:       "expired certificate",
			err:        collectorWrapped(errors.New(`Get "https://kubernetes.example.com": tls: failed to verify certificate: x509: certificate has expired`)),
			server:     testServer,
			wantReason: "server certificate could not be verified",
		},
		{
			name:       "no server known",
			err:        execPluginError(),
			server:     "",
			wantReason: "credentials could not be obtained",
		},
		{
			name:         "unrelated error passes through unchanged",
			err:          collectorWrapped(apierrors.NewInternalError(errors.New("etcd is having a bad day"))),
			server:       testServer,
			wantVerbatim: true,
		},
		{
			name:         "plain error passes through unchanged",
			err:          errors.New("invalid --output \"yaml\""),
			server:       testServer,
			wantVerbatim: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := explainClusterFailure(tc.err, tc.server)

			if tc.wantVerbatim {
				// Identity, not chain membership, is the assertion: an unrecognized
				// error must come back as-is, not wrapped in a misleading hint.
				if got != tc.err { //nolint:errorlint // see above
					t.Fatalf("explainClusterFailure() = %v, want the original error untouched", got)
				}
				return
			}

			msg := got.Error()
			if !strings.Contains(msg, "cannot reach or authenticate to") {
				t.Errorf("message = %q, want it to state the cluster could not be reached", msg)
			}
			if !strings.Contains(msg, tc.wantReason) {
				t.Errorf("message = %q, want it to contain reason %q", msg, tc.wantReason)
			}
			if !strings.Contains(msg, "kubectl config current-context") {
				t.Errorf("message = %q, want it to point at the kubectl context", msg)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("explainClusterFailure() dropped the original error from the chain")
			}
			if !strings.Contains(msg, tc.err.Error()) {
				t.Errorf("message = %q, want it to keep the underlying detail %q", msg, tc.err.Error())
			}

			wantServer := "cluster " + tc.server
			if tc.server == "" {
				wantServer = "the cluster"
			}
			if !strings.Contains(msg, wantServer) {
				t.Errorf("message = %q, want it to name %q", msg, wantServer)
			}
		})
	}
}

// A cluster we cannot authenticate to must still exit 1 (the documented error
// code) while printing the friendly line instead of the raw client-go chain.
func TestRunUnreachableClusterReportsHint(t *testing.T) {
	cs := newClient(t)
	cs.PrependReactor("get", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, execPluginError()
	})

	var out, errOut bytes.Buffer
	code := Run(context.Background(), cs, Options{
		Namespace: testNamespace,
		Pod:       testPodName,
		Server:    testServer,
		Out:       &out,
		ErrOut:    &errOut,
	})

	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	stderr := errOut.String()
	for _, want := range []string{
		"cannot reach or authenticate to cluster " + testServer,
		"kubectl config current-context",
		"gke-gcloud-auth-plugin",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr, want)
		}
	}
	if out.String() != "" {
		t.Errorf("stdout = %q, want empty", out.String())
	}
}
