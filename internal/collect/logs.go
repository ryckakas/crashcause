package collect

import (
	"context"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// maxLogBytes caps how much of a container's log stream is ever read into
// memory, regardless of how long the requested lines turn out to be.
const maxLogBytes int64 = 1 << 20

// logTail fetches the tail of a container's log. It returns the lines and
// whether logs are unavailable; it never returns an error, because a log
// failure must never fail a diagnosis (logs are the most likely thing to be
// forbidden by RBAC, rate limited, or already garbage collected).
//
// previous selects the pre-restart log (kubectl logs --previous) and is set
// only when the container has a lastState.terminated to read from; a container
// that never started (CreateContainerConfigError), one that is terminated but
// has no previous incarnation, and a stuck-but-running init container all read
// the current log instead.
func (c *Collector) logTail(ctx context.Context, pod *corev1.Pod, container string, previous bool) (lines []string, unavailable bool) {
	if !c.opts.CollectLogs {
		return nil, true
	}
	if c.opts.LogRateLimiter != nil && !c.opts.LogRateLimiter.Allow() {
		c.logSkips.Add(1)
		return nil, true
	}

	tail := c.opts.PreviousLogLines
	req := c.client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tail,
		Previous:  previous,
	})
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, true
	}
	defer func() {
		_ = rc.Close()
	}()

	raw, err := io.ReadAll(io.LimitReader(rc, maxLogBytes))
	if err != nil {
		return nil, true
	}

	out := splitLogLines(string(raw))
	if int64(len(out)) > tail {
		out = out[int64(len(out))-tail:]
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, false
}

// splitLogLines splits a log blob into lines, normalising CRLF and dropping
// the empty line produced by a trailing newline.
func splitLogLines(raw string) []string {
	if raw == "" {
		return nil
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}
