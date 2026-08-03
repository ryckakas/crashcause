package engine

import (
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Local helpers (rt-prefixed to avoid collisions with other _test.go files in
// this package)
// ---------------------------------------------------------------------------

// rtAssertOnlyImagePull checks that exactly the wanted image-pull cause fired
// and the other two image-pull causes did not.
func rtAssertOnlyImagePull(t *testing.T, ds []Diagnosis, want CauseCode) {
	t.Helper()
	all := []CauseCode{CauseImagePullAuth, CauseImagePullNotFound, CauseImagePullOther}
	for _, c := range all {
		got := hasCause(ds, c)
		if c == want && !got {
			t.Errorf("expected %s to fire, got causes %v", c, causes(ds))
		}
		if c != want && got {
			t.Errorf("expected %s NOT to fire alongside %s, got causes %v", c, want, causes(ds))
		}
	}
}

// ---------------------------------------------------------------------------
// evicted
// ---------------------------------------------------------------------------

func TestRtEvictedResourceVariants(t *testing.T) {
	tests := []struct {
		name         string
		podMessage   string
		wantExplWord string
		wantResource string
		wantStep     string
	}{
		{
			name:         "memory pressure eviction",
			podMessage:   "The node was low on resource: memory. Threshold quantity: 100Mi, available: 50Mi.",
			wantExplWord: "ran out of memory",
			wantResource: "memory",
			wantStep:     "kubectl top nodes",
		},
		{
			name:         "ephemeral storage eviction",
			podMessage:   "The node was low on resource: ephemeral-storage. Container api was using 5Gi, request is 0.",
			wantExplWord: "ephemeral storage",
			wantResource: "ephemeral-storage",
			wantStep:     "ephemeral-storage",
		},
		{
			name:         "pids eviction",
			podMessage:   "The node was low on resource: pids.",
			wantExplWord: "process IDs",
			wantResource: "pids",
			wantStep:     "pod-max-pids",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.PodPhase = "Failed"
			in.PodReason = "Evicted"
			in.PodMessage = tc.podMessage
			in.Events = []Event{warning("Evicted", tc.podMessage, 1)}

			d := diagnosisFor(t, Classify(in), CauseEvicted)
			if d.Confidence != ConfidenceHigh {
				t.Errorf("confidence is %s, want high", d.Confidence)
			}
			if !strings.Contains(d.Explanation, tc.wantExplWord) {
				t.Errorf("explanation %q does not mention %q", d.Explanation, tc.wantExplWord)
			}
			if !evidenceContains(d, "exhausted node resource: "+tc.wantResource) {
				t.Errorf("evidence must cite the exhausted resource, got %v", d.Evidence)
			}
			if len(d.NextSteps) == 0 {
				t.Fatal("evicted must always suggest next steps")
			}
			if !stepsContain(d, tc.wantStep) {
				t.Errorf("next steps %v do not contain %q", d.NextSteps, tc.wantStep)
			}
		})
	}
}

func TestRtEvictedEventFiresWithoutPodReason(t *testing.T) {
	in := baseInputs()
	in.Events = []Event{
		warning("Evicted", "The node was low on resource: memory. Threshold quantity: 100Mi, available: 50Mi.", 1),
	}
	// PodReason deliberately left unset.

	d := diagnosisFor(t, Classify(in), CauseEvicted)
	if d.Confidence != ConfidenceHigh {
		t.Errorf("confidence is %s, want high", d.Confidence)
	}
	if !evidenceContains(d, "exhausted node resource: memory") {
		t.Errorf("evidence must cite the exhausted resource, got %v", d.Evidence)
	}
	if !evidenceContains(d, "event: Evicted") {
		t.Errorf("evidence must cite the Evicted event, got %v", d.Evidence)
	}
}

func TestRtNoEvictionSignalDoesNotFire(t *testing.T) {
	in := baseInputs()
	if d := matchRule(CauseEvicted, in); d != nil {
		t.Fatalf("evicted must not fire without an eviction signal, got %+v", d)
	}
}

// ---------------------------------------------------------------------------
// probe_liveness_failure
// ---------------------------------------------------------------------------

func TestRtProbeLivenessHighWhenKilled(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 4
	in.Liveness = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 10}
	in.Events = []Event{
		warning("Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500", 12),
		warning("Killing", "Container api failed liveness probe, will be restarted", 1),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeLiveness)
	if d.Confidence != ConfidenceHigh {
		t.Errorf("confidence is %s, want high", d.Confidence)
	}
	for _, want := range []string{"failureThreshold=3", "periodSeconds=10", "30s"} {
		if !evidenceContains(d, want) {
			t.Errorf("evidence %v does not contain %q", d.Evidence, want)
		}
	}
	if !evidenceContains(d, "observed liveness probe failures: 12") {
		t.Errorf("evidence must report the observed failure count, got %v", d.Evidence)
	}
	if !evidenceContains(d, "liveness failures") {
		t.Errorf("evidence must report the event timing window, got %v", d.Evidence)
	}
}

