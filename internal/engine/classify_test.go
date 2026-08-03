package engine

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fixture helpers
// ---------------------------------------------------------------------------

var fixedNow = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

// baseInputs returns a healthy-looking app container in a Running pod.
func baseInputs() Inputs {
	return Inputs{
		Pod:           "api-7d9f8c6b4-abcde",
		Namespace:     "production",
		Container:     "api",
		Kind:          KindApp,
		Image:         "registry.example.com/team/api:1.4.2",
		PodPhase:      "Running",
		QOSClass:      "Burstable",
		RestartPolicy: "Always",
		Owner:         Owner{Kind: "Deployment", Name: "api"},
		Now:           fixedNow,
	}
}

func terminated(exit int32, reason string) TerminationState {
	return TerminationState{
		Present:    true,
		ExitCode:   exit,
		Reason:     reason,
		StartedAt:  fixedNow.Add(-5 * time.Minute),
		FinishedAt: fixedNow.Add(-1 * time.Minute),
	}
}

func warning(reason, message string, count int32) Event {
	return Event{
		Type:      "Warning",
		Reason:    reason,
		Message:   message,
		Count:     count,
		FirstSeen: fixedNow.Add(-10 * time.Minute),
		LastSeen:  fixedNow.Add(-1 * time.Minute),
	}
}

func causes(ds []Diagnosis) []CauseCode {
	out := make([]CauseCode, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Cause)
	}
	return out
}

func hasCause(ds []Diagnosis, c CauseCode) bool {
	for _, d := range ds {
		if d.Cause == c {
			return true
		}
	}
	return false
}

func diagnosisFor(t *testing.T, ds []Diagnosis, c CauseCode) Diagnosis {
	t.Helper()
	for _, d := range ds {
		if d.Cause == c {
			return d
		}
	}
	t.Fatalf("expected a %s diagnosis, got %v", c, causes(ds))
	return Diagnosis{}
}

