package engine

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// maxMessageLen caps how much of a raw Kubernetes message is copied into
// Evidence strings, so a pathological kubelet message cannot blow up a report.
const maxMessageLen = 300

// ---------------------------------------------------------------------------
// Input normalisation
// ---------------------------------------------------------------------------

// effectiveKind returns the container kind, defaulting to KindApp when the
// collector left it unset (the overwhelmingly common case is an app container).
func effectiveKind(in Inputs) ContainerKind {
	if in.Kind == "" {
		return KindApp
	}
	return in.Kind
}

// effectiveTermination returns the termination state the engine should reason
// about: lastState.terminated wins (it describes the crash that caused the
// current restart), falling back to state.terminated for containers that are
// terminated right now and have no previous termination recorded.
func effectiveTermination(in Inputs) TerminationState {
	if in.LastTermination.Present {
		return in.LastTermination
	}
	return in.CurrentTermination
}

// initStuckThreshold returns the configured threshold, substituting the spec
// default when the caller left it zero-valued. A zero threshold must never
// silently disable the rule (nor make every init container "stuck").
func initStuckThreshold(in Inputs) time.Duration {
	if in.InitStuckThreshold <= 0 {
		return defaultInitStuckThreshold
	}
	return in.InitStuckThreshold
}

// ---------------------------------------------------------------------------
// Event helpers
// ---------------------------------------------------------------------------

// eventsByReason returns every event whose Reason equals reason (case-insensitive).
func eventsByReason(in Inputs, reasons ...string) []Event {
	var out []Event
	for _, e := range in.Events {
		for _, r := range reasons {
			if strings.EqualFold(e.Reason, r) {
				out = append(out, e)
				break
			}
		}
	}
	return out
}

// firstEventByReason returns the first event with one of the given reasons.
func firstEventByReason(in Inputs, reasons ...string) (Event, bool) {
	evs := eventsByReason(in, reasons...)
	if len(evs) == 0 {
		return Event{}, false
	}
	return evs[0], true
}

// eventsMatching returns events with the given reason whose message contains
// substr (case-insensitive).
func eventsMatching(in Inputs, reason, substr string) []Event {
	var out []Event
	for _, e := range eventsByReason(in, reason) {
		if containsFold(e.Message, substr) {
			out = append(out, e)
		}
	}
	return out
}

// containsFold is a case-insensitive strings.Contains.
func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// eventCount returns the reported occurrence count, treating 0 as 1 (some
// event sources leave Count unset for a single occurrence).
func eventCount(e Event) int32 {
	if e.Count <= 0 {
		return 1
	}
	return e.Count
}

// totalEventCount sums the occurrence counts of the given events.
func totalEventCount(evs []Event) int32 {
	var n int32
	for _, e := range evs {
		n += eventCount(e)
	}
	return n
}

// describeEvent renders an event as a single evidence line.
func describeEvent(e Event) string {
	return fmt.Sprintf("event: %s (x%d): %s", e.Reason, eventCount(e), truncateMessage(e.Message))
}

// describeEventTiming renders the observed time window of a set of events,
// returning "" when no timestamps were collected.
func describeEventTiming(evs []Event) string {
	var first, last time.Time
	for _, e := range evs {
		if !e.FirstSeen.IsZero() && (first.IsZero() || e.FirstSeen.Before(first)) {
			first = e.FirstSeen
		}
		if !e.LastSeen.IsZero() && (last.IsZero() || e.LastSeen.After(last)) {
			last = e.LastSeen
		}
	}
	switch {
	case first.IsZero() && last.IsZero():
		return ""
	case first.IsZero():
		return fmt.Sprintf("last observed at %s", last.UTC().Format(time.RFC3339))
	case last.IsZero() || last.Equal(first):
		return fmt.Sprintf("first observed at %s", first.UTC().Format(time.RFC3339))
	default:
		return fmt.Sprintf("observed from %s to %s (%s window)",
			first.UTC().Format(time.RFC3339), last.UTC().Format(time.RFC3339),
			formatDuration(last.Sub(first)))
	}
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func truncateMessage(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= maxMessageLen {
		return s
	}
	return s[:maxMessageLen] + "..."
}

func formatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	if d < time.Second {
		return d.String()
	}
	return d.Round(time.Second).String()
}