func TestRtProbeLivenessMediumWithoutKilling(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 2
	in.Liveness = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 10}
	in.Events = []Event{
		warning("Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500", 5),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeLiveness)
	if d.Confidence != ConfidenceMedium {
		t.Errorf("confidence is %s, want medium", d.Confidence)
	}
}

func TestRtProbeLivenessNoMatchWithoutKillOrRestart(t *testing.T) {
	in := baseInputs()
	in.Liveness = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 10}
	in.Events = []Event{
		warning("Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500", 3),
	}

	if d := matchRule(CauseProbeLiveness, in); d != nil {
		t.Fatalf("liveness probe with no kill and no restart must not fire, got %+v", d)
	}
}

func TestRtProbeLivenessNoMatchForReadinessOnly(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 3
	in.Events = []Event{
		warning("Unhealthy", "Readiness probe failed: HTTP probe failed with statuscode: 503", 3),
		warning("Killing", "Container api failed readiness probe, will be restarted", 1),
	}

	if d := matchRule(CauseProbeLiveness, in); d != nil {
		t.Fatalf("readiness-only events must not fire probe_liveness_failure, got %+v", d)
	}
}

func TestRtProbeLivenessBudgetExcludesInitialDelay(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 4
	in.Liveness = ProbeSpec{Defined: true, FailureThreshold: 7, PeriodSeconds: 10, InitialDelaySeconds: 15}
	in.Events = []Event{
		warning("Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 500", 14),
		warning("Killing", "Container api failed liveness probe, will be restarted", 2),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeLiveness)
	if !strings.Contains(d.Explanation, "killed after roughly 1m10s of failing probes") {
		t.Errorf("explanation must quote failureThreshold x periodSeconds = 1m10s, got %q", d.Explanation)
	}
	if strings.Contains(d.Explanation, "1m25s of failing probes") {
		t.Errorf("explanation counts initialDelaySeconds as failing-probe time, got %q", d.Explanation)
	}
	if !strings.Contains(d.Explanation, "about 1m25s after it starts") {
		t.Errorf("explanation must state the delay-inclusive total separately, got %q", d.Explanation)
	}
	if !evidenceContains(d, "failureThreshold=7 x periodSeconds=10 = 1m10s before the kubelet acts (plus initialDelaySeconds=15)") {
		t.Errorf("evidence must carry the probing budget with the delay called out, got %v", d.Evidence)
	}
}

func TestRtProbeEventsFromAnotherContainerDoNotContaminate(t *testing.T) {
	// Container "api" exited 1 on its own; the sidecar's liveness failures
	// must not reattribute that crash to a probe kill.
	unhealthy := warning("Unhealthy", "Liveness probe failed: HTTP probe failed with statuscode: 503", 6)
	unhealthy.Container = "sidecar"
	killing := warning("Killing", "Container sidecar failed liveness probe, will be restarted", 2)
	killing.Container = "sidecar"

	in := baseInputs()
	in.RestartCount = 2
	in.LastTermination = terminated(1, "Error")
	in.Liveness = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 10}
	in.Events = []Event{unhealthy, killing}

	got := Classify(in)
	if hasCause(got, CauseProbeLiveness) {
		t.Fatalf("another container's probe events must not fire probe_liveness_failure, got %v", causes(got))
	}
	diagnosisFor(t, got, CauseAppExitNonzero)

	// Control: the same events attributed to the diagnosed container (or not
	// attributed at all) keep the probe diagnosis and suppress app_exit_nonzero.
	for name, container := range map[string]string{"same container": "api", "unattributed": ""} {
		t.Run(name, func(t *testing.T) {
			ctrl := in
			u, k := unhealthy, killing
			u.Container = container
			k.Container = container
			ctrl.Events = []Event{u, k}
			ctrlGot := Classify(ctrl)
			if !hasCause(ctrlGot, CauseProbeLiveness) {
				t.Fatalf("control case failed: probe_liveness_failure did not fire, got %v", causes(ctrlGot))
			}
			if hasCause(ctrlGot, CauseAppExitNonzero) {
				t.Fatalf("app_exit_nonzero must stand down for a probe kill, got %v", causes(ctrlGot))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// probe_startup_failure
// ---------------------------------------------------------------------------

func TestRtProbeStartupHighWhenKilled(t *testing.T) {
	in := baseInputs()
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 6),
		warning("Killing", "Container api failed startup probe, will be restarted", 1),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeStartup)
	if d.Confidence != ConfidenceHigh {
		t.Errorf("confidence is %s, want high", d.Confidence)
	}
}

func TestRtProbeStartupMediumWithRestartsOnly(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 3
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 6),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeStartup)
	if d.Confidence != ConfidenceMedium {
		t.Errorf("confidence is %s, want medium", d.Confidence)
	}
}