func evidenceContains(d Diagnosis, substr string) bool {
	for _, e := range d.Evidence {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

func stepsContain(d Diagnosis, substr string) bool {
	for _, s := range d.NextSteps {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// matchRule runs a single rule's matcher, honoring its applicability filter,
// exactly as Classify would. It is the spec's "rule in isolation" seam.
func matchRule(cause CauseCode, in Inputs) *Diagnosis {
	in = normalizeInputs(in)
	for _, r := range Rules() {
		if r.Cause != cause {
			continue
		}
		if !r.appliesToKind(effectiveKind(in)) {
			return nil
		}
		return r.Match(in)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rule table sanity
// ---------------------------------------------------------------------------

func TestRulesTableIsCompleteAndOrdered(t *testing.T) {
	want := []CauseCode{
		CauseOOMKilled,
		CauseSigkillUnattributed,
		CauseEvicted,
		CauseProbeLiveness,
		CauseProbeStartup,
		CauseImagePullAuth,
		CauseImagePullNotFound,
		CauseImagePullOther,
		CauseSecurityContextViolation,
		CauseConfigMissingRef,
		CauseVolumeMountFailure,
		CauseInitContainerFailure,
		CauseInitContainerStuck,
		CauseUnschedulable,
		CauseAppExitNonzero,
		CauseSigkillAfterGrace,
		CauseCompletedRestartLoop,
		CauseUnknown,
	}
	got := Rules()
	if len(got) != len(want) {
		t.Fatalf("rule table has %d rules, want %d", len(got), len(want))
	}
	for i, c := range want {
		if got[i].Cause != c {
			t.Errorf("rule %d is %s, want %s", i, got[i].Cause, c)
		}
		if got[i].Match == nil {
			t.Errorf("rule %s has a nil matcher", c)
		}
		if len(got[i].AppliesTo) == 0 {
			t.Errorf("rule %s declares no container applicability", c)
		}
	}
	// Every declared cause code must exist in the table exactly once.
	seen := map[CauseCode]int{}
	for _, r := range got {
		seen[r.Cause]++
	}
	for _, c := range AllCauses() {
		if seen[c] != 1 {
			t.Errorf("cause %s appears %d times in the rule table, want 1", c, seen[c])
		}
	}
}

func TestAppOnlyRulesNeverApplyToInitContainers(t *testing.T) {
	appOnlyCauses := map[CauseCode]bool{
		CauseProbeLiveness:        true,
		CauseProbeStartup:         true,
		CauseSigkillAfterGrace:    true,
		CauseCompletedRestartLoop: true,
		CauseInitContainerFailure: true,
	}
	for _, r := range Rules() {
		applies := r.appliesToKind(KindInit)
		if appOnlyCauses[r.Cause] && applies {
			t.Errorf("rule %s must not apply to init containers", r.Cause)
		}
		if r.Cause == CauseInitContainerStuck && r.appliesToKind(KindApp) {
			t.Errorf("rule %s must not apply to app containers", r.Cause)
		}
	}
}

// ---------------------------------------------------------------------------
// Applicability: probe / grace / completed-loop rules must not fire on init
// (spec §9)
// ---------------------------------------------------------------------------

func TestInitContainerDoesNotGetAppOnlyDiagnoses(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*Inputs)
		forbidden CauseCode
	}{
		{
			name: "liveness probe events on an init container",
			mutate: func(in *Inputs) {
				in.RestartCount = 4
				in.LastTermination = terminated(137, "Error")
				in.Events = []Event{
					warning("Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500", 12),
					warning("Killing", "Container init-db failed liveness probe, will be restarted", 3),
				}
				in.Liveness = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 10}
			},
			forbidden: CauseProbeLiveness,
		},
		{
			name: "startup probe events on an init container",
			mutate: func(in *Inputs) {
				in.RestartCount = 2
				in.LastTermination = terminated(137, "Error")
				in.Events = []Event{warning("Unhealthy", "Startup probe failed: connection refused", 9)}
				in.Startup = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 5}
			},
			forbidden: CauseProbeStartup,
		},
		{
			name: "sigkill after grace on an init container",
			mutate: func(in *Inputs) {
				in.Deleting = true
				in.DeletionTime = fixedNow.Add(-2 * time.Minute)
				in.LastTermination = terminated(137, "Error")
			},
			forbidden: CauseSigkillAfterGrace,
		},
		{
			name: "exit 0 on an init container is success, never a restart loop",
			mutate: func(in *Inputs) {
				in.RestartCount = 3
				in.RestartPolicy = "Always"
				in.LastTermination = terminated(0, "Completed")
			},
			forbidden: CauseCompletedRestartLoop,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.Kind = KindInit
			in.Container = "init-db"
			in.PodPhase = "Pending"
			tc.mutate(&in)

			got := Classify(in)
			if hasCause(got, tc.forbidden) {
				t.Fatalf("rule %s fired for an init container: got %v", tc.forbidden, causes(got))
			}

			// The same fixture as an APP container must produce the rule, so
			// the test proves applicability filtering and not a broken matcher.
			appIn := in
			appIn.Kind = KindApp
			if appGot := Classify(appIn); !hasCause(appGot, tc.forbidden) {
				t.Fatalf("control case failed: %s did not fire for the app-container variant (got %v)",
					tc.forbidden, causes(appGot))
			}
		})
	}
}

func TestInitContainerExitZeroProducesNoDiagnosis(t *testing.T) {
	in := baseInputs()
	in.Kind = KindInit
	in.Container = "init-db"
	in.PodPhase = "Running"
	in.LastTermination = terminated(0, "Completed")

	if got := Classify(in); len(got) != 0 {
		t.Fatalf("a successful init container must produce no diagnosis, got %v", causes(got))
	}
}

// ---------------------------------------------------------------------------
// Lifecycle noise and the SIGKILL family (spec §9, decision log #1 and #7)
// ---------------------------------------------------------------------------

func TestSigkillFamilyAndLifecycleNoise(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*Inputs)
		wantCauses []CauseCode // nil means "no diagnosis at all"
		wantConf   Confidence
	}{
		{
			name: "exit 143 while deleting is normal lifecycle",
			mutate: func(in *Inputs) {
				in.Deleting = true
				in.DeletionTime = fixedNow.Add(-30 * time.Second)
				in.LastTermination = terminated(143, "Error")
			},
			wantCauses: nil,
		},
		{
			name: "signal 15 while deleting is normal lifecycle",
			mutate: func(in *Inputs) {
				in.Deleting = true
				in.LastTermination = TerminationState{Present: true, Signal: 15, Reason: "Error"}
			},
			wantCauses: nil,
		},
		{
			name: "exit 143 during a rolling update is normal lifecycle",
			mutate: func(in *Inputs) {
				in.OwnerRolling = true
				in.RestartCount = 3
				in.LastTermination = terminated(143, "Error")
				in.Events = []Event{
					{Type: "Normal", Reason: "Killing", Message: "Stopping container api", Count: 3},
				}
			},
			wantCauses: nil,
		},
		{
			name: "exit 137 while deleting is grace-period expiry",
			mutate: func(in *Inputs) {
				in.Deleting = true
				in.DeletionTime = fixedNow.Add(-45 * time.Second)
				in.LastTermination = terminated(137, "Error")
			},
			wantCauses: []CauseCode{CauseSigkillAfterGrace},
			wantConf:   ConfidenceHigh,
		},
		{
			name: "exit 137 alone is an unattributed SIGKILL",
			mutate: func(in *Inputs) {
				in.RestartCount = 2
				in.LastTermination = terminated(137, "Error")
				in.Events = []Event{warning("BackOff", "Back-off restarting failed container api in pod api-7d9f8c6b4-abcde", 4)}
			},
			wantCauses: []CauseCode{CauseSigkillUnattributed},
			wantConf:   ConfidenceMedium,
		},
		{
			name: "exit 137 with no events at all drops to low confidence",
			mutate: func(in *Inputs) {
				in.RestartCount = 1
				in.LastTermination = terminated(137, "Error")
			},
			wantCauses: []CauseCode{CauseSigkillUnattributed},
			wantConf:   ConfidenceLow,
		},
		{
			name: "signal 9 without exit 137 is still an unattributed SIGKILL",
			mutate: func(in *Inputs) {
				in.RestartCount = 1
				in.LastTermination = TerminationState{Present: true, ExitCode: 1, Signal: 9, Reason: "Error"}
				in.Events = []Event{warning("BackOff", "Back-off restarting failed container", 2)}
			},
			wantCauses: []CauseCode{CauseSigkillUnattributed},
			wantConf:   ConfidenceMedium,
		},
		{
			name: "reason OOMKilled wins over exit 137",
			mutate: func(in *Inputs) {
				in.RestartCount = 5
				in.LastTermination = terminated(137, "OOMKilled")
				in.Limits = map[string]string{"memory": "128Mi"}
				in.Requests = map[string]string{"memory": "64Mi"}
			},
			wantCauses: []CauseCode{CauseOOMKilled},
			wantConf:   ConfidenceHigh,
		},
		{
			name: "OOMKilled while deleting is still an OOM kill",
			mutate: func(in *Inputs) {
				in.Deleting = true
				in.LastTermination = terminated(137, "OOMKilled")
			},
			wantCauses: []CauseCode{CauseOOMKilled},
			wantConf:   ConfidenceHigh,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			tc.mutate(&in)
			got := Classify(in)

			if tc.wantCauses == nil {
				if len(got) != 0 {
					t.Fatalf("want zero diagnoses, got %v", causes(got))
				}
				return
			}
			if len(got) != len(tc.wantCauses) {
				t.Fatalf("got causes %v, want exactly %v", causes(got), tc.wantCauses)
			}
			for i, c := range tc.wantCauses {
				if got[i].Cause != c {
					t.Fatalf("diagnosis %d is %s, want %s (all: %v)", i, got[i].Cause, c, causes(got))
				}
			}
			if tc.wantConf != "" && got[0].Confidence != tc.wantConf {
				t.Errorf("confidence is %s, want %s", got[0].Confidence, tc.wantConf)
			}
		})
	}
}