// containerNoun describes the container being diagnosed in prose.
func containerNoun(in Inputs) string {
	switch effectiveKind(in) {
	case KindInit:
		return "init container"
	case KindEphemeral:
		return "ephemeral container"
	case KindApp:
		return "container"
	default:
		return "container"
	}
}

// containerRef renders "container \"api\"" for use inside explanations.
func containerRef(in Inputs) string {
	if in.Container == "" {
		return containerNoun(in)
	}
	return fmt.Sprintf("%s %q", containerNoun(in), in.Container)
}

// podRef renders the pod for use in suggested commands.
func podRef(in Inputs) string {
	pod := in.Pod
	if pod == "" {
		pod = "<pod>"
	}
	return pod
}

// nsFlag renders "-n <namespace>" for suggested commands.
func nsFlag(in Inputs) string {
	ns := in.Namespace
	if ns == "" {
		ns = "<namespace>"
	}
	return "-n " + ns
}

// containerFlag renders "-c <container>" for suggested commands.
func containerFlag(in Inputs) string {
	if in.Container == "" {
		return ""
	}
	return " -c " + in.Container
}

// logsCommand builds the canonical "fetch the crashed container's logs" command.
func logsCommand(in Inputs, previous bool) string {
	cmd := fmt.Sprintf("kubectl logs %s %s%s", podRef(in), nsFlag(in), containerFlag(in))
	if previous {
		cmd += " --previous"
	}
	return cmd
}

// describeCommand builds "kubectl describe pod ...".
func describeCommand(in Inputs) string {
	return fmt.Sprintf("kubectl describe pod %s %s", podRef(in), nsFlag(in))
}

// ---------------------------------------------------------------------------
// Evidence builders shared by several rules
// ---------------------------------------------------------------------------

// terminationEvidence renders the standard exit-code / reason / signal lines.
func terminationEvidence(t TerminationState) []string {
	if !t.Present {
		return nil
	}
	ev := []string{fmt.Sprintf("exit code %d%s", t.ExitCode, exitCodeGloss(t.ExitCode))}
	if t.Reason != "" {
		ev = append(ev, "terminated reason: "+t.Reason)
	}
	if t.Signal != 0 {
		ev = append(ev, fmt.Sprintf("termination signal: %d (%s)", t.Signal, signalName(t.Signal)))
	}
	if msg := truncateMessage(t.Message); msg != "" {
		ev = append(ev, "termination message: "+msg)
	}
	if !t.StartedAt.IsZero() && !t.FinishedAt.IsZero() && t.FinishedAt.After(t.StartedAt) {
		ev = append(ev, fmt.Sprintf("container ran for %s before terminating (%s to %s)",
			formatDuration(t.FinishedAt.Sub(t.StartedAt)),
			t.StartedAt.UTC().Format(time.RFC3339), t.FinishedAt.UTC().Format(time.RFC3339)))
	}
	return ev
}

// exitCodeGloss annotates well-known exit codes.
func exitCodeGloss(code int32) string {
	switch code {
	case 126:
		return " (command found but not executable)"
	case 127:
		return " (command not found)"
	case 137:
		return " (128 + 9: killed by SIGKILL)"
	case 139:
		return " (128 + 11: killed by SIGSEGV, segmentation fault)"
	case 143:
		return " (128 + 15: killed by SIGTERM)"
	default:
		return ""
	}
}

func signalName(sig int32) string {
	switch sig {
	case 9:
		return "SIGKILL"
	case 11:
		return "SIGSEGV"
	case 15:
		return "SIGTERM"
	case 6:
		return "SIGABRT"
	case 2:
		return "SIGINT"
	default:
		return "signal"
	}
}

