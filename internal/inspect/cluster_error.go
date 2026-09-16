package inspect

import (
	"errors"
	"fmt"
	"net"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// clusterAdvice is the one action that fixes every failure in this bucket: the
// kubeconfig context in use is stale, wrong, or missing working credentials.
const clusterAdvice = "check your kubectl context and kubeconfig credentials (kubectl config current-context)"

// clusterFailureMarkers maps a marker in a client-go error chain onto the reason
// shown to the user. client-go reports these as plain wrapped errors (there is
// no typed error to match on), so substrings are the only handle available. The
// credential markers come first because they usually wrap a transport error.
var clusterFailureMarkers = []struct {
	marker string
	reason string
}{
	{"getting credentials", "the kubeconfig credentials could not be obtained"},
	{"exec: executable", "the kubeconfig credential plugin failed"},
	{"no such host", "the server address could not be resolved"},
	{"connection refused", "the connection was refused"},
	{"x509:", "the server certificate could not be verified"},
	{"context deadline exceeded", "the connection timed out"},
	{"i/o timeout", "the connection timed out"},
}

// explainClusterFailure rewrites "could not reach or authenticate to the API
// server" errors into a line an operator can act on, naming the server when it
// is known. Anything else — a missing pod, a bad option, an API error unrelated
// to connectivity — is returned untouched.
//
// The original error stays wrapped: the raw client-go detail is the only thing
// that distinguishes, say, one broken credential plugin from another.
func explainClusterFailure(err error, server string) error {
	reason, ok := clusterFailureReason(err)
	if !ok {
		return err
	}
	subject := "the cluster"
	if server != "" {
		subject = "cluster " + server
	}
	return fmt.Errorf("cannot reach or authenticate to %s: %s — %s: %w", subject, reason, clusterAdvice, err)
}

func clusterFailureReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	if apierrors.IsUnauthorized(err) {
		return "the server rejected the credentials (401 Unauthorized)", true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "the server address could not be resolved", true
	}

	msg := err.Error()
	for _, f := range clusterFailureMarkers {
		if strings.Contains(msg, f.marker) {
			return f.reason, true
		}
	}
	return "", false
}