func TestRtProbeStartupNoMatch(t *testing.T) {
	in := baseInputs()
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 6),
	}

	if d := matchRule(CauseProbeStartup, in); d != nil {
		t.Fatalf("startup probe with no kill and no restart must not fire, got %+v", d)
	}
}

func TestRtProbeStartupBudgetTooTight(t *testing.T) {
	in := baseInputs()
	in.Startup = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 5} // budget 15s
	in.LastTermination = TerminationState{
		Present:    true,
		ExitCode:   1,
		Reason:     "Error",
		StartedAt:  fixedNow.Add(-15 * time.Second),
		FinishedAt: fixedNow.Add(-1 * time.Second), // 14s window
	}
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 3),
		warning("Killing", "Container api failed startup probe, will be restarted", 1),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeStartup)
	if !strings.Contains(d.Explanation, "too tight") {
		t.Errorf("explanation must say the budget is too tight, got %q", d.Explanation)
	}
	if !evidenceContains(d, "failureThreshold=3 x periodSeconds=5 = 15s") {
		t.Errorf("evidence must cite the startup budget, got %v", d.Evidence)
	}
}

func TestRtProbeStartupRanLongerThanBudget(t *testing.T) {
	in := baseInputs()
	in.Startup = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 5} // budget 15s
	in.LastTermination = TerminationState{
		Present:    true,
		ExitCode:   1,
		Reason:     "Error",
		StartedAt:  fixedNow.Add(-5 * time.Minute),
		FinishedAt: fixedNow.Add(-1 * time.Minute), // 4m window, well past budget
	}
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 3),
		warning("Killing", "Container api failed startup probe, will be restarted", 1),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeStartup)
	if !strings.Contains(d.Explanation, "well past the budget") {
		t.Errorf("explanation must say the container ran well past the budget, got %q", d.Explanation)
	}
}

func TestRtProbeStartupWindowFromRunningDuration(t *testing.T) {
	in := baseInputs()
	in.RestartCount = 2
	in.Startup = ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 5} // budget 15s
	in.Running = RunningState{Present: true, StartedAt: fixedNow.Add(-10 * time.Second)}
	in.RunningDuration = 10 * time.Second
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 3),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeStartup)
	if !evidenceContains(d, "observed startup window: container ran 10s before termination") {
		t.Errorf("evidence must report the running-duration derived window, got %v", d.Evidence)
	}
	if !strings.Contains(d.Explanation, "too tight") {
		t.Errorf("explanation must say the budget is too tight, got %q", d.Explanation)
	}
}

func TestRtProbeStartupBudgetExcludesInitialDelay(t *testing.T) {
	in := baseInputs()
	// Probing budget 10s, delay-inclusive total 20s.
	in.Startup = ProbeSpec{Defined: true, FailureThreshold: 2, PeriodSeconds: 5, InitialDelaySeconds: 10}
	in.LastTermination = TerminationState{
		Present:    true,
		ExitCode:   1,
		Reason:     "Error",
		StartedAt:  fixedNow.Add(-60 * time.Second),
		FinishedAt: fixedNow.Add(-42 * time.Second), // 18s window: within the total, past the probing budget
	}
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 2),
		warning("Killing", "Container api failed startup probe, will be restarted", 1),
	}

	d := diagnosisFor(t, Classify(in), CauseProbeStartup)
	if !strings.Contains(d.Explanation, "failureThreshold=2 x periodSeconds=5 = 10s") {
		t.Errorf("explanation must quote the pure probing product (10s), got %q", d.Explanation)
	}
	if strings.Contains(d.Explanation, "= 20s") {
		t.Errorf("explanation folds initialDelaySeconds into the quoted product, got %q", d.Explanation)
	}
	// The observed 18s window is measured from container start, so the
	// too-tight comparison must use the delay-inclusive total (20s).
	if !strings.Contains(d.Explanation, "too tight") {
		t.Errorf("explanation must say the budget is too tight, got %q", d.Explanation)
	}
	if !evidenceContains(d, "failureThreshold=2 x periodSeconds=5 = 10s before the kubelet acts (plus initialDelaySeconds=10)") {
		t.Errorf("evidence must cite the probing budget with the delay called out, got %v", d.Evidence)
	}
}

