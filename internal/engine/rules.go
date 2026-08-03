package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Shared matchers
//
// Every rule below is a pure predicate over Inputs, so each one is unit
// testable in isolation (spec §4.2: "func (r Rule) Match(in Inputs) *Diagnosis").
// Rules that mean "no Kubernetes-side cause applies" (app_exit_nonzero) or
// "nothing matched" (unknown) therefore re-evaluate the other matchers rather
// than relying on evaluation order inside Classify.
// ---------------------------------------------------------------------------

// oomMatched reports the ONLY signal the engine accepts as an OOM kill:
// terminated.reason == OOMKilled (spec decision log #1). Bare exit 137 is
// deliberately NOT treated as OOM.
func oomMatched(in Inputs) bool {
	t := effectiveTermination(in)
	return t.Present && strings.EqualFold(t.Reason, "OOMKilled")
}

// sigkillTermination reports exit 137 / SIGKILL regardless of attribution.
func sigkillTermination(in Inputs) bool {
	t := effectiveTermination(in)
	return t.Present && (t.ExitCode == 137 || t.Signal == 9)
}

// sigtermTermination reports exit 143 / SIGTERM.
func sigtermTermination(in Inputs) bool {
	t := effectiveTermination(in)
	return t.Present && (t.ExitCode == 143 || t.Signal == 15)
}

// evictionContext returns the eviction message and whether the pod was evicted.
func evictionContext(in Inputs) (string, bool) {
	evicted := strings.EqualFold(in.PodReason, "Evicted")
	e, hasEvent := firstEventByReason(in, "Evicted")
	if !evicted && !hasEvent {
		return "", false
	}
	msg := in.PodMessage
	if msg == "" && hasEvent {
		msg = e.Message
	}
	return msg, true
}

// livenessProbeContext returns the Unhealthy(Liveness) and Killing events when
// the liveness-probe signal is present. Probes never apply to init containers.
func livenessProbeContext(in Inputs) (unhealthy, killing []Event, ok bool) {
	if effectiveKind(in) != KindApp {
		return nil, nil, false
	}
	unhealthy = eventsForContainer(in, eventsMatching(in, "Liveness"))
	if len(unhealthy) == 0 {
		return nil, nil, false
	}
	killing = eventsForContainer(in, eventsByReason(in, "Killing"))
	if len(killing) == 0 && in.RestartCount == 0 {
		// A liveness probe that failed but never caused a kill or a restart is
		// not (yet) a crash cause.
		return nil, nil, false
	}
	return unhealthy, killing, true
}

// startupProbeContext is the startup-probe equivalent of livenessProbeContext.
func startupProbeContext(in Inputs) (unhealthy, killing []Event, ok bool) {
	if effectiveKind(in) != KindApp {
		return nil, nil, false
	}
	unhealthy = eventsForContainer(in, eventsMatching(in, "Startup"))
	if len(unhealthy) == 0 {
		return nil, nil, false
	}
	killing = eventsForContainer(in, eventsByReason(in, "Killing"))
	if len(killing) == 0 && in.RestartCount == 0 {
		return nil, nil, false
	}
	return unhealthy, killing, true
}

// probeKillMatched reports whether the kubelet killed this container because a
// probe failed. Used to keep sigkill_unattributed and app_exit_nonzero from
// claiming a kill that IS attributable.
func probeKillMatched(in Inputs) bool {
	if _, _, ok := livenessProbeContext(in); ok {
		return true
	}
	_, _, ok := startupProbeContext(in)
	return ok
}

// volumeMountEvents returns the mount/attach failure events for the pod.
func volumeMountEvents(in Inputs) []Event {
	return eventsByReason(in, "FailedMount", "FailedAttachVolume", "FailedMapVolume")
}

// schedulingEvents returns the FailedScheduling events for a Pending pod.
func schedulingEvents(in Inputs) []Event {
	if !strings.EqualFold(in.PodPhase, "Pending") {
		return nil
	}
	return eventsByReason(in, "FailedScheduling")
}

// imagePullClassification splits an image-pull failure into the three v1
// cause codes. category is "auth", "not_found" or "other"; a plain string is
// used deliberately so this stays out of CauseCode exhaustiveness checks.
func imagePullClassification(in Inputs) (category, msg string, ok bool) {
	msg, ok = imagePullContext(in)
	if !ok {
		return "", "", false
	}
	switch {
	case reImagePullAuth.MatchString(msg):
		return "auth", msg, true
	case reImagePullNotFound.MatchString(msg):
		return "not_found", msg, true
	default:
		return "other", msg, true
	}
}

// initFailureContext reports whether an app container is blocked because an
// init container of the same pod failed, and names it when the events allow.
func initFailureContext(in Inputs) (name string, hits []Event, ok bool) {
	if effectiveKind(in) != KindApp {
		return "", nil, false
	}
	if !in.Waiting.Present || !strings.EqualFold(in.Waiting.Reason, "PodInitializing") {
		return "", nil, false
	}
	for _, e := range in.Events {
		switch {
		case containsFold(e.Message, "init container"):
			hits = append(hits, e)
			if m := reInitContainerName.FindStringSubmatch(e.Message); m != nil && name == "" {
				name = m[1]
			}
		case strings.EqualFold(e.Reason, "BackOff"), strings.EqualFold(e.Reason, "Failed"):
			// While the pod is still initializing, a failing container can
			// only be an init container.
			hits = append(hits, e)
			if m := reFailedContainerName.FindStringSubmatch(e.Message); m != nil && name == "" {
				name = m[1]
			}
		}
	}
	if len(hits) == 0 {
		return "", nil, false
	}
	return name, hits, true
}

// observedStartupWindow returns how long the container managed to run before
// it was killed, when that is derivable from the collected inputs.
func observedStartupWindow(in Inputs) (time.Duration, bool) {
	t := effectiveTermination(in)
	if t.Present && !t.StartedAt.IsZero() && !t.FinishedAt.IsZero() && t.FinishedAt.After(t.StartedAt) {
		return t.FinishedAt.Sub(t.StartedAt), true
	}
	if in.Running.Present && in.RunningDuration > 0 {
		return in.RunningDuration, true
	}
	return 0, false
}

// probeBudgets separates the two durations a probe implies. Conflating them
// produces false arithmetic in reports: initialDelaySeconds elapses before any
// probe runs, so it belongs in "time since the container started" but never in
// "failureThreshold x periodSeconds".
type probeBudgets struct {
	// line is the evidence line describing the probe's configuration.
	line string
	// probing is failureThreshold x periodSeconds: how long the kubelet
	// tolerates FAILING probes before acting.
	probing time.Duration
	// total is probing plus initialDelaySeconds: how long after container
	// start the kubelet acts, at the earliest.
	total time.Duration
}