// restartEvidence reports the restart count when non-zero.
func restartEvidence(in Inputs) []string {
	if in.RestartCount <= 0 {
		return nil
	}
	ev := []string{fmt.Sprintf("restart count: %d", in.RestartCount)}
	if in.RestartPolicy != "" {
		ev = append(ev, "pod restartPolicy: "+in.RestartPolicy)
	}
	return ev
}

// memorySpecEvidence reports SPEC memory values only. The engine never has
// usage data (it is not collectable post-mortem from the Kubernetes API), so
// it must never make usage claims -- see spec decision log #1.
func memorySpecEvidence(in Inputs) []string {
	var ev []string
	if v := in.Limits["memory"]; v != "" {
		ev = append(ev, fmt.Sprintf("memory limit was %s (pod spec value)", v))
	}
	if v := in.Requests["memory"]; v != "" {
		ev = append(ev, fmt.Sprintf("memory request was %s (pod spec value)", v))
	}
	if in.QOSClass != "" {
		ev = append(ev, "QoS class: "+in.QOSClass)
	}
	return ev
}

// nodePressureEvidence reports collected node conditions that are true.
func nodePressureEvidence(in Inputs) []string {
	if !in.Node.Known {
		return nil
	}
	var ev []string
	if in.Node.MemoryPressure {
		ev = append(ev, "node condition MemoryPressure=true")
	}
	if in.Node.DiskPressure {
		ev = append(ev, "node condition DiskPressure=true")
	}
	if in.Node.PIDPressure {
		ev = append(ev, "node condition PIDPressure=true")
	}
	return ev
}

// ---------------------------------------------------------------------------
// Message parsing
// ---------------------------------------------------------------------------

var (
	reImagePullAuth = regexp.MustCompile(
		`(?i)(unauthorized|forbidden|authorization|authentication required|pull access denied|` +
			`requested access to the resource is denied|\b401\b|\b403\b)`)
	reImagePullNotFound = regexp.MustCompile(
		`(?i)(not found|manifest unknown|does not exist|no such host.*manifest)`)

	reSecretOrConfigMap = regexp.MustCompile(`(?i)\b(secrets?|config ?maps?)\s+"([^"]+)"\s+not found`)
	reMissingKey        = regexp.MustCompile(`(?i)couldn't find key\s+([^\s"]+)\s+in\s+(Secret|ConfigMap)\s+([^\s",]+)`)
	reNonExistentKey    = regexp.MustCompile(`(?i)references non-existent (secret|config ?map) key:?\s*([^\s",]+)`)
	reOptionalMissing   = regexp.MustCompile(`(?i)(secret|config ?map)\s+"?([^\s"]+)"?\s+not found`)

	reVolumeName       = regexp.MustCompile(`(?i)for volume\s+"([^"]+)"`)
	reUnmountedVolumes = regexp.MustCompile(`(?i)unmounted volumes=\[([^\]]*)\]`)
	rePVCNotFound      = regexp.MustCompile(`(?i)persistentvolumeclaims?\s+"([^"]+)"\s+not found`)

	reInsufficient   = regexp.MustCompile(`(?i)insufficient\s+([a-z0-9._/-]+)`)
	reUntoleratedTnt = regexp.MustCompile(`(?i)untolerated taint\s*\{([^}]*)\}`)
	reNodesAvailable = regexp.MustCompile(`(?i)(\d+)/(\d+)\s+nodes are available`)

	reFailedContainerName = regexp.MustCompile(`(?i)restarting failed container\s+(\S+)\s+in pod`)
	reInitContainerName   = regexp.MustCompile(`(?i)init container\s+"?([a-z0-9][a-z0-9.-]*)"?`)

	reEvictionResource = regexp.MustCompile(`(?i)node was low on resource:?\s*([a-z0-9.\-]+)`)
)