func TestRtProbeStartupNoProbeConfigCollected(t *testing.T) {
	in := baseInputs()
	in.Events = []Event{
		warning("Unhealthy", "Startup probe failed: connection refused", 3),
		warning("Killing", "Container api failed startup probe, will be restarted", 1),
	}
	// in.Startup left zero-valued: Defined == false.

	d := diagnosisFor(t, Classify(in), CauseProbeStartup)
	if !evidenceContains(d, "startup probe configuration was not collected") {
		t.Errorf("evidence must note the missing probe config, got %v", d.Evidence)
	}
}

// ---------------------------------------------------------------------------
// image_pull_auth / image_pull_not_found / image_pull_other
// ---------------------------------------------------------------------------

func TestRtImagePullClassification(t *testing.T) {
	tests := []struct {
		name     string
		image    string
		message  string
		want     CauseCode
		wantConf Confidence
		wantStep string
	}{
		{
			name:  "auth: 401 unauthorized",
			image: "registry.example.com/team/api:1.4.2",
			message: `Failed to pull image "registry.example.com/team/api:1.4.2": rpc error: code = Unknown desc = ` +
				`failed to resolve reference: pulling from host registry.example.com failed with status code ` +
				`[manifests 1.4.2]: 401 Unauthorized`,
			want:     CauseImagePullAuth,
			wantConf: ConfidenceHigh,
			wantStep: "imagePullSecrets",
		},
		{
			name:  "auth: docker hub pull access denied",
			image: "private/app:latest",
			message: `Failed to pull image "private/app:latest": rpc error: code = Unknown desc = failed to resolve ` +
				`reference: pull access denied for private/app, repository does not exist or may require 'docker login'`,
			want:     CauseImagePullAuth,
			wantConf: ConfidenceHigh,
			wantStep: "imagePullSecrets",
		},
		{
			name:  "not found: bad tag",
			image: "nginx:1.2.3-nope",
			message: `Failed to pull image "nginx:1.2.3-nope": rpc error: code = NotFound desc = failed to pull and ` +
				`unpack image "docker.io/library/nginx:1.2.3-nope": failed to resolve reference ` +
				`"docker.io/library/nginx:1.2.3-nope": docker.io/library/nginx:1.2.3-nope: not found`,
			want:     CauseImagePullNotFound,
			wantConf: ConfidenceHigh,
			wantStep: "Verify the tag exists",
		},
		{
			name:  "not found: manifest unknown",
			image: "app:missing-tag",
			message: `Failed to pull image "app:missing-tag": rpc error: code = NotFound desc = failed to resolve ` +
				`reference: unexpected status code 404: manifest unknown: manifest unknown`,
			want:     CauseImagePullNotFound,
			wantConf: ConfidenceHigh,
			wantStep: "Verify the tag exists",
		},
		{
			name:  "other: network timeout",
			image: "app:1.0.0",
			message: `Failed to pull image "app:1.0.0": rpc error: code = DeadlineExceeded desc = failed to pull ` +
				`and unpack image: dial tcp 10.0.0.1:443: i/o timeout`,
			want:     CauseImagePullOther,
			wantConf: ConfidenceMedium,
			wantStep: "connectivity",
		},
		{
			name:  "other: TLS trust failure",
			image: "app:1.0.0",
			message: `Failed to pull image "app:1.0.0": rpc error: code = Unknown desc = failed to resolve reference: ` +
				`pulling from host registry.example.com failed: x509: certificate signed by unknown authority`,
			want:     CauseImagePullOther,
			wantConf: ConfidenceMedium,
			wantStep: "TLS",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.PodPhase = "Pending"
			in.Image = tc.image
			in.Waiting = WaitingState{Present: true, Reason: "ImagePullBackOff"}
			in.Events = []Event{warning("Failed", tc.message, 1)}

			got := Classify(in)
			rtAssertOnlyImagePull(t, got, tc.want)

			d := diagnosisFor(t, got, tc.want)
			if d.Confidence != tc.wantConf {
				t.Errorf("confidence is %s, want %s", d.Confidence, tc.wantConf)
			}
			if !evidenceContains(d, "image: "+tc.image) {
				t.Errorf("evidence must cite the image, got %v", d.Evidence)
			}
			if !stepsContain(d, tc.wantStep) {
				t.Errorf("next steps %v do not contain %q", d.NextSteps, tc.wantStep)
			}
		})
	}
}