func TestOOMEvidenceCitesSpecLimitsAndNeverUsage(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 6
	in.LastTermination = terminated(137, "OOMKilled")
	in.Limits = map[string]string{"memory": "128Mi", "cpu": "500m"}
	in.Requests = map[string]string{"memory": "128Mi"}
	in.QOSClass = "Guaranteed"

	d := diagnosisFor(t, Classify(in), CauseOOMKilled)
	if !evidenceContains(d, "memory limit was 128Mi") {
		t.Errorf("evidence must cite the spec memory limit, got %v", d.Evidence)
	}
	if !evidenceContains(d, "OOMKilled") {
		t.Errorf("evidence must cite the OOMKilled reason, got %v", d.Evidence)
	}
	for _, e := range d.Evidence {
		low := strings.ToLower(e)
		for _, banned := range []string{"usage", "was using", "consumed", "near the limit", "utilization"} {
			if strings.Contains(low, banned) {
				t.Errorf("evidence makes a usage claim (%q): %q", banned, e)
			}
		}
	}
	if !stepsContain(d, "limits.memory") {
		t.Errorf("next steps must mention the memory limit, got %v", d.NextSteps)
	}
}

func TestSigkillUnattributedCitesNodeMemoryPressure(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 1
	in.LastTermination = terminated(137, "Error")
	in.Events = []Event{warning("BackOff", "Back-off restarting failed container", 3)}
	in.Node = NodeConditions{Known: true, MemoryPressure: true}

	d := diagnosisFor(t, Classify(in), CauseSigkillUnattributed)
	if !evidenceContains(d, "MemoryPressure=true") {
		t.Errorf("evidence must cite node MemoryPressure, got %v", d.Evidence)
	}
	if !strings.Contains(d.Explanation, "MemoryPressure=true") {
		t.Errorf("explanation must cite node MemoryPressure, got %q", d.Explanation)
	}
	if !strings.Contains(strings.ToLower(d.Explanation), "external sigkill") {
		t.Errorf("explanation must enumerate the external-SIGKILL suspect, got %q", d.Explanation)
	}
	if !stepsContain(d, "dmesg") {
		t.Errorf("next steps must point at dmesg on the node, got %v", d.NextSteps)
	}
	if !stepsContain(d, "describe node") {
		t.Errorf("next steps must point at node conditions, got %v", d.NextSteps)
	}
}

func TestSigkillAfterGraceNextSteps(t *testing.T) {
	in := baseInputs()
	in.Deleting = true
	in.DeletionTime = fixedNow.Add(-1 * time.Minute)
	in.LastTermination = terminated(137, "Error")

	d := diagnosisFor(t, Classify(in), CauseSigkillAfterGrace)
	if !stepsContain(d, "SIGTERM") {
		t.Errorf("next steps must tell the user to handle SIGTERM, got %v", d.NextSteps)
	}
	if !stepsContain(d, "terminationGracePeriodSeconds") {
		t.Errorf("next steps must offer raising terminationGracePeriodSeconds, got %v", d.NextSteps)
	}
	if !evidenceContains(d, "deletionTimestamp") {
		t.Errorf("evidence must cite the deletionTimestamp, got %v", d.Evidence)
	}
}

// ---------------------------------------------------------------------------
// Unschedulable (spec §9: works with zero logs)
// ---------------------------------------------------------------------------