// imagePullContext returns the most informative image-pull failure message
// available, and whether an image-pull failure is present at all.
func imagePullContext(in Inputs) (string, bool) {
	waitingPull := in.Waiting.Present &&
		(strings.EqualFold(in.Waiting.Reason, "ErrImagePull") ||
			strings.EqualFold(in.Waiting.Reason, "ImagePullBackOff") ||
			strings.EqualFold(in.Waiting.Reason, "ImageInspectError") ||
			strings.EqualFold(in.Waiting.Reason, "RegistryUnavailable") ||
			strings.EqualFold(in.Waiting.Reason, "ErrImageNeverPull"))

	// The event message is normally far richer than the waiting message.
	for _, e := range eventsByReason(in, "Failed", "FailedToPullImage", "ErrImagePull") {
		if containsFold(e.Message, "pull") || containsFold(e.Message, "image") {
			return e.Message, true
		}
	}
	for _, e := range eventsByReason(in, "BackOff") {
		if containsFold(e.Message, "image") || containsFold(e.Message, "pulling") {
			return e.Message, true
		}
	}
	if waitingPull {
		return in.Waiting.Message, true
	}
	return "", false
}

// imagePullEvidence builds the evidence lines shared by all three pull rules.
func imagePullEvidence(in Inputs, msg string) []string {
	var ev []string
	if in.Waiting.Present && in.Waiting.Reason != "" {
		ev = append(ev, "waiting reason: "+in.Waiting.Reason)
	}
	if in.Image != "" {
		ev = append(ev, "image: "+in.Image)
	}
	for _, e := range eventsByReason(in, "Failed", "BackOff") {
		if containsFold(e.Message, "image") || containsFold(e.Message, "pull") {
			ev = append(ev, describeEvent(e))
		}
	}
	if len(ev) == 0 || !containsAny(ev, msg) {
		if m := truncateMessage(msg); m != "" {
			ev = append(ev, "registry message: "+m)
		}
	}
	return ev
}

func containsAny(haystack []string, needle string) bool {
	if needle == "" {
		return true
	}
	for _, h := range haystack {
		if strings.Contains(h, truncateMessage(needle)) {
			return true
		}
	}
	return false
}

// configErrorContext returns the CreateContainerConfigError message (or an
// equivalent event message) and whether such a failure is present.
func configErrorContext(in Inputs) (string, bool) {
	if in.Waiting.Present && strings.EqualFold(in.Waiting.Reason, "CreateContainerConfigError") {
		msg := in.Waiting.Message
		if msg == "" {
			if e, ok := firstEventByReason(in, "Failed"); ok {
				msg = e.Message
			}
		}
		return msg, true
	}
	for _, e := range eventsByReason(in, "Failed") {
		if reSecretOrConfigMap.MatchString(e.Message) || reMissingKey.MatchString(e.Message) ||
			reNonExistentKey.MatchString(e.Message) {
			return e.Message, true
		}
	}
	return "", false
}

// missingReference is the parsed identity of the object a container references
// but that does not exist (or lacks the referenced key).
type missingReference struct {
	Kind string // "Secret" | "ConfigMap"
	Name string
	Key  string
}

// parseMissingReference extracts the referenced object from a kubelet
// CreateContainerConfigError message. ok is false when nothing was parseable.
func parseMissingReference(msg string) (missingReference, bool) {
	if m := reMissingKey.FindStringSubmatch(msg); m != nil {
		name := m[3]
		// Kubelet renders this as "namespace/name".
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		return missingReference{Kind: normalizeRefKind(m[2]), Name: name, Key: m[1]}, true
	}
	if m := reSecretOrConfigMap.FindStringSubmatch(msg); m != nil {
		return missingReference{Kind: normalizeRefKind(m[1]), Name: m[2]}, true
	}
	if m := reNonExistentKey.FindStringSubmatch(msg); m != nil {
		return missingReference{Kind: normalizeRefKind(m[1]), Key: m[2]}, true
	}
	if m := reOptionalMissing.FindStringSubmatch(msg); m != nil {
		return missingReference{Kind: normalizeRefKind(m[1]), Name: m[2]}, true
	}
	return missingReference{}, false
}