func TestRtImagePullFromWaitingMessageOnly(t *testing.T) {
	in := baseInputs()
	in.PodPhase = "Pending"
	in.Image = "registry.example.com/team/api:1.4.2"
	in.Waiting = WaitingState{
		Present: true,
		Reason:  "ImagePullBackOff",
		Message: `Failed to pull image "registry.example.com/team/api:1.4.2": rpc error: code = Unknown desc = ` +
			`failed to resolve reference: 401 Unauthorized`,
	}
	// No events at all: the waiting message is the only carrier of the error.

	got := Classify(in)
	rtAssertOnlyImagePull(t, got, CauseImagePullAuth)
	d := diagnosisFor(t, got, CauseImagePullAuth)
	if d.Confidence != ConfidenceHigh {
		t.Errorf("confidence is %s, want high", d.Confidence)
	}
}

// ---------------------------------------------------------------------------
// volume_mount_failure
// ---------------------------------------------------------------------------

func TestRtVolumeMountFailureVariants(t *testing.T) {
	tests := []struct {
		name     string
		reason   string
		message  string
		wantName string
	}{
		{
			name:   "unmounted volumes timed out",
			reason: "FailedMount",
			message: `Unable to attach or mount volumes: unmounted volumes=[data], unattached volumes=[data ` +
				`kube-api-access-x9k2p]: timed out waiting for the condition`,
			wantName: "data",
		},
		{
			name:     "configmap-backed volume missing",
			reason:   "FailedMount",
			message:  `MountVolume.SetUp failed for volume "config" : configmap "app-config" not found`,
			wantName: "config",
		},
		{
			name:     "attach failed",
			reason:   "FailedAttachVolume",
			message:  `AttachVolume.Attach failed for volume "pvc-8a1f" : timed out waiting for external-attacher`,
			wantName: "pvc-8a1f",
		},
		{
			name:   "pvc not found",
			reason: "FailedMount",
			message: `Unable to attach or mount volumes: unmounted volumes=[app-data], unattached volumes=[app-data]: ` +
				`persistentvolumeclaims "my-pvc" not found`,
			wantName: "my-pvc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.PodPhase = "Pending"
			in.Events = []Event{warning(tc.reason, tc.message, 1)}

			d := diagnosisFor(t, Classify(in), CauseVolumeMountFailure)
			if d.Confidence != ConfidenceHigh {
				t.Errorf("confidence is %s, want high", d.Confidence)
			}
			if !strings.Contains(d.Explanation, tc.wantName) {
				t.Errorf("explanation must name %q, got %q", tc.wantName, d.Explanation)
			}
			if !evidenceContains(d, tc.wantName) {
				t.Errorf("evidence must name %q, got %v", tc.wantName, d.Evidence)
			}
			if !stepsContain(d, "pvc") {
				t.Errorf("next steps must mention pvc, got %v", d.NextSteps)
			}
		})
	}
}

func TestRtVolumeMountFailureNoMountEventsDoesNotFire(t *testing.T) {
	in := baseInputs()
	if d := matchRule(CauseVolumeMountFailure, in); d != nil {
		t.Fatalf("volume_mount_failure must not fire without mount events, got %+v", d)
	}
}

// ---------------------------------------------------------------------------
// init_container_failure (app containers only)
// ---------------------------------------------------------------------------