func TestUnschedulableVariants(t *testing.T) {
	tests := []struct {
		name         string
		message      string
		wantInExpl   []string
		wantEvidence string
	}{
		{
			name: "insufficient cpu",
			message: "0/5 nodes are available: 3 Insufficient cpu, 2 Insufficient memory. " +
				"preemption: 0/5 nodes are available: 5 No preemption victims found for incoming pod.",
			wantInExpl:   []string{"cpu", "memory", "allocatable"},
			wantEvidence: "insufficient node resource: cpu",
		},
		{
			name:         "untolerated taints",
			message:      "0/3 nodes are available: 3 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }.",
			wantInExpl:   []string{"taint"},
			wantEvidence: "untolerated taint: node-role.kubernetes.io/control-plane",
		},
		{
			name:         "node affinity mismatch",
			message:      "0/4 nodes are available: 4 node(s) didn't match Pod's node affinity/selector.",
			wantInExpl:   []string{"node affinity"},
			wantEvidence: "pod phase: Pending",
		},
		{
			name:         "volume zone conflict",
			message:      "0/6 nodes are available: 6 node(s) had volume node affinity conflict.",
			wantInExpl:   []string{"volume"},
			wantEvidence: "pod phase: Pending",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.PodPhase = "Pending"
			in.LogsUnavailable = true // no logs exist for a pod that never ran
			in.Requests = map[string]string{"cpu": "32", "memory": "64Gi"}
			in.Events = []Event{warning("FailedScheduling", tc.message, 3)}

			got := Classify(in)
			d := diagnosisFor(t, got, CauseUnschedulable)
			if d.Confidence != ConfidenceHigh {
				t.Errorf("confidence is %s, want high", d.Confidence)
			}
			for _, want := range tc.wantInExpl {
				if !strings.Contains(strings.ToLower(d.Explanation), strings.ToLower(want)) {
					t.Errorf("explanation %q does not mention %q", d.Explanation, want)
				}
			}
			if !evidenceContains(d, tc.wantEvidence) {
				t.Errorf("evidence %v does not contain %q", d.Evidence, tc.wantEvidence)
			}
			if !evidenceContains(d, "container requests: cpu=32") {
				t.Errorf("evidence must include the pod's requests, got %v", d.Evidence)
			}
			if len(d.NextSteps) == 0 {
				t.Error("unschedulable must always suggest next steps")
			}
		})
	}
}

func TestPendingWithoutFailedSchedulingIsNotUnschedulable(t *testing.T) {
	in := baseInputs()
	in.PodPhase = "Pending"
	in.Waiting = WaitingState{Present: true, Reason: "ContainerCreating"}

	if got := Classify(in); len(got) != 0 {
		t.Fatalf("a normally-starting pod must produce no diagnosis, got %v", causes(got))
	}
}

// ---------------------------------------------------------------------------
// Init container stuck (spec §9: 15m over a 10m threshold fires, 5m does not)
// ---------------------------------------------------------------------------

func TestInitContainerStuckThreshold(t *testing.T) {
	tests := []struct {
		name      string
		running   time.Duration
		threshold time.Duration
		deadline  bool
		want      bool
		wantConf  Confidence
	}{
		{name: "15m over a 10m threshold", running: 15 * time.Minute, threshold: 10 * time.Minute, want: true, wantConf: ConfidenceMedium},
		{name: "5m under a 10m threshold", running: 5 * time.Minute, threshold: 10 * time.Minute, want: false},
		{name: "exactly at the threshold does not fire", running: 10 * time.Minute, threshold: 10 * time.Minute, want: false},
		{name: "zero threshold falls back to the 10m default (15m fires)", running: 15 * time.Minute, threshold: 0, want: true, wantConf: ConfidenceMedium},
		{name: "zero threshold falls back to the 10m default (5m does not)", running: 5 * time.Minute, threshold: 0, want: false},
		{name: "activeDeadlineSeconds exceeded fires regardless of duration", running: time.Minute, threshold: 10 * time.Minute, deadline: true, want: true, wantConf: ConfidenceHigh},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.Kind = KindInit
			in.Container = "wait-for-db"
			in.PodPhase = "Pending"
			in.Running = RunningState{Present: true, StartedAt: fixedNow.Add(-tc.running)}
			in.RunningDuration = tc.running
			in.InitStuckThreshold = tc.threshold
			in.ActiveDeadlineExceeded = tc.deadline

			got := Classify(in)
			if !tc.want {
				if len(got) != 0 {
					t.Fatalf("want no diagnosis, got %v", causes(got))
				}
				return
			}
			d := diagnosisFor(t, got, CauseInitContainerStuck)
			if d.Confidence != tc.wantConf {
				t.Errorf("confidence is %s, want %s", d.Confidence, tc.wantConf)
			}
			if !tc.deadline && !evidenceContains(d, "10m0s") {
				t.Errorf("evidence must state the threshold, got %v", d.Evidence)
			}
			if tc.deadline && !evidenceContains(d, "activeDeadlineSeconds") {
				t.Errorf("evidence must cite the exceeded deadline, got %v", d.Evidence)
			}
		})
	}
}

func TestInitStuckRuleNeverFiresForAppContainers(t *testing.T) {
	in := baseInputs()
	in.Kind = KindApp
	in.Running = RunningState{Present: true, StartedAt: fixedNow.Add(-3 * time.Hour)}
	in.RunningDuration = 3 * time.Hour

	if got := Classify(in); hasCause(got, CauseInitContainerStuck) {
		t.Fatalf("init_container_stuck must not fire for app containers, got %v", causes(got))
	}
}

// ---------------------------------------------------------------------------
// config_missing_reference: extract WHICH object from real kubelet messages
// ---------------------------------------------------------------------------

func TestConfigMissingReferenceExtraction(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantName string
		wantKey  string
		wantCmd  string
	}{
		{
			name:     "missing secret",
			message:  `secret "db-credentials" not found`,
			wantName: "db-credentials",
			wantCmd:  "kubectl get secret db-credentials -n production",
		},
		{
			name:     "missing configmap",
			message:  `configmap "app-config" not found`,
			wantName: "app-config",
			wantCmd:  "kubectl get configmap app-config -n production",
		},
		{
			name:     "missing key in a secret",
			message:  `couldn't find key DB_PASSWORD in Secret production/db-credentials`,
			wantName: "db-credentials",
			wantKey:  "DB_PASSWORD",
			wantCmd:  "kubectl get secret db-credentials -n production",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.PodPhase = "Pending"
			in.Waiting = WaitingState{Present: true, Reason: "CreateContainerConfigError", Message: tc.message}
			in.Events = []Event{warning("Failed", "Error: "+tc.message, 5)}

			d := diagnosisFor(t, Classify(in), CauseConfigMissingRef)
			if d.Confidence != ConfidenceHigh {
				t.Errorf("confidence is %s, want high", d.Confidence)
			}
			if !evidenceContains(d, tc.wantName) {
				t.Errorf("evidence must name %q, got %v", tc.wantName, d.Evidence)
			}
			if tc.wantKey != "" && !evidenceContains(d, tc.wantKey) {
				t.Errorf("evidence must name the missing key %q, got %v", tc.wantKey, d.Evidence)
			}
			if !strings.Contains(d.Explanation, tc.wantName) {
				t.Errorf("explanation must name %q, got %q", tc.wantName, d.Explanation)
			}
			if !stepsContain(d, tc.wantCmd) {
				t.Errorf("next steps must include %q, got %v", tc.wantCmd, d.NextSteps)
			}
		})
	}
}

