package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Input normalisation
// ---------------------------------------------------------------------------

func TestHTEffectiveKind(t *testing.T) {
	tests := []struct {
		name string
		kind ContainerKind
		want ContainerKind
	}{
		{"unset defaults to app", "", KindApp},
		{"app kept", KindApp, KindApp},
		{"init kept", KindInit, KindInit},
		{"ephemeral kept", KindEphemeral, KindEphemeral},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.Kind = tc.kind
			if got := effectiveKind(in); got != tc.want {
				t.Errorf("effectiveKind() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestHTEffectiveTermination(t *testing.T) {
	last := terminated(137, "OOMKilled")
	current := terminated(1, "Error")

	t.Run("last wins over current", func(t *testing.T) {
		in := baseInputs()
		in.LastTermination = last
		in.CurrentTermination = current
		got := effectiveTermination(in)
		if got.ExitCode != 137 || got.Reason != "OOMKilled" {
			t.Errorf("effectiveTermination() = %+v, want last termination", got)
		}
	})

	t.Run("falls back to current when last absent", func(t *testing.T) {
		in := baseInputs()
		in.CurrentTermination = current
		got := effectiveTermination(in)
		if got.ExitCode != 1 || got.Reason != "Error" {
			t.Errorf("effectiveTermination() = %+v, want current termination", got)
		}
	})

	t.Run("both absent yields not-present zero value", func(t *testing.T) {
		in := baseInputs()
		got := effectiveTermination(in)
		if got.Present {
			t.Errorf("effectiveTermination() = %+v, want Present=false", got)
		}
	})
}

func TestHTInitStuckThreshold(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"zero falls back to default", 0, defaultInitStuckThreshold},
		{"negative falls back to default", -5 * time.Minute, defaultInitStuckThreshold},
		{"explicit value is kept", 20 * time.Minute, 20 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.InitStuckThreshold = tc.in
			if got := initStuckThreshold(in); got != tc.want {
				t.Errorf("initStuckThreshold() = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Event helpers
// ---------------------------------------------------------------------------

func TestHTEventsByReasonAndFirstEventByReason(t *testing.T) {
	in := baseInputs()
	in.Events = []Event{
		warning("Unhealthy", "Liveness probe failed", 2),
		warning("BackOff", "Back-off restarting failed container", 4),
		warning("unhealthy", "case-insensitive match", 1), // lowercase reason
	}

	got := eventsByReason(in, "Unhealthy")
	if len(got) != 2 {
		t.Fatalf("eventsByReason(Unhealthy) = %d events, want 2 (case-insensitive match), got %+v", len(got), got)
	}

	gotMulti := eventsByReason(in, "Unhealthy", "BackOff")
	if len(gotMulti) != 3 {
		t.Fatalf("eventsByReason(Unhealthy, BackOff) = %d events, want 3", len(gotMulti))
	}

	none := eventsByReason(in, "Nonexistent")
	if len(none) != 0 {
		t.Fatalf("eventsByReason(Nonexistent) = %d events, want 0", len(none))
	}

	first, ok := firstEventByReason(in, "BackOff")
	if !ok || first.Reason != "BackOff" {
		t.Fatalf("firstEventByReason(BackOff) = %+v, %v", first, ok)
	}

	_, ok = firstEventByReason(in, "Nonexistent")
	if ok {
		t.Fatalf("firstEventByReason(Nonexistent) ok = true, want false")
	}
}

func TestHTEventsMatching(t *testing.T) {
	in := baseInputs()
	in.Events = []Event{
		warning("Unhealthy", "Liveness probe failed: HTTP 500", 1),
		warning("Unhealthy", "Startup probe failed: connection refused", 1),
	}

	got := eventsMatching(in, "liveness")
	if len(got) != 1 || got[0].Message != "Liveness probe failed: HTTP 500" {
		t.Fatalf("eventsMatching(Unhealthy, liveness) = %+v, want the liveness event only", got)
	}

	none := eventsMatching(in, "nonexistent-substring")
	if len(none) != 0 {
		t.Fatalf("eventsMatching with no match = %+v, want empty", none)
	}
}

func TestHTContainsFold(t *testing.T) {
	if !containsFold("Hello World", "world") {
		t.Error("containsFold should be case-insensitive")
	}
	if containsFold("Hello World", "xyz") {
		t.Error("containsFold should not match absent substring")
	}
}

func TestHTEventCountAndTotalEventCount(t *testing.T) {
	tests := []struct {
		count int32
		want  int32
	}{
		{0, 1},
		{-3, 1},
		{5, 5},
	}
	for _, tc := range tests {
		e := Event{Count: tc.count}
		if got := eventCount(e); got != tc.want {
			t.Errorf("eventCount(%d) = %d, want %d", tc.count, got, tc.want)
		}
	}

	evs := []Event{{Count: 0}, {Count: 3}, {Count: 2}}
	if got := totalEventCount(evs); got != 6 {
		t.Errorf("totalEventCount() = %d, want 6 (1+3+2)", got)
	}
	if got := totalEventCount(nil); got != 0 {
		t.Errorf("totalEventCount(nil) = %d, want 0", got)
	}
}

func TestHTDescribeEvent(t *testing.T) {
	e := Event{Reason: "BackOff", Message: "Back-off restarting failed container", Count: 3}
	want := `event: BackOff (x3): Back-off restarting failed container`
	if got := describeEvent(e); got != want {
		t.Errorf("describeEvent() = %q, want %q", got, want)
	}
}

func TestHTDescribeEventTiming(t *testing.T) {
	t1 := fixedNow.Add(-10 * time.Minute)
	t2 := fixedNow.Add(-1 * time.Minute)

	tests := []struct {
		name string
		evs  []Event
		want string
	}{
		{
			name: "no timestamps at all",
			evs:  []Event{{Reason: "X"}},
			want: "",
		},
		{
			name: "only last seen present",
			evs:  []Event{{LastSeen: t2}},
			want: "last observed at " + t2.UTC().Format(time.RFC3339),
		},
		{
			name: "only first seen present",
			evs:  []Event{{FirstSeen: t1}},
			want: "first observed at " + t1.UTC().Format(time.RFC3339),
		},
		{
			name: "first equals last",
			evs:  []Event{{FirstSeen: t1, LastSeen: t1}},
			want: "first observed at " + t1.UTC().Format(time.RFC3339),
		},
		{
			name: "full window",
			evs:  []Event{{FirstSeen: t1, LastSeen: t2}},
			want: fmt.Sprintf("observed from %s to %s (%s window)",
				t1.UTC().Format(time.RFC3339), t2.UTC().Format(time.RFC3339), formatDuration(t2.Sub(t1))),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeEventTiming(tc.evs); got != tc.want {
				t.Errorf("describeEventTiming() = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

func TestHTTruncateMessage(t *testing.T) {
	t.Run("short message unchanged", func(t *testing.T) {
		if got := truncateMessage("short message"); got != "short message" {
			t.Errorf("truncateMessage() = %q, want unchanged", got)
		}
	})
	t.Run("newlines flattened", func(t *testing.T) {
		got := truncateMessage("line one\nline two\nline three")
		want := "line one line two line three"
		if got != want {
			t.Errorf("truncateMessage() = %q, want %q", got, want)
		}
	})
	t.Run("over 300 chars is truncated with ellipsis", func(t *testing.T) {
		long := ""
		for i := 0; i < 320; i++ {
			long += "a"
		}
		got := truncateMessage(long)
		if len(got) != maxMessageLen+3 {
			t.Fatalf("truncateMessage() length = %d, want %d", len(got), maxMessageLen+3)
		}
		if got[len(got)-3:] != "..." {
			t.Errorf("truncateMessage() does not end with ellipsis: %q", got)
		}
		if got[:maxMessageLen] != long[:maxMessageLen] {
			t.Errorf("truncateMessage() did not preserve the first %d chars", maxMessageLen)
		}
	})
}

func TestHTFormatDuration(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"negative", -5 * time.Second, "0s"},
		{"sub-second", 250 * time.Millisecond, "250ms"},
		{"rounded seconds", 45 * time.Second, "45s"},
		{"rounded minutes", 90 * time.Second, "1m30s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatDuration(tc.d); got != tc.want {
				t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
			}
		})
	}
}

func TestHTContainerNounAndRef(t *testing.T) {
	tests := []struct {
		name      string
		kind      ContainerKind
		container string
		wantNoun  string
		wantRef   string
	}{
		{"app container", KindApp, "api", "container", `container "api"`},
		{"init container", KindInit, "init-db", "init container", `init container "init-db"`},
		{"ephemeral container", KindEphemeral, "debug", "ephemeral container", `ephemeral container "debug"`},
		{"empty container name falls back to noun", KindApp, "", "container", "container"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			in.Kind = tc.kind
			in.Container = tc.container
			if got := containerNoun(in); got != tc.wantNoun {
				t.Errorf("containerNoun() = %q, want %q", got, tc.wantNoun)
			}
			if got := containerRef(in); got != tc.wantRef {
				t.Errorf("containerRef() = %q, want %q", got, tc.wantRef)
			}
		})
	}
}

func TestHTPodRefNsFlagContainerFlag(t *testing.T) {
	t.Run("populated fields", func(t *testing.T) {
		in := baseInputs()
		if got := podRef(in); got != in.Pod {
			t.Errorf("podRef() = %q, want %q", got, in.Pod)
		}
		if got := nsFlag(in); got != "-n "+in.Namespace {
			t.Errorf("nsFlag() = %q, want -n %s", got, in.Namespace)
		}
		if got := containerFlag(in); got != " -c "+in.Container {
			t.Errorf("containerFlag() = %q, want  -c %s", got, in.Container)
		}
	})
	t.Run("empty fields fall back to placeholders", func(t *testing.T) {
		in := baseInputs()
		in.Pod = ""
		in.Namespace = ""
		in.Container = ""
		if got := podRef(in); got != "<pod>" {
			t.Errorf("podRef() = %q, want <pod>", got)
		}
		if got := nsFlag(in); got != "-n <namespace>" {
			t.Errorf("nsFlag() = %q, want -n <namespace>", got)
		}
		if got := containerFlag(in); got != "" {
			t.Errorf("containerFlag() = %q, want empty string", got)
		}
	})
}

func TestHTLogsCommandAndDescribeCommand(t *testing.T) {
	in := baseInputs()
	want := fmt.Sprintf("kubectl logs %s -n %s -c %s", in.Pod, in.Namespace, in.Container)
	if got := logsCommand(in, false); got != want {
		t.Errorf("logsCommand(false) = %q, want %q", got, want)
	}
	if got := logsCommand(in, true); got != want+" --previous" {
		t.Errorf("logsCommand(true) = %q, want %q", got, want+" --previous")
	}
	wantDescribe := fmt.Sprintf("kubectl describe pod %s -n %s", in.Pod, in.Namespace)
	if got := describeCommand(in); got != wantDescribe {
		t.Errorf("describeCommand() = %q, want %q", got, wantDescribe)
	}
}

// ---------------------------------------------------------------------------
// Evidence builders
// ---------------------------------------------------------------------------

func TestHTTerminationEvidence(t *testing.T) {
	t.Run("not present yields nil", func(t *testing.T) {
		if got := terminationEvidence(TerminationState{}); got != nil {
			t.Errorf("terminationEvidence(not present) = %v, want nil", got)
		}
	})

	t.Run("full termination state", func(t *testing.T) {
		started := fixedNow.Add(-5 * time.Minute)
		finished := fixedNow.Add(-1 * time.Minute)
		term := TerminationState{
			Present:    true,
			ExitCode:   137,
			Reason:     "Error",
			Signal:     9,
			Message:    "boom",
			StartedAt:  started,
			FinishedAt: finished,
		}
		ev := terminationEvidence(term)
		want := []string{
			"exit code 137 (128 + 9: killed by SIGKILL)",
			"terminated reason: Error",
			"termination signal: 9 (SIGKILL)",
			"termination message: boom",
			fmt.Sprintf("container ran for %s before terminating (%s to %s)",
				formatDuration(finished.Sub(started)), started.UTC().Format(time.RFC3339), finished.UTC().Format(time.RFC3339)),
		}
		if len(ev) != len(want) {
			t.Fatalf("terminationEvidence() = %v, want %v", ev, want)
		}
		for i := range want {
			if ev[i] != want[i] {
				t.Errorf("terminationEvidence()[%d] = %q, want %q", i, ev[i], want[i])
			}
		}
	})

	t.Run("minimal state omits optional lines", func(t *testing.T) {
		term := TerminationState{Present: true, ExitCode: 0}
		ev := terminationEvidence(term)
		if len(ev) != 1 || ev[0] != "exit code 0" {
			t.Errorf("terminationEvidence(minimal) = %v, want just the exit code line", ev)
		}
	})
}

func TestHTExitCodeGloss(t *testing.T) {
	tests := []struct {
		code int32
		want string
	}{
		{126, " (command found but not executable)"},
		{127, " (command not found)"},
		{137, " (128 + 9: killed by SIGKILL)"},
		{139, " (128 + 11: killed by SIGSEGV, segmentation fault)"},
		{143, " (128 + 15: killed by SIGTERM)"},
		{1, ""},
	}
	for _, tc := range tests {
		if got := exitCodeGloss(tc.code); got != tc.want {
			t.Errorf("exitCodeGloss(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

func TestHTSignalName(t *testing.T) {
	tests := []struct {
		sig  int32
		want string
	}{
		{9, "SIGKILL"},
		{11, "SIGSEGV"},
		{15, "SIGTERM"},
		{6, "SIGABRT"},
		{2, "SIGINT"},
		{99, "signal"},
	}
	for _, tc := range tests {
		if got := signalName(tc.sig); got != tc.want {
			t.Errorf("signalName(%d) = %q, want %q", tc.sig, got, tc.want)
		}
	}
}

func TestHTRestartEvidence(t *testing.T) {
	t.Run("zero restarts yields nil", func(t *testing.T) {
		in := baseInputs()
		in.RestartCount = 0
		if got := restartEvidence(in); got != nil {
			t.Errorf("restartEvidence(0) = %v, want nil", got)
		}
	})
	t.Run("with restart policy", func(t *testing.T) {
		in := baseInputs()
		in.RestartCount = 3
		in.RestartPolicy = "Always"
		got := restartEvidence(in)
		want := []string{"restart count: 3", "pod restartPolicy: Always"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("restartEvidence() = %v, want %v", got, want)
		}
	})
	t.Run("without restart policy", func(t *testing.T) {
		in := baseInputs()
		in.RestartCount = 2
		in.RestartPolicy = ""
		got := restartEvidence(in)
		if len(got) != 1 || got[0] != "restart count: 2" {
			t.Errorf("restartEvidence() = %v, want just the count line", got)
		}
	})
}

func TestHTMemorySpecEvidence(t *testing.T) {
	t.Run("all fields present", func(t *testing.T) {
		in := baseInputs()
		in.Limits = map[string]string{"memory": "256Mi"}
		in.Requests = map[string]string{"memory": "128Mi"}
		in.QOSClass = "Burstable"
		got := memorySpecEvidence(in)
		want := []string{
			"memory limit was 256Mi (pod spec value)",
			"memory request was 128Mi (pod spec value)",
			"QoS class: Burstable",
		}
		if len(got) != len(want) {
			t.Fatalf("memorySpecEvidence() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("memorySpecEvidence()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
	t.Run("nothing collected yields nil", func(t *testing.T) {
		in := baseInputs()
		in.Limits = nil
		in.Requests = nil
		in.QOSClass = ""
		if got := memorySpecEvidence(in); got != nil {
			t.Errorf("memorySpecEvidence() = %v, want nil", got)
		}
	})
}

func TestHTNodePressureEvidence(t *testing.T) {
	t.Run("unknown node yields nil", func(t *testing.T) {
		in := baseInputs()
		in.Node = NodeConditions{Known: false, MemoryPressure: true}
		if got := nodePressureEvidence(in); got != nil {
			t.Errorf("nodePressureEvidence(Known=false) = %v, want nil", got)
		}
	})
	t.Run("known with no pressure yields nil", func(t *testing.T) {
		in := baseInputs()
		in.Node = NodeConditions{Known: true}
		if got := nodePressureEvidence(in); got != nil {
			t.Errorf("nodePressureEvidence(no pressure) = %v, want nil", got)
		}
	})
	t.Run("each condition reported", func(t *testing.T) {
		in := baseInputs()
		in.Node = NodeConditions{Known: true, MemoryPressure: true, DiskPressure: true, PIDPressure: true}
		got := nodePressureEvidence(in)
		want := []string{
			"node condition MemoryPressure=true",
			"node condition DiskPressure=true",
			"node condition PIDPressure=true",
		}
		if len(got) != len(want) {
			t.Fatalf("nodePressureEvidence() = %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("nodePressureEvidence()[%d] = %q, want %q", i, got[i], want[i])
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Message parsing: missing config references
// ---------------------------------------------------------------------------

func TestHTParseMissingReference(t *testing.T) {
	tests := []struct {
		name     string
		msg      string
		wantKind string
		wantName string
		wantKey  string
		wantOK   bool
	}{
		{
			name:     "missing secret",
			msg:      `secret "db-credentials" not found`,
			wantKind: "Secret",
			wantName: "db-credentials",
			wantOK:   true,
		},
		{
			name:     "missing configmap",
			msg:      `configmap "app-config" not found`,
			wantKind: "ConfigMap",
			wantName: "app-config",
			wantOK:   true,
		},
		{
			name:     "missing key in a secret",
			msg:      `couldn't find key DB_PASSWORD in Secret production/db-credentials`,
			wantKind: "Secret",
			wantName: "db-credentials",
			wantKey:  "DB_PASSWORD",
			wantOK:   true,
		},
		{
			name:     "missing key in a configmap",
			msg:      `couldn't find key log.level in ConfigMap default/app-config`,
			wantKind: "ConfigMap",
			wantName: "app-config",
			wantKey:  "log.level",
			wantOK:   true,
		},
		{
			name:     "non-existent secret key reference",
			msg:      `Error: references non-existent secret key: API_KEY`,
			wantKind: "Secret",
			wantKey:  "API_KEY",
			wantOK:   true,
		},
		{
			name:   "unparseable message",
			msg:    "container failed to start for reasons unknown",
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ref, ok := parseMissingReference(tc.msg)
			if ok != tc.wantOK {
				t.Fatalf("parseMissingReference(%q) ok = %v, want %v", tc.msg, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if ref.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q", ref.Kind, tc.wantKind)
			}
			if ref.Name != tc.wantName {
				t.Errorf("Name = %q, want %q", ref.Name, tc.wantName)
			}
			if ref.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", ref.Key, tc.wantKey)
			}
		})
	}
}

func TestHTNormalizeRefKind(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Secret", "Secret"},
		{"secrets", "Secret"},
		{"ConfigMap", "ConfigMap"},
		{"configmaps", "ConfigMap"},
		{"config map", "ConfigMap"},
		{"config maps", "ConfigMap"},
	}
	for _, tc := range tests {
		if got := normalizeRefKind(tc.in); got != tc.want {
			t.Errorf("normalizeRefKind(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHTKubectlResource(t *testing.T) {
	if got := kubectlResource("ConfigMap"); got != "configmap" {
		t.Errorf("kubectlResource(ConfigMap) = %q, want configmap", got)
	}
	if got := kubectlResource("Secret"); got != "secret" {
		t.Errorf("kubectlResource(Secret) = %q, want secret", got)
	}
	if got := kubectlResource("anything else"); got != "secret" {
		t.Errorf("kubectlResource(anything else) = %q, want secret (default)", got)
	}
}

// ---------------------------------------------------------------------------
// Message parsing: volumes
// ---------------------------------------------------------------------------

func TestHTParseVolumeNames(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want []string
	}{
		{
			name: "for volume form",
			msg:  `MountVolume.SetUp failed for volume "data" : secret "creds" not found`,
			want: []string{"data"},
		},
		{
			name: "unmounted volumes list",
			msg:  `Unable to attach or mount volumes: unmounted volumes=[data config], unattached volumes=[]: timed out`,
			want: []string{"data", "config"},
		},
		{
			name: "pvc not found",
			msg:  `persistentvolumeclaim "pgdata" not found`,
			want: []string{"pgdata"},
		},
		{
			name: "dedup across several forms referencing the same volume",
			msg:  `unmounted volumes=[data], unattached volumes=[data]: timed out; for volume "data" ...; persistentvolumeclaim "data" not found`,
			want: []string{"data"},
		},
		{
			name: "empty message yields nil",
			msg:  "",
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseVolumeNames(tc.msg)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("parseVolumeNames(%q) = %v, want nil", tc.msg, got)
				}
				return
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseVolumeNames(%q) = %v, want %v", tc.msg, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("parseVolumeNames(%q)[%d] = %q, want %q", tc.msg, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Message parsing: scheduling failures
// ---------------------------------------------------------------------------

func TestHTParseSchedulingFailure(t *testing.T) {
	tests := []struct {
		name          string
		msg           string
		wantKind      string
		wantResources []string
		wantTaints    []string
	}{
		{
			name:          "insufficient cpu and memory",
			msg:           "0/5 nodes are available: 3 Insufficient cpu, 2 Insufficient memory.",
			wantKind:      "resources",
			wantResources: []string{"cpu", "memory"},
		},
		{
			name:       "untolerated taint",
			msg:        "0/3 nodes are available: 3 node(s) had untolerated taint {node-role.kubernetes.io/control-plane: }.",
			wantKind:   "taints",
			wantTaints: []string{"node-role.kubernetes.io/control-plane:"},
		},
		{
			name:     "node affinity mismatch",
			msg:      "0/4 nodes are available: 4 node(s) didn't match Pod's node affinity/selector.",
			wantKind: "affinity",
		},
		{
			name:     "volume node affinity conflict",
			msg:      "0/6 nodes are available: 6 node(s) had volume node affinity conflict.",
			wantKind: "volume",
		},
		{
			name:     "no available persistent volumes to bind",
			msg:      "0/2 nodes are available: 2 node(s) didn't find available persistent volumes to bind.",
			wantKind: "volume",
		},
		{
			name:     "too many pods",
			msg:      "0/3 nodes are available: 3 Too many pods.",
			wantKind: "pods",
		},
		{
			name:     "unrecognized message",
			msg:      "0/2 nodes are available: 2 node(s) had some unrelated problem.",
			wantKind: "other",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sc := parseSchedulingFailure(tc.msg)
			if sc.Kind != tc.wantKind {
				t.Fatalf("parseSchedulingFailure(%q).Kind = %q, want %q", tc.msg, sc.Kind, tc.wantKind)
			}
			if tc.wantResources != nil {
				if len(sc.Resources) != len(tc.wantResources) {
					t.Fatalf("Resources = %v, want %v", sc.Resources, tc.wantResources)
				}
				for i := range tc.wantResources {
					if sc.Resources[i] != tc.wantResources[i] {
						t.Errorf("Resources[%d] = %q, want %q", i, sc.Resources[i], tc.wantResources[i])
					}
				}
			}
			if tc.wantTaints != nil {
				if len(sc.Taints) != len(tc.wantTaints) {
					t.Fatalf("Taints = %v, want %v", sc.Taints, tc.wantTaints)
				}
				for i := range tc.wantTaints {
					if sc.Taints[i] != tc.wantTaints[i] {
						t.Errorf("Taints[%d] = %q, want %q", i, sc.Taints[i], tc.wantTaints[i])
					}
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Message parsing: eviction resource
// ---------------------------------------------------------------------------

func TestHTParseEvictionResource(t *testing.T) {
	tests := []struct {
		name string
		msg  string
		want string
	}{
		{"memory", "The node was low on resource: memory. Container api was using ...", "memory"},
		{"ephemeral-storage", "The node was low on resource: ephemeral-storage. Usage exceeds ...", "ephemeral-storage"},
		{"pids", "The node was low on resource: pids. Too many processes", "pids"},
		{"disk word only", "the node ran out of disk space entirely", "ephemeral-storage"},
		{"unrecognized", "the pod was evicted for reasons unrelated to any known resource", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseEvictionResource(tc.msg); got != tc.want {
				t.Errorf("parseEvictionResource(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Log-tail pattern scanning
// ---------------------------------------------------------------------------

func TestHTLogPatternsEachNeedleMatches(t *testing.T) {
	for _, p := range logPatterns() {
		for _, needle := range p.needles {
			line, ok := findLine([]string{"prefix " + needle + " suffix"}, p)
			if !ok {
				t.Errorf("pattern %q: needle %q did not match its own line", p.label, needle)
				continue
			}
			if line == "" {
				t.Errorf("pattern %q: matched line is empty", p.label)
			}
		}
	}
}

func TestHTScanLogTailCaseSensitivity(t *testing.T) {
	t.Run("lowercase panic: does not match (case sensitive)", func(t *testing.T) {
		hints := scanLogTail([]string{"this is not a match: PANIC: something"})
		for _, h := range hints {
			if h.Label == "go panic" {
				t.Errorf("lowercase-vs-case PANIC: unexpectedly matched go panic pattern: %+v", h)
			}
		}
	})
	t.Run("uppercase panic: does match", func(t *testing.T) {
		hints := scanLogTail([]string{"panic: runtime error"})
		found := false
		for _, h := range hints {
			if h.Label == "go panic" {
				found = true
			}
		}
		if !found {
			t.Error("exact-case 'panic:' should match the go panic pattern")
		}
	})
	t.Run("lowercase fatal alone does not match fatal pattern", func(t *testing.T) {
		hints := scanLogTail([]string{"this fatal error was lowercase only"})
		for _, h := range hints {
			if h.Label == "fatal log line" {
				t.Errorf("lowercase 'fatal' unexpectedly matched the case-sensitive fatal pattern: %+v", h)
			}
		}
	})
	t.Run("FATAL uppercase matches fatal pattern", func(t *testing.T) {
		hints := scanLogTail([]string{"FATAL: could not connect"})
		found := false
		for _, h := range hints {
			if h.Label == "fatal log line" {
				found = true
			}
		}
		if !found {
			t.Error("uppercase 'FATAL' should match the fatal log line pattern")
		}
	})
	t.Run("Fatal capitalized matches fatal pattern", func(t *testing.T) {
		hints := scanLogTail([]string{"Fatal error occurred during startup"})
		found := false
		for _, h := range hints {
			if h.Label == "fatal log line" {
				found = true
			}
		}
		if !found {
			t.Error("capitalized 'Fatal' should match the fatal log line pattern")
		}
	})
}

func TestHTScanLogTailMostSpecificFirst(t *testing.T) {
	// Both "go panic" and "fatal log line" patterns match; go panic is earlier
	// in logPatterns() and must appear first in the returned hints regardless
	// of line order in the log tail.
	lines := []string{"FATAL: shutting down", "panic: something bad happened"}
	hints := scanLogTail(lines)
	if len(hints) < 2 {
		t.Fatalf("expected at least 2 hints, got %+v", hints)
	}
	if hints[0].Label != "go panic" {
		t.Errorf("hints[0].Label = %q, want %q (most specific pattern first)", hints[0].Label, "go panic")
	}
	foundFatal := false
	for _, h := range hints {
		if h.Label == "fatal log line" {
			foundFatal = true
		}
	}
	if !foundFatal {
		t.Error("expected the fatal log line hint to also be present")
	}
}

func TestHTScanLogTailEmpty(t *testing.T) {
	if got := scanLogTail(nil); got != nil {
		t.Errorf("scanLogTail(nil) = %+v, want nil", got)
	}
	if got := scanLogTail([]string{}); got != nil {
		t.Errorf("scanLogTail(empty) = %+v, want nil", got)
	}
}

func TestHTFindLineNotFound(t *testing.T) {
	p := logPatterns()[0]
	line, ok := findLine([]string{"nothing interesting here"}, p)
	if ok || line != "" {
		t.Errorf("findLine() = (%q, %v), want (\"\", false)", line, ok)
	}
}

// ---------------------------------------------------------------------------
// Image pull context and classification
// ---------------------------------------------------------------------------

func TestHTImagePullContextAndClassification(t *testing.T) {
	t.Run("no pull failure present", func(t *testing.T) {
		in := baseInputs()
		_, ok := imagePullContext(in)
		if ok {
			t.Error("imagePullContext() ok = true, want false for a healthy container")
		}
		_, _, ok = imagePullClassification(in)
		if ok {
			t.Error("imagePullClassification() ok = true, want false for a healthy container")
		}
	})

	t.Run("auth category", func(t *testing.T) {
		in := baseInputs()
		in.Waiting = WaitingState{Present: true, Reason: "ErrImagePull"}
		in.Events = []Event{warning("Failed", "Failed to pull image \"nginx:bogus\": rpc error: code = Unknown desc = "+
			"Error response from daemon: pull access denied for nginx, repository does not exist or may require "+
			"'docker login': denied: requested access to the resource is denied", 1)}
		category, msg, ok := imagePullClassification(in)
		if !ok || category != "auth" {
			t.Fatalf("imagePullClassification() = (%q, %v), want (auth, true); msg=%q", category, ok, msg)
		}
	})

	t.Run("not_found category", func(t *testing.T) {
		in := baseInputs()
		in.Waiting = WaitingState{Present: true, Reason: "ErrImagePull"}
		in.Events = []Event{warning("Failed", "Failed to pull image \"nginx:doesnotexist\": rpc error: code = NotFound "+
			"desc = manifest unknown: manifest unknown", 1)}
		category, _, ok := imagePullClassification(in)
		if !ok || category != "not_found" {
			t.Fatalf("imagePullClassification() category = %q, ok = %v, want (not_found, true)", category, ok)
		}
	})

	t.Run("other category", func(t *testing.T) {
		in := baseInputs()
		in.Waiting = WaitingState{Present: true, Reason: "ErrImagePull"}
		in.Events = []Event{warning("Failed", "Failed to pull image \"nginx:latest\": rpc error: code = Unknown desc = "+
			"Get \"https://registry-1.docker.io/v2/\": context deadline exceeded", 1)}
		category, _, ok := imagePullClassification(in)
		if !ok || category != "other" {
			t.Fatalf("imagePullClassification() category = %q, ok = %v, want (other, true)", category, ok)
		}
	})

	t.Run("informative message wins over tied bare ErrImagePull events", func(t *testing.T) {
		// A single bad-tag pull yields several events with the same
		// one-second timestamp; sorted order between them is arbitrary and
		// "Error: ErrImagePull" sorts before "Failed to pull image ...".
		// The detailed not-found message must still win.
		in := baseInputs()
		in.Waiting = WaitingState{Present: true, Reason: "ImagePullBackOff", Message: "Back-off pulling image \"nginx:doesnotexist\""}
		in.Events = []Event{
			warning("Failed", "Error: ErrImagePull", 3),
			warning("Failed", "Failed to pull image \"nginx:doesnotexist\": rpc error: code = NotFound "+
				"desc = failed to pull and unpack image \"docker.io/library/nginx:doesnotexist\": "+
				"docker.io/library/nginx:doesnotexist: not found", 3),
			warning("BackOff", "Back-off pulling image \"nginx:doesnotexist\"", 5),
		}
		category, msg, ok := imagePullClassification(in)
		if !ok || category != "not_found" {
			t.Fatalf("imagePullClassification() = (%q, %v), want (not_found, true); msg=%q", category, ok, msg)
		}
		if !strings.Contains(msg, "not found") {
			t.Errorf("imagePullContext() picked %q, want the detailed not-found message", msg)
		}
	})

	t.Run("falls back to waiting message when no useful event exists", func(t *testing.T) {
		in := baseInputs()
		in.Waiting = WaitingState{Present: true, Reason: "ImagePullBackOff", Message: "Back-off pulling image \"nginx:bogus\""}
		msg, ok := imagePullContext(in)
		if !ok || msg != in.Waiting.Message {
			t.Errorf("imagePullContext() = (%q, %v), want (%q, true)", msg, ok, in.Waiting.Message)
		}
	})
}

func TestHTImagePullEvidence(t *testing.T) {
	in := baseInputs()
	in.Waiting = WaitingState{Present: true, Reason: "ImagePullBackOff"}
	in.Image = "registry.example.com/team/api:bogus"
	msg := "Failed to pull image \"registry.example.com/team/api:bogus\": manifest unknown"
	in.Events = []Event{warning("Failed", msg, 2)}

	ev := imagePullEvidence(in, msg)
	if !htContainsLine(ev, "waiting reason: ImagePullBackOff") {
		t.Errorf("imagePullEvidence() missing waiting reason line, got %v", ev)
	}
	if !htContainsLine(ev, "image: registry.example.com/team/api:bogus") {
		t.Errorf("imagePullEvidence() missing image line, got %v", ev)
	}
	if !htContainsSubstr(ev, "event: Failed") {
		t.Errorf("imagePullEvidence() missing event line, got %v", ev)
	}

	t.Run("falls back to registry message when nothing else cites it", func(t *testing.T) {
		bare := baseInputs()
		got := imagePullEvidence(bare, "some standalone registry error")
		if !htContainsLine(got, "registry message: some standalone registry error") {
			t.Errorf("imagePullEvidence() = %v, want a registry message fallback line", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Config error context
// ---------------------------------------------------------------------------

func TestHTConfigErrorContext(t *testing.T) {
	t.Run("waiting reason path", func(t *testing.T) {
		in := baseInputs()
		in.Waiting = WaitingState{Present: true, Reason: "CreateContainerConfigError", Message: `secret "db-credentials" not found`}
		msg, ok := configErrorContext(in)
		if !ok || msg != `secret "db-credentials" not found` {
			t.Errorf("configErrorContext() = (%q, %v), want the waiting message", msg, ok)
		}
	})

	t.Run("waiting reason with empty message falls back to Failed event", func(t *testing.T) {
		in := baseInputs()
		in.Waiting = WaitingState{Present: true, Reason: "CreateContainerConfigError", Message: ""}
		in.Events = []Event{warning("Failed", `secret "db-credentials" not found`, 1)}
		msg, ok := configErrorContext(in)
		if !ok || msg != `secret "db-credentials" not found` {
			t.Errorf("configErrorContext() = (%q, %v), want the event message", msg, ok)
		}
	})

	t.Run("event path without waiting present", func(t *testing.T) {
		in := baseInputs()
		in.Events = []Event{warning("Failed", `configmap "app-config" not found`, 1)}
		msg, ok := configErrorContext(in)
		if !ok || msg != `configmap "app-config" not found` {
			t.Errorf("configErrorContext() = (%q, %v), want the event message", msg, ok)
		}
	})

	t.Run("negative case", func(t *testing.T) {
		in := baseInputs()
		if _, ok := configErrorContext(in); ok {
			t.Error("configErrorContext() ok = true, want false for a healthy container")
		}
	})
}

// ---------------------------------------------------------------------------
// Probe budget and observed startup window
// ---------------------------------------------------------------------------

func TestHTProbeBudget(t *testing.T) {
	t.Run("undefined probe", func(t *testing.T) {
		_, _, ok := probeBudget("liveness", ProbeSpec{})
		if ok {
			t.Error("probeBudget(undefined) ok = true, want false")
		}
	})
	t.Run("zero failure threshold", func(t *testing.T) {
		_, _, ok := probeBudget("liveness", ProbeSpec{Defined: true, FailureThreshold: 0, PeriodSeconds: 5})
		if ok {
			t.Error("probeBudget(zero failureThreshold) ok = true, want false")
		}
	})
	t.Run("zero period seconds", func(t *testing.T) {
		_, _, ok := probeBudget("liveness", ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 0})
		if ok {
			t.Error("probeBudget(zero periodSeconds) ok = true, want false")
		}
	})
	t.Run("basic budget without extras", func(t *testing.T) {
		line, budget, ok := probeBudget("liveness", ProbeSpec{Defined: true, FailureThreshold: 3, PeriodSeconds: 10})
		if !ok {
			t.Fatal("probeBudget() ok = false, want true")
		}
		if budget != 30*time.Second {
			t.Errorf("budget = %v, want 30s", budget)
		}
		want := "liveness probe config: failureThreshold=3 x periodSeconds=10 = 30s before the kubelet acts"
		if line != want {
			t.Errorf("line = %q, want %q", line, want)
		}
	})
	t.Run("with initial delay and timeout", func(t *testing.T) {
		line, budget, ok := probeBudget("startup", ProbeSpec{
			Defined: true, FailureThreshold: 3, PeriodSeconds: 10, InitialDelaySeconds: 5, TimeoutSeconds: 2,
		})
		if !ok {
			t.Fatal("probeBudget() ok = false, want true")
		}
		if budget != 35*time.Second {
			t.Errorf("budget = %v, want 35s (30s + 5s initial delay)", budget)
		}
		want := "startup probe config: failureThreshold=3 x periodSeconds=10 = 30s before the kubelet acts" +
			" (plus initialDelaySeconds=5), timeoutSeconds=2"
		if line != want {
			t.Errorf("line = %q, want %q", line, want)
		}
	})
}

func TestHTObservedStartupWindow(t *testing.T) {
	t.Run("from termination timestamps", func(t *testing.T) {
		in := baseInputs()
		in.LastTermination = terminated(1, "Error") // StartedAt 5m before FinishedAt 1m before fixedNow
		got, ok := observedStartupWindow(in)
		if !ok {
			t.Fatal("observedStartupWindow() ok = false, want true")
		}
		want := in.LastTermination.FinishedAt.Sub(in.LastTermination.StartedAt)
		if got != want {
			t.Errorf("observedStartupWindow() = %v, want %v", got, want)
		}
	})
	t.Run("from running duration", func(t *testing.T) {
		in := baseInputs()
		in.Running = RunningState{Present: true, StartedAt: fixedNow.Add(-3 * time.Minute)}
		in.RunningDuration = 3 * time.Minute
		got, ok := observedStartupWindow(in)
		if !ok || got != 3*time.Minute {
			t.Errorf("observedStartupWindow() = (%v, %v), want (3m, true)", got, ok)
		}
	})
	t.Run("not derivable", func(t *testing.T) {
		in := baseInputs()
		_, ok := observedStartupWindow(in)
		if ok {
			t.Error("observedStartupWindow() ok = true, want false when nothing is derivable")
		}
	})
}

// ---------------------------------------------------------------------------
// sortedResourceEvidence, capitalizeFirst, quoteAll
// ---------------------------------------------------------------------------

func TestHTSortedResourceEvidence(t *testing.T) {
	t.Run("empty map yields nil", func(t *testing.T) {
		if got := sortedResourceEvidence("requests", nil); got != nil {
			t.Errorf("sortedResourceEvidence(nil) = %v, want nil", got)
		}
		if got := sortedResourceEvidence("requests", map[string]string{}); got != nil {
			t.Errorf("sortedResourceEvidence(empty map) = %v, want nil", got)
		}
	})
	t.Run("deterministic sorted order", func(t *testing.T) {
		m := map[string]string{"memory": "128Mi", "cpu": "500m", "ephemeral-storage": "1Gi"}
		got := sortedResourceEvidence("container requests", m)
		want := []string{"container requests: cpu=500m, ephemeral-storage=1Gi, memory=128Mi"}
		if len(got) != 1 || got[0] != want[0] {
			t.Errorf("sortedResourceEvidence() = %v, want %v", got, want)
		}
		// Run again to confirm map-iteration order never changes the result.
		got2 := sortedResourceEvidence("container requests", m)
		if got2[0] != got[0] {
			t.Errorf("sortedResourceEvidence() is not deterministic: %q vs %q", got[0], got2[0])
		}
	})
}

func TestHTCapitalizeFirst(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", ""},
		{"hello", "Hello"},
		{"a", "A"},
		{"Already", "Already"},
	}
	for _, tc := range tests {
		if got := capitalizeFirst(tc.in); got != tc.want {
			t.Errorf("capitalizeFirst(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHTQuoteAll(t *testing.T) {
	got := quoteAll([]string{"data", "config"})
	want := []string{`"data"`, `"config"`}
	if len(got) != len(want) {
		t.Fatalf("quoteAll() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("quoteAll()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if got := quoteAll(nil); len(got) != 0 {
		t.Errorf("quoteAll(nil) = %v, want empty", got)
	}
}

// ---------------------------------------------------------------------------
// somethingIsWrong / isProblemWaitingReason / isLifecycleNoise / normalizeInputs
// ---------------------------------------------------------------------------

func TestHTSomethingIsWrong(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Inputs)
		want   bool
	}{
		{"healthy baseline", func(_ *Inputs) {}, false},
		{"restart count > 0", func(in *Inputs) { in.RestartCount = 1 }, true},
		{"termination with nonzero exit", func(in *Inputs) { in.LastTermination = terminated(1, "Error") }, true},
		{"termination with nonzero signal only", func(in *Inputs) {
			in.LastTermination = TerminationState{Present: true, Signal: 9}
		}, true},
		{"termination present but exit 0 signal 0", func(in *Inputs) {
			in.LastTermination = TerminationState{Present: true, ExitCode: 0, Signal: 0}
		}, false},
		{"active deadline exceeded", func(in *Inputs) { in.ActiveDeadlineExceeded = true }, true},
		{"pod phase Failed", func(in *Inputs) { in.PodPhase = "Failed" }, true},
		{"pod reason set", func(in *Inputs) { in.PodReason = "NodeLost" }, true},
		{"waiting with a problem reason", func(in *Inputs) {
			in.Waiting = WaitingState{Present: true, Reason: "CrashLoopBackOff"}
		}, true},
		{"waiting with ContainerCreating is not a problem", func(in *Inputs) {
			in.Waiting = WaitingState{Present: true, Reason: "ContainerCreating"}
		}, false},
		{"waiting with PodInitializing is not a problem", func(in *Inputs) {
			in.Waiting = WaitingState{Present: true, Reason: "PodInitializing"}
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			tc.mutate(&in)
			if got := somethingIsWrong(in); got != tc.want {
				t.Errorf("somethingIsWrong() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHTIsProblemWaitingReason(t *testing.T) {
	tests := []struct {
		reason string
		want   bool
	}{
		{"", false},
		{"ContainerCreating", false},
		{"containercreating", false},
		{"PodInitializing", false},
		{"podinitializing", false},
		{"CrashLoopBackOff", true},
		{"ImagePullBackOff", true},
		{"CreateContainerConfigError", true},
	}
	for _, tc := range tests {
		if got := isProblemWaitingReason(tc.reason); got != tc.want {
			t.Errorf("isProblemWaitingReason(%q) = %v, want %v", tc.reason, got, tc.want)
		}
	}
}

func TestHTIsLifecycleNoise(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Inputs)
		want   bool
	}{
		{"not deleting, not rolling", func(in *Inputs) { in.LastTermination = terminated(143, "Error") }, false},
		{"deleting but no termination present", func(in *Inputs) { in.Deleting = true }, false},
		{"deleting with exit 143", func(in *Inputs) {
			in.Deleting = true
			in.LastTermination = terminated(143, "Error")
		}, true},
		{"deleting with signal 15", func(in *Inputs) {
			in.Deleting = true
			in.LastTermination = TerminationState{Present: true, Signal: 15}
		}, true},
		{"deleting with exit 1 is not noise", func(in *Inputs) {
			in.Deleting = true
			in.LastTermination = terminated(1, "Error")
		}, false},
		{"owner rolling with exit 143", func(in *Inputs) {
			in.OwnerRolling = true
			in.LastTermination = terminated(143, "Error")
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			tc.mutate(&in)
			if got := isLifecycleNoise(in); got != tc.want {
				t.Errorf("isLifecycleNoise() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHTNormalizeInputs(t *testing.T) {
	t.Run("defaults kind and threshold", func(t *testing.T) {
		in := baseInputs()
		in.Kind = ""
		in.InitStuckThreshold = 0
		got := normalizeInputs(in)
		if got.Kind != KindApp {
			t.Errorf("Kind = %q, want %q", got.Kind, KindApp)
		}
		if got.InitStuckThreshold != defaultInitStuckThreshold {
			t.Errorf("InitStuckThreshold = %v, want %v", got.InitStuckThreshold, defaultInitStuckThreshold)
		}
	})
	t.Run("keeps explicit values", func(t *testing.T) {
		in := baseInputs()
		in.Kind = KindInit
		in.InitStuckThreshold = 25 * time.Minute
		got := normalizeInputs(in)
		if got.Kind != KindInit {
			t.Errorf("Kind = %q, want %q", got.Kind, KindInit)
		}
		if got.InitStuckThreshold != 25*time.Minute {
			t.Errorf("InitStuckThreshold = %v, want 25m", got.InitStuckThreshold)
		}
	})
}

// ---------------------------------------------------------------------------
// Rule.appliesToKind
// ---------------------------------------------------------------------------

func TestHTRuleAppliesToKind(t *testing.T) {
	t.Run("empty AppliesTo applies to every kind", func(t *testing.T) {
		r := Rule{Cause: CauseUnknown}
		for _, k := range []ContainerKind{KindApp, KindInit, KindEphemeral} {
			if !r.appliesToKind(k) {
				t.Errorf("appliesToKind(%s) = false, want true for empty AppliesTo", k)
			}
		}
	})
	t.Run("restricted AppliesTo", func(t *testing.T) {
		r := Rule{Cause: CauseAppExitNonzero, AppliesTo: []ContainerKind{KindApp}}
		if !r.appliesToKind(KindApp) {
			t.Error("appliesToKind(KindApp) = false, want true")
		}
		if r.appliesToKind(KindInit) {
			t.Error("appliesToKind(KindInit) = true, want false")
		}
		if r.appliesToKind(KindEphemeral) {
			t.Error("appliesToKind(KindEphemeral) = true, want false")
		}
	})
}

// ---------------------------------------------------------------------------
// appendEvidence / appendSteps / newDiagnosis / finalize / promotePrimary
// ---------------------------------------------------------------------------

func TestHTAppendEvidenceSkipsEmptyAndDuplicates(t *testing.T) {
	d := &Diagnosis{}
	appendEvidence(d, "first", "", "second", "first")
	want := []string{"first", "second"}
	if len(d.Evidence) != len(want) {
		t.Fatalf("Evidence = %v, want %v", d.Evidence, want)
	}
	for i := range want {
		if d.Evidence[i] != want[i] {
			t.Errorf("Evidence[%d] = %q, want %q", i, d.Evidence[i], want[i])
		}
	}
	appendEvidence(d, "first")
	if len(d.Evidence) != 2 {
		t.Errorf("appendEvidence must skip a duplicate across calls, got %v", d.Evidence)
	}
}

func TestHTAppendStepsSkipsEmptyAndDuplicates(t *testing.T) {
	d := &Diagnosis{}
	appendSteps(d, "do this", "", "do that", "do this")
	want := []string{"do this", "do that"}
	if len(d.NextSteps) != len(want) {
		t.Fatalf("NextSteps = %v, want %v", d.NextSteps, want)
	}
	for i := range want {
		if d.NextSteps[i] != want[i] {
			t.Errorf("NextSteps[%d] = %q, want %q", i, d.NextSteps[i], want[i])
		}
	}
}

func TestHTNewDiagnosis(t *testing.T) {
	in := baseInputs()
	d := newDiagnosis(in, CauseOOMKilled, ConfidenceHigh)
	if d.Cause != CauseOOMKilled {
		t.Errorf("Cause = %q, want %q", d.Cause, CauseOOMKilled)
	}
	if d.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want %q", d.Confidence, ConfidenceHigh)
	}
	if d.Container != in.Container || d.Pod != in.Pod || d.Namespace != in.Namespace {
		t.Errorf("identity fields not filled: %+v", d)
	}
	if d.Owner != in.Owner {
		t.Errorf("Owner = %+v, want %+v", d.Owner, in.Owner)
	}
	if !d.Timestamp.Equal(in.Now) {
		t.Errorf("Timestamp = %v, want %v", d.Timestamp, in.Now)
	}
	if d.Evidence != nil || d.NextSteps != nil {
		t.Errorf("newDiagnosis should not pre-populate Evidence/NextSteps, got %+v", d)
	}
}

func TestHTFinalize(t *testing.T) {
	t.Run("stamps identity and clock", func(t *testing.T) {
		in := baseInputs()
		d := &Diagnosis{Cause: CauseUnknown}
		finalize(d, in)
		if d.Pod != in.Pod || d.Namespace != in.Namespace || d.Container != in.Container || d.Owner != in.Owner {
			t.Errorf("finalize did not stamp identity: %+v", d)
		}
		if !d.Timestamp.Equal(in.Now) {
			t.Errorf("Timestamp = %v, want %v", d.Timestamp, in.Now)
		}
	})
	t.Run("falls back to wall clock when Now is zero and Timestamp unset", func(t *testing.T) {
		in := baseInputs()
		in.Now = time.Time{}
		d := &Diagnosis{}
		before := time.Now()
		finalize(d, in)
		if d.Timestamp.Before(before) {
			t.Errorf("Timestamp = %v, want >= %v (wall clock fallback)", d.Timestamp, before)
		}
	})
	t.Run("keeps a pre-set Timestamp when Now is zero", func(t *testing.T) {
		in := baseInputs()
		in.Now = time.Time{}
		preset := fixedNow.Add(-24 * time.Hour)
		d := &Diagnosis{Timestamp: preset}
		finalize(d, in)
		if !d.Timestamp.Equal(preset) {
			t.Errorf("Timestamp = %v, want unchanged preset %v", d.Timestamp, preset)
		}
	})
}

func TestHTPromotePrimary(t *testing.T) {
	t.Run("empty slice", func(t *testing.T) {
		if got := promotePrimary(nil); got != nil {
			t.Errorf("promotePrimary(nil) = %v, want nil", got)
		}
	})
	t.Run("single element", func(t *testing.T) {
		ds := []Diagnosis{{Cause: CauseUnknown, Confidence: ConfidenceLow}}
		got := promotePrimary(ds)
		if len(got) != 1 || got[0].Cause != CauseUnknown {
			t.Errorf("promotePrimary(single) = %v, want unchanged", got)
		}
	})
	t.Run("high confidence already at index 0", func(t *testing.T) {
		ds := []Diagnosis{
			{Cause: CauseOOMKilled, Confidence: ConfidenceHigh},
			{Cause: CauseAppExitNonzero, Confidence: ConfidenceLow},
		}
		got := promotePrimary(ds)
		if got[0].Cause != CauseOOMKilled || got[1].Cause != CauseAppExitNonzero {
			t.Errorf("order changed when high confidence was already first: %v", causes(got))
		}
	})
	t.Run("high confidence at index 2 is promoted, rest preserved in order", func(t *testing.T) {
		ds := []Diagnosis{
			{Cause: CauseSigkillUnattributed, Confidence: ConfidenceMedium},
			{Cause: CauseAppExitNonzero, Confidence: ConfidenceLow},
			{Cause: CauseEvicted, Confidence: ConfidenceHigh},
			{Cause: CauseUnknown, Confidence: ConfidenceLow},
		}
		got := promotePrimary(ds)
		want := []CauseCode{CauseEvicted, CauseSigkillUnattributed, CauseAppExitNonzero, CauseUnknown}
		if len(got) != len(want) {
			t.Fatalf("promotePrimary() = %v, want %v", causes(got), want)
		}
		for i := range want {
			if got[i].Cause != want[i] {
				t.Errorf("promotePrimary()[%d] = %s, want %s (all: %v)", i, got[i].Cause, want[i], causes(got))
			}
		}
	})
	t.Run("no high confidence at all preserves order", func(t *testing.T) {
		ds := []Diagnosis{
			{Cause: CauseSigkillUnattributed, Confidence: ConfidenceMedium},
			{Cause: CauseAppExitNonzero, Confidence: ConfidenceLow},
		}
		got := promotePrimary(ds)
		if got[0].Cause != CauseSigkillUnattributed || got[1].Cause != CauseAppExitNonzero {
			t.Errorf("order changed without a high-confidence match: %v", causes(got))
		}
	})
}

// ---------------------------------------------------------------------------
// unknownRule evidence branch coverage (via Classify)
// ---------------------------------------------------------------------------

func TestHTUnknownRuleEvidenceBranches(t *testing.T) {
	t.Run("pod phase Failed with PodReason and PodMessage", func(t *testing.T) {
		in := baseInputs()
		in.PodPhase = "Failed"
		in.PodReason = "NodeLost"
		in.PodMessage = "the node became unresponsive"

		d := diagnosisFor(t, Classify(in), CauseUnknown)
		if !evidenceContains(d, "pod phase: Failed") {
			t.Errorf("evidence must cite the pod phase, got %v", d.Evidence)
		}
		if !evidenceContains(d, "pod status reason: NodeLost") {
			t.Errorf("evidence must cite the pod reason, got %v", d.Evidence)
		}
		if !evidenceContains(d, "pod status message: the node became unresponsive") {
			t.Errorf("evidence must cite the pod message, got %v", d.Evidence)
		}
	})

	t.Run("activeDeadlineExceeded on an app container", func(t *testing.T) {
		in := baseInputs()
		in.Kind = KindApp
		in.ActiveDeadlineExceeded = true

		d := diagnosisFor(t, Classify(in), CauseUnknown)
		if !evidenceContains(d, "pod activeDeadlineSeconds exceeded") {
			t.Errorf("evidence must cite the exceeded deadline, got %v", d.Evidence)
		}
	})

	t.Run("currently running with restarts and warning events", func(t *testing.T) {
		in := baseInputs()
		in.RestartCount = 2
		in.Running = RunningState{Present: true, StartedAt: fixedNow.Add(-30 * time.Second)}
		in.RunningDuration = 30 * time.Second
		in.Events = []Event{warning("SomeUnusualReason", "an unusual thing happened", 1)}

		d := diagnosisFor(t, Classify(in), CauseUnknown)
		if !evidenceContains(d, "container is currently running (for 30s)") {
			t.Errorf("evidence must cite the running duration, got %v", d.Evidence)
		}
		if !evidenceContains(d, "event: SomeUnusualReason") {
			t.Errorf("evidence must cite the warning event, got %v", d.Evidence)
		}
	})

	t.Run("logs unavailable", func(t *testing.T) {
		in := baseInputs()
		in.RestartCount = 1
		in.LogsUnavailable = true

		d := diagnosisFor(t, Classify(in), CauseUnknown)
		if !evidenceContains(d, "logs were unavailable (collection disabled, rate-limited, or failed)") {
			t.Errorf("evidence must cite unavailable logs, got %v", d.Evidence)
		}
	})

	t.Run("log-tail hints", func(t *testing.T) {
		in := baseInputs()
		in.RestartCount = 1
		in.LogTail = []string{"panic: boom"}

		d := diagnosisFor(t, Classify(in), CauseUnknown)
		if !evidenceContains(d, "log hint (go panic): panic: boom") {
			t.Errorf("evidence must cite the log-tail hint, got %v", d.Evidence)
		}
	})
}

// ---------------------------------------------------------------------------
// small local test helpers (ht-prefixed to avoid collisions)
// ---------------------------------------------------------------------------

func htContainsLine(lines []string, want string) bool {
	for _, l := range lines {
		if l == want {
			return true
		}
	}
	return false
}

func htContainsSubstr(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