func TestRtInitContainerFailureNamesTheContainer(t *testing.T) {
	in := baseInputs()
	in.PodPhase = "Pending"
	in.Waiting = WaitingState{Present: true, Reason: "PodInitializing"}
	in.Events = []Event{
		warning("BackOff", "Back-off restarting failed container init-db in pod api-7d9f8c6b4-abcde_production(abc123)", 4),
	}

	d := diagnosisFor(t, Classify(in), CauseInitContainerFailure)
	if d.Confidence != ConfidenceHigh {
		t.Errorf("confidence is %s, want high", d.Confidence)
	}
	if !strings.Contains(d.Explanation, "init-db") {
		t.Errorf("explanation must name the failing init container, got %q", d.Explanation)
	}
	if !evidenceContains(d, "init-db") {
		t.Errorf("evidence must name the failing init container, got %v", d.Evidence)
	}
	if !stepsContain(d, "-c init-db --previous") {
		t.Errorf("next steps must suggest logs -c init-db --previous, got %v", d.NextSteps)
	}
}

func TestRtInitContainerFailureUnnamedIsMedium(t *testing.T) {
	in := baseInputs()
	in.PodPhase = "Pending"
	in.Waiting = WaitingState{Present: true, Reason: "PodInitializing"}
	in.Events = []Event{
		warning("BackOff", "Back-off restarting failed container", 4),
	}

	d := diagnosisFor(t, Classify(in), CauseInitContainerFailure)
	if d.Confidence != ConfidenceMedium {
		t.Errorf("confidence is %s, want medium", d.Confidence)
	}
}

func TestRtInitContainerFailureDoesNotFireForInitContainer(t *testing.T) {
	in := baseInputs()
	in.Kind = KindInit
	in.Container = "init-db"
	in.PodPhase = "Pending"
	in.Waiting = WaitingState{Present: true, Reason: "PodInitializing"}
	in.Events = []Event{
		warning("BackOff", "Back-off restarting failed container init-db in pod api-7d9f8c6b4-abcde_production(abc123)", 4),
	}

	if got := Classify(in); hasCause(got, CauseInitContainerFailure) {
		t.Fatalf("init_container_failure must not fire for an init container itself, got %v", causes(got))
	}
}

func TestRtPodInitializingWithNoFailureEventsProducesNoDiagnosis(t *testing.T) {
	in := baseInputs()
	in.PodPhase = "Pending"
	in.Waiting = WaitingState{Present: true, Reason: "PodInitializing"}

	if got := Classify(in); len(got) != 0 {
		t.Fatalf("PodInitializing with no failure events must produce no diagnosis, got %v", causes(got))
	}
}

// ---------------------------------------------------------------------------
// Cross-rule interaction sanity: rules that also apply to init containers
// ---------------------------------------------------------------------------

func TestRtInitContainerImagePullStillFires(t *testing.T) {
	in := baseInputs()
	in.Kind = KindInit
	in.Container = "init-db"
	in.PodPhase = "Pending"
	in.Image = "registry.example.com/team/api:1.4.2"
	in.Waiting = WaitingState{Present: true, Reason: "ImagePullBackOff"}
	in.Events = []Event{
		warning("Failed", `Failed to pull image "registry.example.com/team/api:1.4.2": rpc error: code = Unknown `+
			`desc = failed to resolve reference: 401 Unauthorized`, 1),
	}

	got := Classify(in)
	if !hasCause(got, CauseImagePullAuth) {
		t.Fatalf("image_pull_auth must still fire for an init container, got %v", causes(got))
	}
}

func TestRtInitContainerOOMKilledStillFires(t *testing.T) {
	in := baseInputs()
	in.Kind = KindInit
	in.Container = "init-db"
	in.PodPhase = "Pending"
	in.RestartCount = 1
	in.LastTermination = terminated(137, "OOMKilled")

	got := Classify(in)
	if !hasCause(got, CauseOOMKilled) {
		t.Fatalf("oom_killed must still fire for an init container, got %v", causes(got))
	}
}

func TestRtInitContainerConfigMissingRefStillFires(t *testing.T) {
	in := baseInputs()
	in.Kind = KindInit
	in.Container = "init-db"
	in.PodPhase = "Pending"
	in.Waiting = WaitingState{Present: true, Reason: "CreateContainerConfigError", Message: `secret "db-credentials" not found`}
	in.Events = []Event{warning("Failed", `Error: secret "db-credentials" not found`, 1)}

	got := Classify(in)
	if !hasCause(got, CauseConfigMissingRef) {
		t.Fatalf("config_missing_reference must still fire for an init container, got %v", causes(got))
	}
}