// probeBudget renders "failureThreshold=3 x periodSeconds=5 = 15s" for a probe.
func probeBudget(name string, p ProbeSpec) (probeBudgets, bool) {
	if !p.Defined || p.FailureThreshold <= 0 || p.PeriodSeconds <= 0 {
		return probeBudgets{}, false
	}
	b := probeBudgets{}
	b.probing = time.Duration(p.FailureThreshold) * time.Duration(p.PeriodSeconds) * time.Second
	b.total = b.probing
	b.line = fmt.Sprintf("%s probe config: failureThreshold=%d x periodSeconds=%d = %s before the kubelet acts",
		name, p.FailureThreshold, p.PeriodSeconds, formatDuration(b.probing))
	if p.InitialDelaySeconds > 0 {
		b.line += fmt.Sprintf(" (plus initialDelaySeconds=%d)", p.InitialDelaySeconds)
		b.total += time.Duration(p.InitialDelaySeconds) * time.Second
	}
	if p.TimeoutSeconds > 0 {
		b.line += fmt.Sprintf(", timeoutSeconds=%d", p.TimeoutSeconds)
	}
	return b, true
}

// sortedResourceEvidence renders a resource map deterministically.
func sortedResourceEvidence(label string, m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return []string{label + ": " + strings.Join(parts, ", ")}
}

func capitalizeFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---------------------------------------------------------------------------
// 1. oom_killed
// ---------------------------------------------------------------------------

