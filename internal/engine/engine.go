package engine

import "time"

// defaultInitStuckThreshold is the spec default (§4.2) for how long an init
// container may run before it is reported as stuck. It is applied whenever
// Inputs.InitStuckThreshold is zero-valued, so a caller that forgot to plumb
// the flag through can never accidentally disable the rule.
const defaultInitStuckThreshold = 10 * time.Minute

// Rule is one classification rule: if Match returns a non-nil Diagnosis for
// a given Inputs, the rule fired.
type Rule struct {
	Cause     CauseCode
	AppliesTo []ContainerKind
	Match     func(Inputs) *Diagnosis
}

// appliesToKind reports whether the rule may fire when diagnosing a container
// of the given kind. An empty AppliesTo means "every container kind".
func (r Rule) appliesToKind(kind ContainerKind) bool {
	if len(r.AppliesTo) == 0 {
		return true
	}
	for _, k := range r.AppliesTo {
		if k == kind {
			return true
		}
	}
	return false
}

// allKinds is the applicability set for pod-level rules.
func allKinds() []ContainerKind {
	return []ContainerKind{KindApp, KindInit, KindEphemeral}
}

// appOnly is the applicability set for rules that make no sense for init or
// ephemeral containers (probes, graceful-shutdown semantics, restart loops,
// and the "an init container is blocking me" rule itself).
func appOnly() []ContainerKind {
	return []ContainerKind{KindApp}
}

// Rules returns the ordered rule table (priority order per spec §4.2).
//
// Order is the spec table order, and it is load-bearing: Classify walks it
// top to bottom and the caller renders index 0 by default. The `unknown`
// fallback is always last and only fires when no other applicable rule did.
func Rules() []Rule {
	return append(specificRules(), unknownRule())
}

// specificRules is the rule table without the `unknown` fallback. It exists
// so the fallback can ask "did anything else match?" without recursing.
func specificRules() []Rule {
	return []Rule{
		oomKilledRule(),                // 1
		sigkillUnattributedRule(),      // 2
		evictedRule(),                  // 3
		probeLivenessRule(),            // 4
		probeStartupRule(),             // 5
		imagePullAuthRule(),            // 6
		imagePullNotFoundRule(),        // 7
		imagePullOtherRule(),           // 8
		securityContextViolationRule(), // 9
		configMissingRefRule(),         // 10
		volumeMountFailureRule(),       // 11
		initContainerFailureRule(),     // 12
		initContainerStuckRule(),       // 13
		unschedulableRule(),            // 14
		appExitNonzeroRule(),           // 15
		sigkillAfterGraceRule(),        // 16
		completedRestartLoopRule(),     // 17
	}
}

// Classify runs all applicable rules. Returned slice is priority-ordered;
// index 0 is the primary diagnosis. Empty slice means "nothing wrong /
// normal lifecycle" (e.g. exit 143 during deletion) — callers must treat
// empty as no-finding, NOT as unknown.
func Classify(in Inputs) []Diagnosis {
	in = normalizeInputs(in)

	// Lifecycle-noise guard (spec §4.2 / decision log #7): a SIGTERM
	// termination inside a deletion or rolling-update context is normal
	// Kubernetes behavior, not a finding. Suppressing it here rather than
	// in a rule keeps every deploy from producing a stream of false crashes.
	if isLifecycleNoise(in) {
		return nil
	}

	kind := effectiveKind(in)
	var out []Diagnosis
	for _, r := range Rules() {
		if !r.appliesToKind(kind) {
			continue
		}
		d := r.Match(in)
		if d == nil {
			continue
		}
		finalize(d, in)
		out = append(out, *d)
	}
	return promotePrimary(out)
}

// normalizeInputs applies engine-side defaults so that a zero-valued field
// never changes classification behavior in a surprising way.
func normalizeInputs(in Inputs) Inputs {
	in.Kind = effectiveKind(in)
	if in.InitStuckThreshold <= 0 {
		in.InitStuckThreshold = defaultInitStuckThreshold
	}
	return in
}

// isLifecycleNoise reports whether the container's termination is ordinary
// pod-lifecycle churn that must produce no diagnosis at all.
func isLifecycleNoise(in Inputs) bool {
	if !in.Deleting && !in.OwnerRolling {
		return false
	}
	t := effectiveTermination(in)
	if !t.Present {
		return false
	}
	return t.ExitCode == 143 || t.Signal == 15
}

// finalize stamps pod identity and the diagnosis timestamp onto a Diagnosis.
func finalize(d *Diagnosis, in Inputs) {
	d.Pod = in.Pod
	d.Namespace = in.Namespace
	d.Container = in.Container
	d.Owner = in.Owner
	if !in.Now.IsZero() {
		d.Timestamp = in.Now
	} else if d.Timestamp.IsZero() {
		d.Timestamp = time.Now()
	}
}

// promotePrimary moves the first high-confidence diagnosis to index 0 while
// preserving the priority order of every other element. Callers render [0] by
// default and the whole slice under --verbose.
func promotePrimary(ds []Diagnosis) []Diagnosis {
	if len(ds) < 2 {
		return ds
	}
	idx := -1
	for i, d := range ds {
		if d.Confidence == ConfidenceHigh {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return ds
	}
	out := make([]Diagnosis, 0, len(ds))
	out = append(out, ds[idx])
	out = append(out, ds[:idx]...)
	out = append(out, ds[idx+1:]...)
	return out
}

// newDiagnosis builds a Diagnosis pre-filled with pod identity so that rules
// are also useful when matched in isolation (the spec's testing seam).
func newDiagnosis(in Inputs, cause CauseCode, conf Confidence) *Diagnosis {
	return &Diagnosis{
		Cause:      cause,
		Confidence: conf,
		Container:  in.Container,
		Pod:        in.Pod,
		Namespace:  in.Namespace,
		Owner:      in.Owner,
		Timestamp:  in.Now,
	}
}

// appendEvidence appends non-empty evidence lines, skipping duplicates.
func appendEvidence(d *Diagnosis, lines ...string) {
	for _, l := range lines {
		if l == "" {
			continue
		}
		dup := false
		for _, existing := range d.Evidence {
			if existing == l {
				dup = true
				break
			}
		}
		if !dup {
			d.Evidence = append(d.Evidence, l)
		}
	}
}

// appendSteps appends non-empty next steps, skipping duplicates.
func appendSteps(d *Diagnosis, steps ...string) {
	for _, s := range steps {
		if s == "" {
			continue
		}
		dup := false
		for _, existing := range d.NextSteps {
			if existing == s {
				dup = true
				break
			}
		}
		if !dup {
			d.NextSteps = append(d.NextSteps, s)
		}
	}
}