// https://github.com/ryckakas/crashcause/issues/17: a CreateContainerConfigError
// caused by runAsNonRoot vs a root image was reported as image_pull_other,
// because the Failed event message happens to contain the word "image".
func TestSecurityContextConfigErrorIsNotImagePull(t *testing.T) {
	tests := []struct {
		name     string
		message  string
		wantExpl string
	}{
		{
			name:     "image runs as root",
			message:  "container has runAsNonRoot and image will run as root",
			wantExpl: "the image is configured to run as root",
		},
		{
			name:     "non-numeric user",
			message:  "container has runAsNonRoot and image has non-numeric user (nginx), cannot verify user is non-root",
			wantExpl: "non-numeric user",
		},
		{
			name:     "explicit runAsUser 0",
			message:  `container's runAsUser breaks non-root policy (pod: "runasnonroot-repro_default(1c6b8e2a)", container: nginx-proxy)`,
			wantExpl: "sets runAsUser to root",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.Container = "nginx-proxy"
			in.Image = "nginx:alpine"
			in.PodPhase = "Pending"
			in.Waiting = WaitingState{Present: true, Reason: "CreateContainerConfigError", Message: tc.message}
			in.Events = []Event{
				warning("Failed", "Error: "+tc.message, 8480),
				{Type: "Normal", Reason: "Pulled", Message: `Container image "nginx:alpine" already present on machine`, Count: 8479},
			}

			ds := Classify(in)
			if len(ds) == 0 {
				t.Fatal("expected a diagnosis")
			}
			for _, c := range []CauseCode{CauseImagePullAuth, CauseImagePullNotFound, CauseImagePullOther} {
				if hasCause(ds, c) {
					t.Errorf("the image was pulled successfully, %s must not fire; got %v", c, causes(ds))
				}
			}
			if hasCause(ds, CauseConfigMissingRef) {
				t.Errorf("no ConfigMap/Secret reference is missing, config_missing_reference must stand down; got %v", causes(ds))
			}
			if ds[0].Cause != CauseSecurityContextViolation {
				t.Fatalf("primary diagnosis is %s, want %s (all: %v)", ds[0].Cause, CauseSecurityContextViolation, causes(ds))
			}
			if ds[0].Confidence != ConfidenceHigh {
				t.Errorf("confidence is %s, want high", ds[0].Confidence)
			}
			if !evidenceContains(ds[0], tc.message) {
				t.Errorf("evidence must quote the kubelet message %q, got %v", tc.message, ds[0].Evidence)
			}
			if !strings.Contains(ds[0].Explanation, tc.wantExpl) {
				t.Errorf("explanation must contain %q, got %q", tc.wantExpl, ds[0].Explanation)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// app_exit_nonzero: log-pattern hints and log degradation
// ---------------------------------------------------------------------------

func TestAppExitNonzeroLogHints(t *testing.T) {
	tests := []struct {
		name      string
		exit      int32
		logs      []string
		wantHint  string
		wantInEvd string
	}{
		{
			name:      "go panic",
			exit:      2,
			logs:      []string{"2026/08/02 11:59:58 starting", "panic: runtime error: invalid memory address or nil pointer dereference"},
			wantHint:  "go panic",
			wantInEvd: "panic: runtime error",
		},
		{
			name:      "connection refused",
			exit:      1,
			logs:      []string{"Error: connect ECONNREFUSED 10.2.3.4:5432"},
			wantHint:  "connection refused",
			wantInEvd: "ECONNREFUSED",
		},
		{
			name:      "address already in use",
			exit:      1,
			logs:      []string{"listen tcp :8080: bind: address already in use"},
			wantHint:  "port already in use",
			wantInEvd: "address already in use",
		},
		{
			name:      "permission denied",
			exit:      1,
			logs:      []string{"open /data/state.db: permission denied"},
			wantHint:  "permission denied",
			wantInEvd: "permission denied",
		},
		{
			name:      "jvm heap exhaustion",
			exit:      1,
			logs:      []string{"Exception in thread \"main\" java.lang.OutOfMemoryError: Java heap space"},
			wantHint:  "jvm out of memory",
			wantInEvd: "OutOfMemoryError",
		},
		{
			name:      "node module not found",
			exit:      1,
			logs:      []string{"Error: Cannot find module 'express'", "code: 'MODULE_NOT_FOUND'"},
			wantHint:  "node module not found",
			wantInEvd: "Cannot find module",
		},
		{
			name:      "node MODULE_NOT_FOUND code only",
			exit:      1,
			logs:      []string{"    code: 'MODULE_NOT_FOUND',"},
			wantHint:  "node module not found",
			wantInEvd: "MODULE_NOT_FOUND",
		},
		{
			name:      "java class not found",
			exit:      1,
			logs:      []string{"Caused by: java.lang.NoClassDefFoundError: org/slf4j/LoggerFactory"},
			wantHint:  "java class not found",
			wantInEvd: "NoClassDefFoundError",
		},
		{
			name:      "fatal log line",
			exit:      1,
			logs:      []string{"FATAL: could not connect to config server"},
			wantHint:  "fatal log line",
			wantInEvd: "FATAL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.RestartCount = 3
			in.LastTermination = terminated(tc.exit, "Error")
			in.LogTail = tc.logs

			d := diagnosisFor(t, Classify(in), CauseAppExitNonzero)
			if !evidenceContains(d, "log hint ("+tc.wantHint+")") {
				t.Errorf("evidence must carry the %q hint, got %v", tc.wantHint, d.Evidence)
			}
			if !evidenceContains(d, tc.wantInEvd) {
				t.Errorf("evidence must quote the matching log line (%q), got %v", tc.wantInEvd, d.Evidence)
			}
			if !strings.Contains(d.Explanation, "log tail shows") {
				t.Errorf("explanation must reflect the log hint, got %q", d.Explanation)
			}
			if d.Confidence != ConfidenceMedium {
				t.Errorf("confidence is %s, want medium", d.Confidence)
			}
		})
	}
}

func TestAppExitNonzeroSpecialExitCodes(t *testing.T) {
	tests := []struct {
		name     string
		term     TerminationState
		wantConf Confidence
		wantExpl string
	}{
		{name: "127 command not found", term: terminated(127, "Error"), wantConf: ConfidenceHigh, wantExpl: "not found"},
		{name: "126 not executable", term: terminated(126, "Error"), wantConf: ConfidenceHigh, wantExpl: "could not be executed"},
		{name: "139 segfault", term: terminated(139, "Error"), wantConf: ConfidenceHigh, wantExpl: "SIGSEGV"},
		{name: "143 outside deletion", term: terminated(143, "Error"), wantConf: ConfidenceLow, wantExpl: "not a Kubernetes-initiated stop"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.RestartCount = 2
			in.LastTermination = tc.term
			in.LogTail = []string{"starting up"}

			d := diagnosisFor(t, Classify(in), CauseAppExitNonzero)
			if d.Confidence != tc.wantConf {
				t.Errorf("confidence is %s, want %s", d.Confidence, tc.wantConf)
			}
			if !strings.Contains(d.Explanation, tc.wantExpl) {
				t.Errorf("explanation %q does not mention %q", d.Explanation, tc.wantExpl)
			}
		})
	}
}

func TestExit143OutsideDeletionFoldsIntoAppExitNonzero(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 1
	in.LastTermination = terminated(143, "Error")

	d := diagnosisFor(t, Classify(in), CauseAppExitNonzero)
	if d.Confidence != ConfidenceLow {
		t.Errorf("confidence is %s, want low", d.Confidence)
	}
	if !evidenceContains(d, "terminated by SIGTERM from inside the pod - not initiated by Kubernetes deletion") {
		t.Errorf("evidence must carry the exact SIGTERM-from-inside wording, got %v", d.Evidence)
	}
}

func TestAppExitNonzeroDegradesWithoutLogs(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 4
	in.LastTermination = terminated(1, "Error")
	in.LogsUnavailable = true

	d := diagnosisFor(t, Classify(in), CauseAppExitNonzero)
	if d.Confidence != ConfidenceLow {
		t.Errorf("confidence is %s, want low when logs are unavailable", d.Confidence)
	}
	if !evidenceContains(d, "exit code and events only") {
		t.Errorf("evidence must state the diagnosis is exit-code/event based, got %v", d.Evidence)
	}
	if !stepsContain(d, "logCollection.enabled=true") {
		t.Errorf("next steps must suggest enabling log collection, got %v", d.NextSteps)
	}
}

func TestAppExitNonzeroAdaptsWordingForInitContainers(t *testing.T) {
	in := baseInputs()
	in.Kind = KindInit
	in.Container = "run-migrations"
	in.PodPhase = "Pending"
	in.RestartCount = 3
	in.LastTermination = terminated(1, "Error")
	in.LogTail = []string{"migration failed: relation \"users\" already exists"}

	d := diagnosisFor(t, Classify(in), CauseAppExitNonzero)
	if !strings.Contains(d.Explanation, "Init container") {
		t.Errorf("explanation must be worded for an init container, got %q", d.Explanation)
	}
	if !strings.Contains(d.Explanation, "exits 0") {
		t.Errorf("explanation must say the pod waits for a zero exit, got %q", d.Explanation)
	}
}

func TestAppExitNonzeroStandsDownForKubernetesSideCauses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Inputs)
	}{
		{name: "oom", mutate: func(in *Inputs) { in.LastTermination = terminated(137, "OOMKilled") }},
		{name: "sigkill", mutate: func(in *Inputs) { in.LastTermination = terminated(137, "Error") }},
		{
			name: "liveness probe kill",
			mutate: func(in *Inputs) {
				in.LastTermination = terminated(143, "Error")
				in.Events = []Event{
					warning("Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 503", 6),
					warning("Killing", "Container api failed liveness probe, will be restarted", 2),
				}
			},
		},
		{
			name: "evicted",
			mutate: func(in *Inputs) {
				in.LastTermination = terminated(1, "Error")
				in.PodReason = "Evicted"
				in.PodMessage = "The node was low on resource: memory."
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.RestartCount = 2
			tc.mutate(&in)
			if got := Classify(in); hasCause(got, CauseAppExitNonzero) {
				t.Fatalf("app_exit_nonzero must stand down for %s, got %v", tc.name, causes(got))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// completed_restart_loop
// ---------------------------------------------------------------------------

func TestCompletedRestartLoop(t *testing.T) {
	tests := []struct {
		name    string
		policy  string
		exit    int32
		count   int32
		wantHit bool
	}{
		{name: "exit 0 + Always + restarts", policy: "Always", exit: 0, count: 4, wantHit: true},
		{name: "exit 0 + OnFailure is not a loop", policy: "OnFailure", exit: 0, count: 4, wantHit: false},
		{name: "exit 0 + Always without restarts yet", policy: "Always", exit: 0, count: 0, wantHit: false},
		{name: "exit 1 is not a completion", policy: "Always", exit: 1, count: 4, wantHit: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.RestartPolicy = tc.policy
			in.RestartCount = tc.count
			in.LastTermination = terminated(tc.exit, "Completed")
			in.Waiting = WaitingState{Present: true, Reason: "CrashLoopBackOff"}

			got := Classify(in)
			if hasCause(got, CauseCompletedRestartLoop) != tc.wantHit {
				t.Fatalf("completed_restart_loop presence = %v, want %v (got %v)",
					!tc.wantHit, tc.wantHit, causes(got))
			}
			if tc.wantHit {
				d := diagnosisFor(t, got, CauseCompletedRestartLoop)
				if d.Confidence != ConfidenceHigh {
					t.Errorf("confidence is %s, want high", d.Confidence)
				}
				if !stepsContain(d, "Job") {
					t.Errorf("next steps must suggest a Job/CronJob, got %v", d.NextSteps)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Classify plumbing: identity, ordering, primary promotion, healthy input
// ---------------------------------------------------------------------------

func TestClassifyFillsIdentityOnEveryDiagnosis(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 2
	in.LastTermination = terminated(137, "OOMKilled")

	got := Classify(in)
	if len(got) == 0 {
		t.Fatal("expected at least one diagnosis")
	}
	for _, d := range got {
		if d.Pod != in.Pod || d.Namespace != in.Namespace || d.Container != in.Container {
			t.Errorf("identity not filled on %s: %+v", d.Cause, d)
		}
		if d.Owner != in.Owner {
			t.Errorf("owner not filled on %s: %+v", d.Cause, d.Owner)
		}
		if !d.Timestamp.Equal(fixedNow) {
			t.Errorf("timestamp is %v, want the injected clock %v", d.Timestamp, fixedNow)
		}
	}
}

func TestClassifyFallsBackToWallClockWhenNowIsZero(t *testing.T) {
	in := baseInputs()
	in.Now = time.Time{}
	in.RestartCount = 1
	in.LastTermination = terminated(137, "OOMKilled")

	before := time.Now()
	got := Classify(in)
	if len(got) == 0 {
		t.Fatal("expected a diagnosis")
	}
	if got[0].Timestamp.Before(before) {
		t.Errorf("timestamp %v should fall back to wall-clock time (>= %v)", got[0].Timestamp, before)
	}
}

func TestClassifyPromotesTheFirstHighConfidenceMatch(t *testing.T) {
	// A CreateContainerConfigError (high, rule 9) alongside a startup-probe
	// signal (medium, rule 5): the high-confidence diagnosis must be primary,
	// but both must be reported for --verbose.
	in := baseInputs()
	in.RestartCount = 2
	in.Waiting = WaitingState{Present: true, Reason: "CreateContainerConfigError", Message: `secret "db-credentials" not found`}
	in.Startup = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 5}
	in.Events = []Event{warning("Unhealthy", "Startup probe failed: dial tcp 127.0.0.1:8080: connect: connection refused", 3)}

	got := Classify(in)
	if len(got) < 2 {
		t.Fatalf("expected both diagnoses, got %v", causes(got))
	}
	if got[0].Cause != CauseConfigMissingRef {
		t.Errorf("primary diagnosis is %s, want the high-confidence %s (all: %v)",
			got[0].Cause, CauseConfigMissingRef, causes(got))
	}
	if !hasCause(got, CauseProbeStartup) {
		t.Errorf("the medium-confidence match must still be reported, got %v", causes(got))
	}
}

func TestClassifyKeepsPriorityOrderWhenNothingIsHighConfidence(t *testing.T) {
	ds := []Diagnosis{
		{Cause: CauseSigkillUnattributed, Confidence: ConfidenceMedium},
		{Cause: CauseAppExitNonzero, Confidence: ConfidenceLow},
	}
	got := promotePrimary(ds)
	if got[0].Cause != CauseSigkillUnattributed || got[1].Cause != CauseAppExitNonzero {
		t.Errorf("order changed without a high-confidence match: %v", causes(got))
	}
}

func TestHealthyContainerProducesNoDiagnosis(t *testing.T) {
	cases := []struct {
		name string
		in   func() Inputs
	}{
		{
			name: "running app container",
			in: func() Inputs {
				in := baseInputs()
				in.Running = RunningState{Present: true, StartedAt: fixedNow.Add(-6 * time.Hour)}
				in.RunningDuration = 6 * time.Hour
				return in
			},
		},
		{
			name: "container still being created",
			in: func() Inputs {
				in := baseInputs()
				in.PodPhase = "Pending"
				in.Waiting = WaitingState{Present: true, Reason: "ContainerCreating"}
				return in
			},
		},
		{
			name: "app container waiting on init containers",
			in: func() Inputs {
				in := baseInputs()
				in.PodPhase = "Pending"
				in.Waiting = WaitingState{Present: true, Reason: "PodInitializing"}
				return in
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.in()); len(got) != 0 {
				t.Fatalf("healthy input must produce no diagnosis, got %v", causes(got))
			}
		})
	}
}

func TestUnknownFiresOnlyWhenSomethingIsWrongAndNothingMatched(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 7
	in.Waiting = WaitingState{Present: true, Reason: "CrashLoopBackOff", Message: "back-off 5m0s restarting failed container"}

	d := diagnosisFor(t, Classify(in), CauseUnknown)
	if d.Confidence != ConfidenceLow {
		t.Errorf("confidence is %s, want low", d.Confidence)
	}
	if !evidenceContains(d, "CrashLoopBackOff") {
		t.Errorf("unknown must report the evidence it collected, got %v", d.Evidence)
	}
	if !stepsContain(d, "--ai") {
		t.Errorf("unknown must suggest the AI layer, got %v", d.NextSteps)
	}
}

func TestUnknownNeverAccompaniesAnotherDiagnosis(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 2
	in.LastTermination = terminated(1, "Error")
	in.LogTail = []string{"panic: boom"}

	got := Classify(in)
	if hasCause(got, CauseUnknown) {
		t.Fatalf("unknown must not fire alongside %v", causes(got))
	}
}

// ---------------------------------------------------------------------------
// Suggested-command hygiene
// ---------------------------------------------------------------------------

// TestNextStepCommandsAreCopyPasteable guards against flag-concatenation bugs
// in the suggested commands. A real one shipped: the liveness rule stripped
// the leading space from containerFlag() and welded the container flag onto
// the namespace, emitting `kubectl exec pod -n prod-c api -- ...`, which fails
// the moment a user pastes it. NextSteps are the actionable half of a
// diagnosis, so a malformed command is a real defect, not cosmetics.
func TestNextStepCommandsAreCopyPasteable(t *testing.T) {
	probe := baseInputs()
	probe.LastTermination = terminated(137, "Error")
	probe.RestartCount = 4
	probe.Liveness = ProbeSpec{Defined: true, FailureThreshold: 2, PeriodSeconds: 5, TimeoutSeconds: 1}
	probe.Events = []Event{
		warning("Unhealthy", "Liveness probe failed: connection refused", 12),
		warning("Killing", "Container api failed liveness probe, will be restarted", 3),
	}

	oom := baseInputs()
	oom.LastTermination = terminated(137, "OOMKilled")
	oom.RestartCount = 3

	appCrash := baseInputs()
	appCrash.LastTermination = terminated(1, "Error")
	appCrash.RestartCount = 2
	appCrash.LogTail = []string{"panic: runtime error: index out of range"}

	initStuck := baseInputs()
	initStuck.Kind = KindInit
	initStuck.Container = "wait-for-db"
	initStuck.PodPhase = "Pending"
	initStuck.Running = RunningState{Present: true, StartedAt: fixedNow.Add(-30 * time.Minute)}
	initStuck.RunningDuration = 30 * time.Minute

	for name, in := range map[string]Inputs{
		"probe":      probe,
		"oom":        oom,
		"app_crash":  appCrash,
		"init_stuck": initStuck,
	} {
		t.Run(name, func(t *testing.T) {
			got := Classify(in)
			if len(got) == 0 {
				t.Fatalf("fixture produced no diagnosis")
			}
			for _, d := range got {
				for _, step := range d.NextSteps {
					// The namespace must never be glued to whatever follows
					// it: "-n production-c api" instead of "-n production -c api".
					if strings.Contains(step, in.Namespace+"-") {
						t.Errorf("cause %s: namespace is concatenated with the next flag:\n  %s", d.Cause, step)
					}
					// Likewise the pod name, which is always followed by a
					// space or ends the command.
					if strings.Contains(step, in.Pod+"-n ") {
						t.Errorf("cause %s: pod name is concatenated with the next flag:\n  %s", d.Cause, step)
					}
					if strings.Contains(step, "  ") && !strings.Contains(step, "    #") {
						t.Errorf("cause %s: double space outside the trailing comment:\n  %s", d.Cause, step)
					}
				}
			}
		})
	}
}

// TestLivenessExecStepIsWellFormed pins the exact command the bug corrupted.
func TestLivenessExecStepIsWellFormed(t *testing.T) {
	in := baseInputs()
	in.LastTermination = terminated(137, "Error")
	in.RestartCount = 4
	in.Liveness = ProbeSpec{Defined: true, FailureThreshold: 2, PeriodSeconds: 5, TimeoutSeconds: 1}
	in.Events = []Event{
		warning("Unhealthy", "Liveness probe failed: connection refused", 12),
		warning("Killing", "Container api failed liveness probe, will be restarted", 3),
	}

	got := Classify(in)
	if len(got) == 0 || got[0].Cause != CauseProbeLiveness {
		t.Fatalf("Classify() = %v, want probe_liveness_failure first", causes(got))
	}

	var exec string
	for _, s := range got[0].NextSteps {
		if strings.Contains(s, "kubectl exec") {
			exec = s
			break
		}
	}
	if exec == "" {
		t.Fatal("probe_liveness_failure should suggest exec'ing the liveness endpoint")
	}
	if want := "kubectl exec api-7d9f8c6b4-abcde -n production -c api -- "; !strings.Contains(exec, want) {
		t.Errorf("exec step is malformed:\n got: %s\nwant it to contain: %s", exec, want)
	}
}