func oomKilledRule() Rule {
	return Rule{
		Cause:     CauseOOMKilled,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			if !oomMatched(in) {
				return nil
			}
			t := effectiveTermination(in)
			d := newDiagnosis(in, CauseOOMKilled, ConfidenceHigh)
			d.Explanation = fmt.Sprintf(
				"%s was killed by the kernel out-of-memory killer: the kubelet reported terminated reason OOMKilled, "+
					"which is a direct cgroup attribution rather than an inference from the exit code. "+
					"The process tried to use more memory than the container's memory limit allows.",
				capitalizeFirst(containerRef(in)))
			appendEvidence(d, terminationEvidence(t)...)
			appendEvidence(d, restartEvidence(in)...)
			appendEvidence(d, memorySpecEvidence(in)...)
			appendEvidence(d, nodePressureEvidence(in)...)
			appendSteps(d,
				"Raise spec.containers[].resources.limits.memory if the workload legitimately needs more memory",
				"Otherwise cap the application's own memory use (JVM -XX:MaxRAMPercentage, Node --max-old-space-size, "+
					"worker/connection-pool counts) so it stays under the container limit",
				describeCommand(in)+"    # check Limits and the Last State: Terminated block",
				logsCommand(in, true)+"    # the log tail just before the kill often shows what allocated",
			)
			if strings.EqualFold(in.QOSClass, "Guaranteed") ||
				(in.Limits["memory"] != "" && in.Limits["memory"] == in.Requests["memory"]) {
				appendSteps(d, "Requests equal limits here (Guaranteed QoS): raising the limit also raises the scheduled reservation")
			}
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 2. sigkill_unattributed
// ---------------------------------------------------------------------------

func sigkillUnattributedRule() Rule {
	return Rule{
		Cause:     CauseSigkillUnattributed,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			if !sigkillTermination(in) || oomMatched(in) || in.Deleting {
				return nil
			}
			if probeKillMatched(in) {
				// The kill IS attributable: a probe caused it.
				return nil
			}
			t := effectiveTermination(in)
			conf := ConfidenceMedium
			sparse := len(in.Events) == 0
			if sparse {
				conf = ConfidenceLow
			}
			d := newDiagnosis(in, CauseSigkillUnattributed, conf)
			expl := fmt.Sprintf(
				"%s was killed with SIGKILL (exit 137), but nothing attributes the kill: the kubelet did not report "+
					"reason OOMKilled and the pod is not being deleted. The suspects are a node-level (system) OOM kill "+
					"that the kernel did not attribute to this container's cgroup, or an external SIGKILL from outside "+
					"the container (node agent, operator, or a manual kill).",
				capitalizeFirst(containerRef(in)))
			if in.Node.Known && in.Node.MemoryPressure {
				expl += " The node reports MemoryPressure=true, which makes a node-level OOM kill the leading suspect."
			}
			d.Explanation = expl
			appendEvidence(d, terminationEvidence(t)...)
			appendEvidence(d,
				"no OOMKilled reason reported by the kubelet: the kernel did not attribute a cgroup OOM kill to this container",
				"pod has no deletionTimestamp, so this is not termination-grace-period expiry")
			appendEvidence(d, restartEvidence(in)...)
			appendEvidence(d, memorySpecEvidence(in)...)
			appendEvidence(d, nodePressureEvidence(in)...)
			if in.Node.Known && !in.Node.MemoryPressure && !in.Node.DiskPressure && !in.Node.PIDPressure {
				appendEvidence(d, "node reports no MemoryPressure/DiskPressure/PIDPressure condition")
			}
			if !in.Node.Known {
				appendEvidence(d, "node conditions were not collected, so node-level memory pressure could not be checked")
			}
			if sparse {
				appendEvidence(d, "no events were collected for this pod, so the kill could not be attributed further "+
					"(confidence lowered)")
			}
			appendSteps(d,
				"kubectl describe node <node>    # look at the MemoryPressure / DiskPressure conditions",
				"kubectl get events -A --field-selector involvedObject.kind=Node    # node-level OOM and eviction events",
				"On the node: dmesg -T | grep -i -e 'killed process' -e 'out of memory'    # kernel OOM killer log",
				"Check whether a sidecar, operator or node agent kills this container (kubectl get events "+
					nsFlag(in)+")",
				"Set or raise resources.limits.memory so a future kill is attributed to the cgroup and reported as OOMKilled",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 3. evicted
// ---------------------------------------------------------------------------

func evictedRule() Rule {
	return Rule{
		Cause:     CauseEvicted,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			msg, ok := evictionContext(in)
			if !ok {
				return nil
			}
			resource := parseEvictionResource(msg)
			d := newDiagnosis(in, CauseEvicted, ConfidenceHigh)
			switch resource {
			case "memory":
				d.Explanation = "The kubelet evicted this pod because the NODE ran out of memory (node-level " +
					"MemoryPressure eviction, not a cgroup OOM kill of this container). Pods are evicted in QoS order: " +
					"BestEffort first, then Burstable that exceed their requests."
				appendSteps(d,
					"Set memory requests that reflect real usage so the pod is ranked higher than BestEffort pods during eviction",
					"kubectl top nodes    # find which node is under memory pressure (needs metrics-server)",
					"Consider a Guaranteed QoS class (requests == limits) for latency-critical workloads",
				)
			case "ephemeral-storage", "disk":
				d.Explanation = "The kubelet evicted this pod because the node ran out of ephemeral storage " +
					"(DiskPressure). Container writable layers, emptyDir volumes and logs all count against the node's " +
					"ephemeral storage."
				appendSteps(d,
					"Set resources.requests/limits.ephemeral-storage for this container",
					"Write large or long-lived data to a PersistentVolume instead of the container filesystem or emptyDir",
					"Check log rotation: a chatty container can fill the node's disk on its own",
				)
			case "pids":
				d.Explanation = "The kubelet evicted this pod because the node ran out of process IDs (PIDPressure)."
				appendSteps(d,
					"Look for a process/thread leak in the application",
					"Consider a pod-level pids limit on the node (kubelet --pod-max-pids)",
				)
			default:
				d.Explanation = "The kubelet evicted this pod: the node reported resource pressure and reclaimed it. " +
					"The eviction message below names the exhausted resource."
				appendSteps(d, "Read the eviction message above to identify which resource the node ran out of")
			}
			d.Explanation = fmt.Sprintf("%s (%s was running in that pod.)", d.Explanation, capitalizeFirst(containerRef(in)))
			if in.PodReason != "" {
				appendEvidence(d, "pod status reason: "+in.PodReason)
			}
			if m := truncateMessage(msg); m != "" {
				appendEvidence(d, "pod status message: "+m)
			}
			for _, e := range eventsByReason(in, "Evicted") {
				appendEvidence(d, describeEvent(e))
			}
			if resource != "" {
				appendEvidence(d, "exhausted node resource: "+resource)
			}
			appendEvidence(d, nodePressureEvidence(in)...)
			appendEvidence(d, sortedResourceEvidence("container requests", in.Requests)...)
			appendEvidence(d, sortedResourceEvidence("container limits", in.Limits)...)
			if in.QOSClass != "" {
				appendEvidence(d, "QoS class: "+in.QOSClass)
			}
			appendSteps(d,
				"kubectl describe node <node>    # confirm the pressure condition and see what else the node runs",
				"kubectl get events "+nsFlag(in)+" --field-selector reason=Evicted",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 4. probe_liveness_failure
// ---------------------------------------------------------------------------

func probeLivenessRule() Rule {
	return Rule{
		Cause:     CauseProbeLiveness,
		AppliesTo: appOnly(),
		Match: func(in Inputs) *Diagnosis {
			unhealthy, killing, ok := livenessProbeContext(in)
			if !ok {
				return nil
			}
			conf := ConfidenceMedium
			if len(killing) > 0 {
				conf = ConfidenceHigh
			}
			failures := totalEventCount(unhealthy)
			d := newDiagnosis(in, CauseProbeLiveness, conf)
			expl := fmt.Sprintf(
				"The kubelet restarted %s because its liveness probe kept failing (%d recorded Unhealthy events). "+
					"The container process was alive but did not answer the liveness probe, so Kubernetes killed it on purpose.",
				containerRef(in), failures)
			budgets, hasBudget := probeBudget("liveness", in.Liveness)
			if hasBudget {
				expl += fmt.Sprintf(" With failureThreshold=%d and periodSeconds=%d the container is killed after roughly %s of failing probes",
					in.Liveness.FailureThreshold, in.Liveness.PeriodSeconds, formatDuration(budgets.probing))
				if budgets.total > budgets.probing {
					expl += fmt.Sprintf(", i.e. about %s after it starts (initialDelaySeconds=%d runs no probes at all)",
						formatDuration(budgets.total), in.Liveness.InitialDelaySeconds)
				}
				expl += "."
			}
			d.Explanation = expl

			for _, e := range unhealthy {
				appendEvidence(d, describeEvent(e))
			}
			for _, e := range killing {
				appendEvidence(d, describeEvent(e))
			}
			if t := describeEventTiming(unhealthy); t != "" {
				appendEvidence(d, "liveness failures "+t)
			}
			appendEvidence(d, fmt.Sprintf("observed liveness probe failures: %d", failures))
			if hasBudget {
				appendEvidence(d, budgets.line)
			} else {
				appendEvidence(d, "liveness probe configuration was not collected")
			}
			appendEvidence(d, restartEvidence(in)...)
			appendEvidence(d, terminationEvidence(effectiveTermination(in))...)
			if in.Startup.Defined {
				if sb, okB := probeBudget("startup", in.Startup); okB {
					appendEvidence(d, sb.line)
				}
			} else {
				appendEvidence(d, "no startup probe is defined, so the liveness probe also governs slow startups")
			}
			appendSteps(d,
				"Call the liveness endpoint yourself: kubectl exec "+podRef(in)+" "+nsFlag(in)+
					containerFlag(in)+" -- wget -qO- http://localhost:<port><path>",
				"Check whether the probe is too strict: raise failureThreshold / periodSeconds / timeoutSeconds",
				"If the app is slow to start, add a startupProbe instead of a long initialDelaySeconds on liveness",
				"Make the liveness endpoint cheap and dependency-free: it must not call the database or downstream services",
				logsCommand(in, true)+"    # what the app was doing while the probe timed out",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 5. probe_startup_failure
// ---------------------------------------------------------------------------

func probeStartupRule() Rule {
	return Rule{
		Cause:     CauseProbeStartup,
		AppliesTo: appOnly(),
		Match: func(in Inputs) *Diagnosis {
			unhealthy, killing, ok := startupProbeContext(in)
			if !ok {
				return nil
			}
			conf := ConfidenceMedium
			if len(killing) > 0 {
				conf = ConfidenceHigh
			}
			failures := totalEventCount(unhealthy)
			d := newDiagnosis(in, CauseProbeStartup, conf)
			expl := fmt.Sprintf(
				"%s never finished starting up: its startup probe failed %d times and the kubelet restarted the container "+
					"before the application became ready to serve.",
				capitalizeFirst(containerRef(in)), failures)

			budgets, hasBudget := probeBudget("startup", in.Startup)
			observed, hasObserved := observedStartupWindow(in)
			if hasBudget && hasObserved {
				// observed is measured from container start, so it is compared
				// against the total (probing plus initialDelaySeconds), while
				// the quoted arithmetic must show the probing budget alone.
				if observed <= budgets.total+2*time.Second {
					expl += fmt.Sprintf(" The startup budget is failureThreshold=%d x periodSeconds=%d = %s and the container "+
						"only ran %s before being killed, so the budget is too tight for this application's startup time.",
						in.Startup.FailureThreshold, in.Startup.PeriodSeconds, formatDuration(budgets.probing), formatDuration(observed))
				} else {
					expl += fmt.Sprintf(" The startup budget is %s; the container ran %s, so it was still failing its probe "+
						"well past the budget.", formatDuration(budgets.probing), formatDuration(observed))
				}
			} else if hasBudget {
				expl += fmt.Sprintf(" The startup budget is failureThreshold=%d x periodSeconds=%d = %s.",
					in.Startup.FailureThreshold, in.Startup.PeriodSeconds, formatDuration(budgets.probing))
			}
			d.Explanation = expl

			for _, e := range unhealthy {
				appendEvidence(d, describeEvent(e))
			}
			for _, e := range killing {
				appendEvidence(d, describeEvent(e))
			}
			if t := describeEventTiming(unhealthy); t != "" {
				appendEvidence(d, "startup failures "+t)
			}
			if hasBudget {
				appendEvidence(d, budgets.line)
			} else {
				appendEvidence(d, "startup probe configuration was not collected")
			}
			if hasObserved {
				appendEvidence(d, "observed startup window: container ran "+formatDuration(observed)+" before termination")
			}
			appendEvidence(d, restartEvidence(in)...)
			appendEvidence(d, terminationEvidence(effectiveTermination(in))...)
			appendSteps(d,
				"Raise startupProbe.failureThreshold (it is the cheap knob: it only affects the startup window)",
				"Measure real cold-start time with "+logsCommand(in, true)+" and set failureThreshold x periodSeconds above it",
				"Check whether startup blocks on a dependency (migrations, cache warm-up, a downstream service)",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 6-8. image_pull_auth / image_pull_not_found / image_pull_other
// ---------------------------------------------------------------------------

func imagePullBase(in Inputs, cause CauseCode, conf Confidence, msg string) *Diagnosis {
	d := newDiagnosis(in, cause, conf)
	appendEvidence(d, imagePullEvidence(in, msg)...)
	appendEvidence(d, restartEvidence(in)...)
	appendSteps(d, describeCommand(in)+"    # the Events section shows the full registry error")
	return d
}

func imagePullAuthRule() Rule {
	return Rule{
		Cause:     CauseImagePullAuth,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			category, msg, ok := imagePullClassification(in)
			if !ok || category != "auth" {
				return nil
			}
			d := imagePullBase(in, CauseImagePullAuth, ConfidenceHigh, msg)
			d.Explanation = fmt.Sprintf(
				"The kubelet could not pull the image for %s because the registry rejected the credentials "+
					"(authorization failure). The image reference itself may be fine: the node is simply not allowed to fetch it.",
				containerRef(in))
			appendSteps(d,
				"Check the pod's imagePullSecrets: "+describeCommand(in)+" | grep -i 'image pull secrets'",
				"Check the ServiceAccount's imagePullSecrets: kubectl get sa <serviceaccount> "+nsFlag(in)+" -o yaml",
				"Verify the secret is of type kubernetes.io/dockerconfigjson and targets the right registry host",
				"Remember that imagePullSecrets are namespaced: a secret that works in one namespace must be copied to this one",
				"For private registries on managed clusters, confirm the node identity has registry read permission",
			)
			return d
		},
	}
}

func imagePullNotFoundRule() Rule {
	return Rule{
		Cause:     CauseImagePullNotFound,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			category, msg, ok := imagePullClassification(in)
			if !ok || category != "not_found" {
				return nil
			}
			d := imagePullBase(in, CauseImagePullNotFound, ConfidenceHigh, msg)
			d.Explanation = fmt.Sprintf(
				"The image referenced by %s does not exist in the registry: the registry answered that the repository "+
					"or the tag/digest is unknown. This is almost always a typo, a tag that was never pushed, or a tag "+
					"that was deleted or overwritten.",
				containerRef(in))
			if in.Image != "" {
				appendSteps(d, "Verify the tag exists: docker manifest inspect "+in.Image+" (or crane manifest "+in.Image+")")
			}
			appendSteps(d,
				"Check the image reference for typos, a missing registry host, or a missing project/org path segment",
				"If CI pushes this tag, confirm the push job actually completed before the rollout",
			)
			return d
		},
	}
}

func imagePullOtherRule() Rule {
	return Rule{
		Cause:     CauseImagePullOther,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			category, msg, ok := imagePullClassification(in)
			if !ok || category != "other" {
				return nil
			}
			d := imagePullBase(in, CauseImagePullOther, ConfidenceMedium, msg)
			d.Explanation = fmt.Sprintf(
				"The kubelet could not pull the image for %s, and the registry error is neither an authorization failure "+
					"nor a missing image. Typical causes are a network timeout, a TLS/proxy problem, or a registry rate "+
					"limit or quota.",
				containerRef(in))
			appendSteps(d,
				"Check node-to-registry connectivity and any egress proxy / firewall rule for the registry host",
				"Check for registry rate limiting (Docker Hub anonymous pull limits are a classic) and authenticate if so",
				"Check the node's container-runtime TLS trust if the registry uses a private CA",
				"kubectl get events "+nsFlag(in)+" --field-selector reason=Failed",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 9. config_missing_reference
// ---------------------------------------------------------------------------

func configMissingRefRule() Rule {
	return Rule{
		Cause:     CauseConfigMissingRef,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			msg, ok := configErrorContext(in)
			if !ok {
				return nil
			}
			ref, parsed := parseMissingReference(msg)
			conf := ConfidenceMedium
			if parsed {
				conf = ConfidenceHigh
			}
			d := newDiagnosis(in, CauseConfigMissingRef, conf)
			switch {
			case parsed && ref.Key != "" && ref.Name != "":
				d.Explanation = fmt.Sprintf(
					"The kubelet could not create %s because it references key %q of %s %q, and that key does not exist. "+
						"The container never started: this is a configuration error, not an application crash.",
					containerRef(in), ref.Key, ref.Kind, ref.Name)
			case parsed && ref.Name != "":
				d.Explanation = fmt.Sprintf(
					"The kubelet could not create %s because it references %s %q, which does not exist in namespace %q. "+
						"The container never started: this is a configuration error, not an application crash.",
					containerRef(in), ref.Kind, ref.Name, in.Namespace)
			case parsed && ref.Key != "":
				d.Explanation = fmt.Sprintf(
					"The kubelet could not create %s because it references a non-existent %s key %q. "+
						"The container never started: this is a configuration error, not an application crash.",
					containerRef(in), ref.Kind, ref.Key)
			default:
				d.Explanation = fmt.Sprintf(
					"The kubelet could not create %s: CreateContainerConfigError means a referenced ConfigMap, Secret or "+
						"key could not be resolved. The container never started.",
					containerRef(in))
			}

			if in.Waiting.Present && in.Waiting.Reason != "" {
				appendEvidence(d, "waiting reason: "+in.Waiting.Reason)
			}
			if m := truncateMessage(msg); m != "" {
				appendEvidence(d, "kubelet message: "+m)
			}
			if parsed {
				if ref.Name != "" {
					appendEvidence(d, fmt.Sprintf("missing reference: %s %q", ref.Kind, ref.Name))
				}
				if ref.Key != "" {
					appendEvidence(d, fmt.Sprintf("missing key: %q", ref.Key))
				}
			}
			for _, e := range eventsByReason(in, "Failed") {
				appendEvidence(d, describeEvent(e))
			}
			appendEvidence(d, restartEvidence(in)...)

			resource := kubectlResource(ref.Kind)
			if parsed && ref.Name != "" {
				appendSteps(d,
					fmt.Sprintf("kubectl get %s %s %s", resource, ref.Name, nsFlag(in)),
					fmt.Sprintf("kubectl describe %s %s %s    # confirm the expected keys exist", resource, ref.Name, nsFlag(in)),
				)
				if ref.Key != "" {
					appendSteps(d, fmt.Sprintf("kubectl get %s %s %s -o jsonpath='{.data.%s}'",
						resource, ref.Name, nsFlag(in), ref.Key))
				}
				appendSteps(d, fmt.Sprintf("If the %s is created by another controller (external-secrets, sealed-secrets, "+
					"a Helm hook), check that it reconciled before this pod was scheduled", ref.Kind))
			} else {
				appendSteps(d,
					"kubectl get configmap,secret "+nsFlag(in)+"    # compare against envFrom / valueFrom / volume references",
					describeCommand(in)+"    # the Events section names the missing object",
				)
			}
			appendSteps(d, "Check for a namespace mismatch: ConfigMaps and Secrets are not shared across namespaces")
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 10. volume_mount_failure
// ---------------------------------------------------------------------------

func volumeMountFailureRule() Rule {
	return Rule{
		Cause:     CauseVolumeMountFailure,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			evs := volumeMountEvents(in)
			if len(evs) == 0 {
				return nil
			}
			var names []string
			seen := map[string]bool{}
			for _, e := range evs {
				for _, n := range parseVolumeNames(e.Message) {
					if !seen[n] {
						seen[n] = true
						names = append(names, n)
					}
				}
			}
			d := newDiagnosis(in, CauseVolumeMountFailure, ConfidenceHigh)
			if len(names) > 0 {
				d.Explanation = fmt.Sprintf(
					"The pod could not start because the kubelet failed to attach or mount volume(s) %s. "+
						"%s never ran: the container cannot be created until every volume is mounted.",
					strings.Join(quoteAll(names), ", "), capitalizeFirst(containerRef(in)))
			} else {
				d.Explanation = fmt.Sprintf(
					"The pod could not start because a volume failed to attach or mount. %s never ran: the container "+
						"cannot be created until every volume is mounted.",
					capitalizeFirst(containerRef(in)))
			}
			for _, e := range evs {
				appendEvidence(d, describeEvent(e))
			}
			for _, n := range names {
				appendEvidence(d, fmt.Sprintf("affected volume/claim: %q", n))
			}
			if in.Waiting.Present && in.Waiting.Reason != "" {
				appendEvidence(d, "waiting reason: "+in.Waiting.Reason)
			}
			appendSteps(d,
				"kubectl get pvc "+nsFlag(in)+"    # is the claim Bound?",
				"kubectl describe pvc <claim> "+nsFlag(in)+"    # provisioning / storage-class errors show up here",
				"For 'volume node affinity conflict': the PV is pinned to a zone the pod cannot be scheduled into",
				"For a Secret/ConfigMap-backed volume: confirm the referenced object exists in "+nsFlag(in)[3:],
				"Check the CSI driver pods on the node (kubectl get pods -n kube-system) and the node's attach limits",
			)
			return d
		},
	}
}

func quoteAll(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, fmt.Sprintf("%q", s))
	}
	return out
}

// ---------------------------------------------------------------------------
// 11. init_container_failure (app containers only)
//
// This rule fires when the container being diagnosed is an APP container whose
// pod cannot progress because an init container failed. The "recursion" from
// spec §4.2 is performed by the caller: it builds a fresh Inputs with
// Kind=KindInit for the failing init container and calls Classify again, at
// which point only init-applicable rules can match.
// ---------------------------------------------------------------------------

func initContainerFailureRule() Rule {
	return Rule{
		Cause:     CauseInitContainerFailure,
		AppliesTo: appOnly(),
		Match: func(in Inputs) *Diagnosis {
			name, hits, ok := initFailureContext(in)
			if !ok {
				return nil
			}
			conf := ConfidenceMedium
			if name != "" {
				conf = ConfidenceHigh
			}
			d := newDiagnosis(in, CauseInitContainerFailure, conf)
			if name != "" {
				d.Explanation = fmt.Sprintf(
					"%s has not started yet: the pod is still initializing and init container %q is failing. "+
						"Nothing is wrong with this container - diagnose the init container instead.",
					capitalizeFirst(containerRef(in)), name)
			} else {
				d.Explanation = fmt.Sprintf(
					"%s has not started yet: the pod is stuck in initialization because an init container is failing. "+
						"Nothing is wrong with this container - diagnose the failing init container instead.",
					capitalizeFirst(containerRef(in)))
			}
			appendEvidence(d, "waiting reason: PodInitializing (the pod never reached its app containers)")
			if in.PodPhase != "" {
				appendEvidence(d, "pod phase: "+in.PodPhase)
			}
			for _, e := range hits {
				appendEvidence(d, describeEvent(e))
			}
			if name != "" {
				appendEvidence(d, fmt.Sprintf("failing init container: %q", name))
			}
			initFlag := " -c <init-container>"
			if name != "" {
				initFlag = " -c " + name
			}
			appendSteps(d,
				fmt.Sprintf("kubectl logs %s %s%s --previous", podRef(in), nsFlag(in), initFlag),
				describeCommand(in)+"    # read the Init Containers section, not the Containers section",
				fmt.Sprintf("crashcause inspect %s %s%s    # classify the init container itself", podRef(in), nsFlag(in), initFlag),
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 12. init_container_stuck (init containers only)
// ---------------------------------------------------------------------------

func initContainerStuckRule() Rule {
	return Rule{
		Cause:     CauseInitContainerStuck,
		AppliesTo: []ContainerKind{KindInit},
		Match: func(in Inputs) *Diagnosis {
			if effectiveKind(in) != KindInit {
				return nil
			}
			threshold := initStuckThreshold(in)
			longRunning := in.Running.Present && in.RunningDuration > threshold
			if !longRunning && !in.ActiveDeadlineExceeded {
				return nil
			}
			conf := ConfidenceMedium
			if in.ActiveDeadlineExceeded {
				conf = ConfidenceHigh
			}
			d := newDiagnosis(in, CauseInitContainerStuck, conf)
			switch {
			case in.ActiveDeadlineExceeded && longRunning:
				d.Explanation = fmt.Sprintf(
					"Init container %q has been running for %s (threshold %s) and the pod exceeded its "+
						"activeDeadlineSeconds. The pod is stuck in initialization: it is not crashing, it is waiting "+
						"for something that never happens.",
					in.Container, formatDuration(in.RunningDuration), formatDuration(threshold))
			case in.ActiveDeadlineExceeded:
				d.Explanation = fmt.Sprintf(
					"The pod exceeded its activeDeadlineSeconds while init container %q was still running. The pod is "+
						"stuck in initialization: it is not crashing, it is waiting for something that never happens.",
					in.Container)
			default:
				d.Explanation = fmt.Sprintf(
					"Init container %q has been running for %s, which is longer than the %s stuck-init threshold. The "+
						"pod is stuck in Init and is not crashing: the init container is most likely waiting on a "+
						"dependency that is not coming up.",
					in.Container, formatDuration(in.RunningDuration), formatDuration(threshold))
			}
			if longRunning {
				appendEvidence(d, fmt.Sprintf("init container has been running for %s (stuck-init threshold %s)",
					formatDuration(in.RunningDuration), formatDuration(threshold)))
			}
			if !in.Running.StartedAt.IsZero() {
				appendEvidence(d, "running since "+in.Running.StartedAt.UTC().Format(time.RFC3339))
			}
			if in.ActiveDeadlineExceeded {
				appendEvidence(d, "pod activeDeadlineSeconds exceeded")
			}
			if in.PodPhase != "" {
				appendEvidence(d, "pod phase: "+in.PodPhase)
			}
			appendEvidence(d, restartEvidence(in)...)
			for _, e := range in.Events {
				if strings.EqualFold(e.Type, "Warning") {
					appendEvidence(d, describeEvent(e))
				}
			}
			appendSteps(d,
				logsCommand(in, false)+" -f    # what is the init container waiting for?",
				"Check the dependency it waits on (a Service with no endpoints, a database, a migration job)",
				"kubectl get endpoints "+nsFlag(in)+"    # a wait-for-service init container hangs when endpoints stay empty",
				"Check NetworkPolicies: an init container that cannot reach its dependency waits forever",
				"Give the init container its own timeout so it fails loudly instead of hanging",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 13. unschedulable
// ---------------------------------------------------------------------------

func unschedulableRule() Rule {
	return Rule{
		Cause:     CauseUnschedulable,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			evs := schedulingEvents(in)
			if len(evs) == 0 {
				return nil
			}
			msg := evs[0].Message
			for _, e := range evs {
				if len(e.Message) > len(msg) {
					msg = e.Message
				}
			}
			sc := parseSchedulingFailure(msg)
			d := newDiagnosis(in, CauseUnschedulable, ConfidenceHigh)
			switch sc.Kind {
			case "resources":
				d.Explanation = fmt.Sprintf(
					"The pod is Pending because the scheduler found no node with enough allocatable %s. This is a "+
						"capacity problem, not a container problem: nothing has started, so there are no logs to read.",
					strings.Join(sc.Resources, " and "))
				appendSteps(d,
					"kubectl describe nodes | grep -A5 'Allocated resources'    # compare with this pod's requests",
					"Lower spec.containers[].resources.requests to something the cluster can actually satisfy",
					"Add capacity (scale the node pool) or wait for the cluster autoscaler",
					"Check ResourceQuota and LimitRange in the namespace: they can inflate effective requests",
				)
			case "taints":
				d.Explanation = fmt.Sprintf(
					"The pod is Pending because every candidate node carries a taint the pod does not tolerate (%s). "+
						"Nothing has started, so there are no logs to read.",
					strings.Join(sc.Taints, "; "))
				appendSteps(d,
					"Add a matching toleration to the pod spec, or remove the taint from the nodes",
					"kubectl get nodes -o custom-columns=NAME:.metadata.name,TAINTS:.spec.taints",
					"Dedicated node pools (GPU, spot, control-plane) are the usual source of this taint",
				)
			case "affinity":
				d.Explanation = "The pod is Pending because no node matches its nodeSelector / node affinity. " +
					"Nothing has started, so there are no logs to read."
				appendSteps(d,
					"kubectl get nodes --show-labels    # compare with the pod's nodeSelector / nodeAffinity",
					"Check for a typo in a label key or value (topology.kubernetes.io/zone, node pool labels)",
					"Relax requiredDuringSchedulingIgnoredDuringExecution to preferred if the constraint is a preference",
				)
			case "volume":
				d.Explanation = "The pod is Pending because of a volume constraint: the scheduler could not place it " +
					"near the persistent volume it must use (typically a zone conflict, or no volume available to bind). " +
					"Nothing has started, so there are no logs to read."
				appendSteps(d,
					"kubectl get pv,pvc "+nsFlag(in)+"    # check binding state and the PV's node affinity",
					"Use a StorageClass with volumeBindingMode: WaitForFirstConsumer to avoid zone conflicts",
					"Check per-node volume attach limits if the nodes already carry many volumes",
				)
			case "pods":
				d.Explanation = "The pod is Pending because the candidate nodes are already at their maximum pod count. " +
					"Nothing has started, so there are no logs to read."
				appendSteps(d,
					"Add nodes, or raise the kubelet --max-pods / node pool pod density setting",
					"kubectl get pods -A -o wide | awk '{print $8}' | sort | uniq -c    # pods per node",
				)
			default:
				d.Explanation = "The pod is Pending: the scheduler reported FailedScheduling and could not place it on " +
					"any node. Nothing has started, so there are no logs to read."
				appendSteps(d, "Read the FailedScheduling message above: it enumerates why each node was rejected")
			}

			appendEvidence(d, "pod phase: Pending")
			for _, e := range evs {
				appendEvidence(d, describeEvent(e))
			}
			if m := reNodesAvailable.FindStringSubmatch(msg); m != nil {
				appendEvidence(d, fmt.Sprintf("scheduler rejected all %s candidate nodes (%s/%s available)", m[2], m[1], m[2]))
			}
			for _, r := range sc.Resources {
				appendEvidence(d, "insufficient node resource: "+r)
			}
			for _, t := range sc.Taints {
				appendEvidence(d, "untolerated taint: "+t)
			}
			appendEvidence(d, sortedResourceEvidence("container requests", in.Requests)...)
			appendEvidence(d, sortedResourceEvidence("container limits", in.Limits)...)
			appendSteps(d, describeCommand(in)+"    # full scheduler message in the Events section")
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 14. app_exit_nonzero
// ---------------------------------------------------------------------------

// kubernetesSideCauseMatched reports whether any Kubernetes-side cause explains
// this termination. app_exit_nonzero is defined as "a clean app crash with no
// k8s-side cause", so it must stand down whenever one of these matched.
func kubernetesSideCauseMatched(in Inputs) bool {
	if oomMatched(in) || sigkillTermination(in) || probeKillMatched(in) {
		return true
	}
	if _, ok := evictionContext(in); ok {
		return true
	}
	if _, ok := imagePullContext(in); ok {
		return true
	}
	if _, ok := configErrorContext(in); ok {
		return true
	}
	return len(volumeMountEvents(in)) > 0
}

func appExitNonzeroRule() Rule {
	return Rule{
		Cause:     CauseAppExitNonzero,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			t := effectiveTermination(in)
			if !t.Present || t.ExitCode == 0 {
				return nil
			}
			if kubernetesSideCauseMatched(in) {
				return nil
			}
			hints := scanLogTail(in.LogTail)
			sigterm := sigtermTermination(in)

			conf := ConfidenceMedium
			switch {
			case t.ExitCode == 126 || t.ExitCode == 127:
				conf = ConfidenceHigh
			case t.ExitCode == 139 || t.Signal == 11:
				conf = ConfidenceHigh
			case sigterm:
				conf = ConfidenceLow
			case len(hints) > 0:
				conf = ConfidenceMedium
			case in.LogsUnavailable || len(in.LogTail) == 0:
				conf = ConfidenceLow
			}

			d := newDiagnosis(in, CauseAppExitNonzero, conf)

			var expl string
			if effectiveKind(in) == KindInit {
				expl = fmt.Sprintf(
					"Init container %q exited with code %d. No Kubernetes-side cause (OOM kill, eviction, image pull, "+
						"config or volume error) applies, so the init process itself failed - and the pod cannot start "+
						"its app containers until this init container exits 0.",
					in.Container, t.ExitCode)
			} else {
				expl = fmt.Sprintf(
					"%s exited with code %d on its own. No Kubernetes-side cause (OOM kill, probe failure, eviction, "+
						"image pull, config or volume error) applies, so this is the application's own failure.",
					capitalizeFirst(containerRef(in)), t.ExitCode)
			}
			switch {
			case t.ExitCode == 127:
				expl += " Exit code 127 means the container's command was not found: check the entrypoint path, " +
					"a missing binary, or a distroless/scratch image without a shell."
			case t.ExitCode == 126:
				expl += " Exit code 126 means the command was found but could not be executed: a missing execute bit, " +
					"a wrong interpreter line, or a CPU-architecture mismatch."
			case t.ExitCode == 139 || t.Signal == 11:
				expl += " Exit code 139 is SIGSEGV: the process segfaulted, which usually means a native-code or " +
					"architecture problem rather than a normal application error."
			case sigterm:
				expl += " Exit code 143 is SIGTERM, and the pod has no deletionTimestamp and its owner is not rolling: " +
					"something inside the pod sent SIGTERM to the process, so this was not a Kubernetes-initiated stop."
			}
			if len(hints) > 0 {
				expl += fmt.Sprintf(" The log tail shows %s.", hints[0].Note)
			}
			d.Explanation = expl

			appendEvidence(d, terminationEvidence(t)...)
			appendEvidence(d, restartEvidence(in)...)
			if in.Waiting.Present && in.Waiting.Reason != "" {
				appendEvidence(d, "waiting reason: "+in.Waiting.Reason)
			}
			if sigterm {
				appendEvidence(d, "terminated by SIGTERM from inside the pod - not initiated by Kubernetes deletion")
				appendEvidence(d, "pod has no deletionTimestamp and its owner is not mid rolling-update")
			}
			for _, e := range eventsByReason(in, "BackOff") {
				appendEvidence(d, describeEvent(e))
			}
			switch {
			case in.LogsUnavailable:
				appendEvidence(d, "logs were unavailable (collection disabled, rate-limited, or failed): this diagnosis "+
					"is based on the exit code and events only")
			case len(in.LogTail) == 0:
				appendEvidence(d, "log tail was empty: this diagnosis is based on the exit code and events only")
			default:
				appendEvidence(d, fmt.Sprintf("inspected %d lines of the previous container's log tail", len(in.LogTail)))
			}
			for _, h := range hints {
				appendEvidence(d, fmt.Sprintf("log hint (%s): %s", h.Label, truncateMessage(h.Line)))
			}
			if len(hints) == 0 && !in.LogsUnavailable && len(in.LogTail) > 0 {
				appendEvidence(d, "last log line: "+truncateMessage(in.LogTail[len(in.LogTail)-1]))
				appendEvidence(d, "no known crash pattern matched the log tail")
			}

			appendSteps(d, logsCommand(in, true)+"    # the crashed instance's logs, not the current one")
			for _, h := range hints {
				appendSteps(d, h.Steps...)
			}
			switch {
			case t.ExitCode == 127:
				appendSteps(d, "Verify the image's ENTRYPOINT/CMD and any command:/args: override in the pod spec",
					"docker run --rm --entrypoint sh "+in.Image+" -c 'ls -l <path>'    # check the binary exists in the image")
			case t.ExitCode == 126:
				appendSteps(d, "chmod +x the entrypoint in the Dockerfile, and check the image architecture matches the nodes")
			case sigterm:
				appendSteps(d, "Look for an in-container supervisor, sidecar or health script that sends SIGTERM to PID 1")
			}
			if in.LogsUnavailable {
				appendSteps(d, "Enable log collection (RBAC pods/log, or logCollection.enabled=true in the Helm chart) "+
					"to get log-pattern hints")
			}
			appendSteps(d, fmt.Sprintf("crashcause inspect %s %s%s --ai    # AI summary of the application error itself",
				podRef(in), nsFlag(in), containerFlag(in)))
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 15. sigkill_after_grace (app containers only)
// ---------------------------------------------------------------------------

func sigkillAfterGraceRule() Rule {
	return Rule{
		Cause:     CauseSigkillAfterGrace,
		AppliesTo: appOnly(),
		Match: func(in Inputs) *Diagnosis {
			if !in.Deleting || !sigkillTermination(in) || oomMatched(in) {
				return nil
			}
			t := effectiveTermination(in)
			d := newDiagnosis(in, CauseSigkillAfterGrace, ConfidenceHigh)
			d.Explanation = fmt.Sprintf(
				"The pod is being deleted and %s was killed with SIGKILL (exit 137): the termination grace period "+
					"expired before the process exited, so the kubelet escalated from SIGTERM to SIGKILL. The "+
					"application did not shut down on SIGTERM in time - in-flight requests and unflushed state were lost.",
				containerRef(in))
			appendEvidence(d, "pod is being deleted (deletionTimestamp is set)")
			if !in.DeletionTime.IsZero() {
				appendEvidence(d, "deletionTimestamp: "+in.DeletionTime.UTC().Format(time.RFC3339))
			}
			if in.OwnerRolling {
				appendEvidence(d, "the owning workload is mid rolling-update or scale-down")
			}
			appendEvidence(d, terminationEvidence(t)...)
			appendEvidence(d, "SIGKILL after a deletion request means the grace period expired, not that the process crashed")
			appendEvidence(d, restartEvidence(in)...)
			for _, e := range eventsByReason(in, "Killing") {
				appendEvidence(d, describeEvent(e))
			}
			appendSteps(d,
				"Handle SIGTERM in the application: stop accepting new work, drain in-flight requests, then exit",
				"Raise spec.terminationGracePeriodSeconds above the application's real drain time",
				"Check PID 1: a shell entrypoint without 'exec' swallows SIGTERM and never forwards it to the process",
				"Add a preStop hook if the app needs time to deregister from a load balancer before shutdown",
				logsCommand(in, true)+"    # did the app log anything after receiving SIGTERM?",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 16. completed_restart_loop (app containers only; exit 0 on an init
// container is SUCCESS, never a finding)
// ---------------------------------------------------------------------------

func completedRestartLoopRule() Rule {
	return Rule{
		Cause:     CauseCompletedRestartLoop,
		AppliesTo: appOnly(),
		Match: func(in Inputs) *Diagnosis {
			t := effectiveTermination(in)
			if !t.Present || t.ExitCode != 0 {
				return nil
			}
			if !strings.EqualFold(in.RestartPolicy, "Always") || in.RestartCount <= 0 {
				return nil
			}
			d := newDiagnosis(in, CauseCompletedRestartLoop, ConfidenceHigh)
			d.Explanation = fmt.Sprintf(
				"%s ran to completion successfully (exit code 0) but the pod's restartPolicy is Always, so Kubernetes "+
					"restarts it every time it finishes. This is a run-to-completion workload deployed as a long-running "+
					"one: the restarts (and any CrashLoopBackOff) are the deployment shape, not an application failure.",
				capitalizeFirst(containerRef(in)))
			appendEvidence(d, terminationEvidence(t)...)
			appendEvidence(d, restartEvidence(in)...)
			if t.Reason != "" && !strings.EqualFold(t.Reason, "Completed") {
				appendEvidence(d, "note: exit code 0 with reason "+t.Reason)
			}
			if in.Waiting.Present && in.Waiting.Reason != "" {
				appendEvidence(d, "waiting reason: "+in.Waiting.Reason)
			}
			appendSteps(d,
				"If this is a batch task, run it as a Job or CronJob instead of a Deployment",
				"If it must stay a pod, set restartPolicy: OnFailure (or Never) so a successful exit is final",
				"If it was meant to be long-running, find why the process returns instead of blocking "+
					"(a missing foreground flag, a daemonized process, or an entrypoint that exits after setup)",
				logsCommand(in, true)+"    # confirm the container did the work it was supposed to do",
			)
			return d
		},
	}
}

// ---------------------------------------------------------------------------
// 17. unknown (fallback)
// ---------------------------------------------------------------------------

// somethingIsWrong reports whether the inputs show evidence of a problem at
// all. A healthy container must never produce a diagnosis - not even `unknown`.
func somethingIsWrong(in Inputs) bool {
	t := effectiveTermination(in)
	switch {
	case in.RestartCount > 0:
		return true
	case t.Present && (t.ExitCode != 0 || t.Signal != 0):
		return true
	case in.ActiveDeadlineExceeded:
		return true
	case strings.EqualFold(in.PodPhase, "Failed"):
		return true
	case in.PodReason != "":
		return true
	case in.Waiting.Present && isProblemWaitingReason(in.Waiting.Reason):
		return true
	}
	return false
}

// isProblemWaitingReason filters out the transient, normal waiting reasons.
func isProblemWaitingReason(reason string) bool {
	switch strings.ToLower(reason) {
	case "", "containercreating", "podinitializing":
		return false
	default:
		return true
	}
}

func unknownRule() Rule {
	return Rule{
		Cause:     CauseUnknown,
		AppliesTo: allKinds(),
		Match: func(in Inputs) *Diagnosis {
			if !somethingIsWrong(in) {
				return nil
			}
			kind := effectiveKind(in)
			for _, r := range specificRules() {
				if !r.appliesToKind(kind) {
					continue
				}
				if r.Match(in) != nil {
					return nil
				}
			}
			d := newDiagnosis(in, CauseUnknown, ConfidenceLow)
			d.Explanation = fmt.Sprintf(
				"Something is wrong with %s, but no rule matched confidently: the collected exit codes, events and "+
					"logs do not fit any known crash signature. Everything the collectors saw is listed below.",
				containerRef(in))
			appendEvidence(d, terminationEvidence(effectiveTermination(in))...)
			appendEvidence(d, restartEvidence(in)...)
			if in.Waiting.Present {
				appendEvidence(d, "waiting reason: "+in.Waiting.Reason)
				if m := truncateMessage(in.Waiting.Message); m != "" {
					appendEvidence(d, "waiting message: "+m)
				}
			}
			if in.PodPhase != "" {
				appendEvidence(d, "pod phase: "+in.PodPhase)
			}
			if in.PodReason != "" {
				appendEvidence(d, "pod status reason: "+in.PodReason)
			}
			if m := truncateMessage(in.PodMessage); m != "" {
				appendEvidence(d, "pod status message: "+m)
			}
			if in.ActiveDeadlineExceeded {
				appendEvidence(d, "pod activeDeadlineSeconds exceeded")
			}
			if in.Running.Present {
				appendEvidence(d, "container is currently running (for "+formatDuration(in.RunningDuration)+")")
			}
			for _, e := range in.Events {
				if strings.EqualFold(e.Type, "Warning") {
					appendEvidence(d, describeEvent(e))
				}
			}
			appendEvidence(d, nodePressureEvidence(in)...)
			for _, h := range scanLogTail(in.LogTail) {
				appendEvidence(d, fmt.Sprintf("log hint (%s): %s", h.Label, truncateMessage(h.Line)))
			}
			if in.LogsUnavailable {
				appendEvidence(d, "logs were unavailable (collection disabled, rate-limited, or failed)")
			}
			appendSteps(d,
				describeCommand(in),
				logsCommand(in, true),
				"kubectl get events "+nsFlag(in)+" --sort-by=.lastTimestamp",
				fmt.Sprintf("crashcause inspect %s %s%s --ai    # let the AI layer read the log tail", podRef(in), nsFlag(in), containerFlag(in)),
				"If this turns out to be a recurring signature, it is a good candidate for a new rule - please open an issue",
			)
			return d
		},
	}
}