func normalizeRefKind(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, " ", ""))
	s = strings.TrimSuffix(s, "s")
	if s == "configmap" {
		return "ConfigMap"
	}
	return "Secret"
}

// kubectlResource maps a reference kind to the kubectl resource name.
func kubectlResource(kind string) string {
	if kind == "ConfigMap" {
		return "configmap"
	}
	return "secret"
}

// parseVolumeNames extracts volume / claim identifiers from a mount failure message.
func parseVolumeNames(msg string) []string {
	var names []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		names = append(names, s)
	}
	if m := reVolumeName.FindStringSubmatch(msg); m != nil {
		add(m[1])
	}
	if m := reUnmountedVolumes.FindStringSubmatch(msg); m != nil {
		for _, v := range strings.Fields(strings.ReplaceAll(m[1], ",", " ")) {
			add(v)
		}
	}
	if m := rePVCNotFound.FindStringSubmatch(msg); m != nil {
		add(m[1])
	}
	return names
}

// schedulingCause is a parsed reason a pod could not be scheduled.
type schedulingCause struct {
	Kind      string   // "resources" | "taints" | "affinity" | "volume" | "pods" | "other"
	Resources []string // for Kind == "resources"
	Taints    []string // for Kind == "taints"
	Detail    string
}

// parseSchedulingFailure interprets a FailedScheduling message.
func parseSchedulingFailure(msg string) schedulingCause {
	sc := schedulingCause{Kind: "other", Detail: truncateMessage(msg)}
	if ms := reInsufficient.FindAllStringSubmatch(msg, -1); len(ms) > 0 {
		seen := map[string]bool{}
		for _, m := range ms {
			// Resource names may contain dots ("requests.nvidia.com/gpu"), so the
			// capture class allows them; strip the sentence punctuation the
			// scheduler message ends with ("... 2 Insufficient memory.").
			r := strings.ToLower(strings.Trim(m[1], ".,;:"))
			if r == "" || seen[r] {
				continue
			}
			seen[r] = true
			sc.Resources = append(sc.Resources, r)
		}
		sc.Kind = "resources"
		return sc
	}
	if ms := reUntoleratedTnt.FindAllStringSubmatch(msg, -1); len(ms) > 0 {
		seen := map[string]bool{}
		for _, m := range ms {
			t := strings.TrimSpace(m[1])
			if t != "" && !seen[t] {
				seen[t] = true
				sc.Taints = append(sc.Taints, t)
			}
		}
		sc.Kind = "taints"
		return sc
	}
	switch {
	case containsFold(msg, "volume node affinity conflict"),
		containsFold(msg, "had volume node affinity conflict"):
		sc.Kind = "volume"
		return sc
	case containsFold(msg, "node affinity"), containsFold(msg, "node selector"),
		containsFold(msg, "didn't match Pod's node affinity"), containsFold(msg, "node(s) didn't match node selector"):
		sc.Kind = "affinity"
		return sc
	case containsFold(msg, "persistent volume"), containsFold(msg, "persistentvolume"),
		containsFold(msg, "exceed max volume count"), containsFold(msg, "volume zone"):
		sc.Kind = "volume"
		return sc
	case containsFold(msg, "too many pods"):
		sc.Kind = "pods"
		return sc
	}
	return sc
}

// parseEvictionResource extracts the exhausted resource from an eviction message.
func parseEvictionResource(msg string) string {
	if m := reEvictionResource.FindStringSubmatch(msg); m != nil {
		return strings.ToLower(strings.TrimSuffix(m[1], "."))
	}
	switch {
	case containsFold(msg, "ephemeral-storage"), containsFold(msg, "disk"), containsFold(msg, "nodefs"),
		containsFold(msg, "imagefs"):
		return "ephemeral-storage"
	case containsFold(msg, "memory"):
		return "memory"
	case containsFold(msg, "pids"), containsFold(msg, "process"):
		return "pids"
	}
	return ""
}

// ---------------------------------------------------------------------------
// Log-tail pattern scanning
// ---------------------------------------------------------------------------

