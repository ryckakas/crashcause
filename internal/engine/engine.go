package engine

// Rule is one classification rule: if Match returns a non-nil Diagnosis for
// a given Inputs, the rule fired.
type Rule struct {
	Cause     CauseCode
	AppliesTo []ContainerKind
	Match     func(Inputs) *Diagnosis
}

// Rules returns the ordered rule table (priority order per spec §4.2).
func Rules() []Rule {
	// TODO(milestone-2): implement the ordered rule table per spec §4.2.
	return nil
}

// Classify runs all applicable rules. Returned slice is priority-ordered;
// index 0 is the primary diagnosis. Empty slice means "nothing wrong /
// normal lifecycle" (e.g. exit 143 during deletion) — callers must treat
// empty as no-finding, NOT as unknown.
func Classify(in Inputs) []Diagnosis {
	// TODO(milestone-2): implement rule evaluation over Rules().
	return nil
}