// logHint is one recognised application-level failure pattern in the log tail.
type logHint struct {
	Label string   // short machine-ish label, e.g. "go panic"
	Note  string   // one clause explaining what the pattern means
	Line  string   // the log line that matched
	Steps []string // hint-specific next steps
}

type logPattern struct {
	label      string
	note       string
	needles    []string
	caseSensit bool
	steps      []string
}

// logPatterns is the ordered pattern list from spec §4.2 (most specific first).
func logPatterns() []logPattern {
	return []logPattern{
		{
			label: "go panic", note: "an unrecovered Go panic terminated the process",
			needles: []string{"panic:"}, caseSensit: true,
			steps: []string{"Read the panic stack trace in the log tail: the top frame is where the process died"},
		},
		{
			label:   "jvm out of memory",
			note:    "the JVM ran out of HEAP (java.lang.OutOfMemoryError) - this is an in-process limit, not a cgroup OOM kill",
			needles: []string{"OutOfMemoryError"},
			steps: []string{
				"Compare -Xmx / -XX:MaxRAMPercentage with the container memory limit",
				"Note: the kernel did NOT report OOMKilled, so this is the JVM's own heap limit, not the cgroup limit",
			},
		},
		{
			label: "node module not found", note: "Node.js could not resolve a required module",
			needles: []string{"MODULE_NOT_FOUND", "Cannot find module"},
			steps:   []string{"Verify the image ships node_modules for the target platform and that the build stage ran npm/yarn install"},
		},
		{
			label: "java class not found", note: "the JVM could not load a class at runtime (classpath problem)",
			needles: []string{"ClassNotFoundException", "NoClassDefFoundError"},
			steps:   []string{"Check the image's classpath / shaded jar contents"},
		},
		{
			label: "connection refused", note: "the process could not reach a dependency (connection refused)",
			needles: []string{"ECONNREFUSED", "connection refused"},
			steps: []string{
				"Verify the dependency Service exists and has endpoints: kubectl get endpoints <service> -n <namespace>",
				"Check NetworkPolicies between this pod and the dependency",
			},
		},
		{
			label: "port already in use", note: "the process could not bind its listening port (address already in use)",
			needles: []string{"address already in use", "EADDRINUSE"},
			steps:   []string{"Check for a duplicate process in the container, a conflicting hostPort, or two containers sharing the pod network namespace on the same port"},
		},
		{
			label: "permission denied", note: "the process was denied access to a file or socket (permission denied)",
			needles: []string{"permission denied", "EACCES"},
			steps: []string{
				"Check securityContext.runAsUser / fsGroup against the ownership of the mounted volumes",
				"Check readOnlyRootFilesystem if the process writes to the root filesystem",
			},
		},
		{
			label: "segmentation fault", note: "the process segfaulted",
			needles: []string{"segmentation fault", "SIGSEGV"},
			steps:   []string{"Check for a CPU-architecture mismatch (arm64 image on amd64 nodes or vice versa) or a native library crash"},
		},
		{
			label: "fatal log line", note: "the application logged a fatal error immediately before exiting",
			needles: []string{"FATAL", "Fatal"}, caseSensit: true,
			steps: nil,
		},
	}
}

// scanLogTail returns the recognised failure patterns in the log tail, most
// specific first, at most one hint per pattern.
func scanLogTail(lines []string) []logHint {
	var hints []logHint
	for _, p := range logPatterns() {
		if line, ok := findLine(lines, p); ok {
			hints = append(hints, logHint{Label: p.label, Note: p.note, Line: line, Steps: p.steps})
		}
	}
	return hints
}

func findLine(lines []string, p logPattern) (string, bool) {
	for _, l := range lines {
		for _, n := range p.needles {
			if p.caseSensit {
				if strings.Contains(l, n) {
					return strings.TrimSpace(l), true
				}
				continue
			}
			if containsFold(l, n) {
				return strings.TrimSpace(l), true
			}
		}
	}
	return "", false
}
