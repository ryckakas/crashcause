// Package engine is the pure classification engine for crashcause.
//
// It performs no I/O of any kind: no Kubernetes API calls, no filesystem
// access, no network access. It consumes a fully-populated Inputs value
// (assembled by collectors elsewhere in the program) and produces zero or
// more Diagnosis values. Because it is pure, it must never import any
// k8s.io package — collectors translate Kubernetes objects into the plain
// Go types declared here, and this package classifies those plain types.
package engine

import "time"

// CauseCode is a stable, machine-readable identifier for a crash cause.
// Values are part of the tool's external contract (used in JSON output and
// metric labels) and must not be renumbered or renamed once released.
type CauseCode string

// CauseCode values enumerated by the engine, covering every crash cause the
// classifier can currently detect (see AllCauses for the exhaustive list).
const (
	CauseOOMKilled            CauseCode = "oom_killed"
	CauseSigkillUnattributed  CauseCode = "sigkill_unattributed"
	CauseEvicted              CauseCode = "evicted"
	CauseProbeLiveness        CauseCode = "probe_liveness_failure"
	CauseProbeStartup         CauseCode = "probe_startup_failure"
	CauseImagePullAuth        CauseCode = "image_pull_auth"
	CauseImagePullNotFound    CauseCode = "image_pull_not_found"
	CauseImagePullOther       CauseCode = "image_pull_other"
	CauseConfigMissingRef     CauseCode = "config_missing_reference"
	CauseVolumeMountFailure   CauseCode = "volume_mount_failure"
	CauseInitContainerFailure CauseCode = "init_container_failure"
	CauseInitContainerStuck   CauseCode = "init_container_stuck"
	CauseUnschedulable        CauseCode = "unschedulable"
	CauseAppExitNonzero       CauseCode = "app_exit_nonzero"
	CauseSigkillAfterGrace    CauseCode = "sigkill_after_grace"
	CauseCompletedRestartLoop CauseCode = "completed_restart_loop"
	CauseUnknown              CauseCode = "unknown"
)

// AllCauses lists every CauseCode (for exhaustiveness checks and metric docs).
func AllCauses() []CauseCode {
	return []CauseCode{
		CauseOOMKilled,
		CauseSigkillUnattributed,
		CauseEvicted,
		CauseProbeLiveness,
		CauseProbeStartup,
		CauseImagePullAuth,
		CauseImagePullNotFound,
		CauseImagePullOther,
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
}

// Confidence expresses how certain the engine is about a Diagnosis.
type Confidence string

// Confidence values, from most to least certain.
const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// ContainerKind distinguishes the role a container plays in a pod.
type ContainerKind string

// ContainerKind values a pod's containers may take on.
const (
	KindApp       ContainerKind = "app"
	KindInit      ContainerKind = "init"
	KindEphemeral ContainerKind = "ephemeral"
)

// Diagnosis is the output of the classification engine for a single
// container of a single pod.
type Diagnosis struct {
	Cause       CauseCode  `json:"cause"`
	Confidence  Confidence `json:"confidence"`
	Explanation string     `json:"explanation"`
	Evidence    []string   `json:"evidence"`
	NextSteps   []string   `json:"next_steps"`
	Container   string     `json:"container"`
	Pod         string     `json:"pod"`
	Namespace   string     `json:"namespace"`
	Owner       Owner      `json:"owner"`
	Timestamp   time.Time  `json:"timestamp"`
	AISummary   *string    `json:"ai_summary,omitempty"`
}

// TerminationState mirrors a container's terminated state (current or last).
type TerminationState struct {
	Present    bool
	ExitCode   int32
	Signal     int32
	Reason     string // "OOMKilled", "Error", "Completed", ...
	Message    string
	StartedAt  time.Time
	FinishedAt time.Time
}

// WaitingState mirrors a container's waiting state.
type WaitingState struct {
	Present bool
	Reason  string // "CrashLoopBackOff", "ImagePullBackOff", "CreateContainerConfigError", ...
	Message string
}

// RunningState mirrors a container's running state.
type RunningState struct {
	Present   bool
	StartedAt time.Time
}

// Event is a plain-Go projection of a Kubernetes Event relevant to a pod.
type Event struct {
	Type      string // "Normal" | "Warning"
	Reason    string // "Killing", "Unhealthy", "BackOff", "FailedScheduling", "FailedMount", ...
	Message   string
	Count     int32
	FirstSeen time.Time
	LastSeen  time.Time
}

// ProbeSpec is a plain-Go projection of a probe's relevant configuration.
type ProbeSpec struct {
	Defined             bool
	FailureThreshold    int32
	PeriodSeconds       int32
	InitialDelaySeconds int32
	TimeoutSeconds      int32
}

// Owner identifies the controller that owns a pod.
type Owner struct {
	Kind        string `json:"kind,omitempty"` // Deployment, StatefulSet, DaemonSet, ReplicaSet, Job, ...
	Name        string `json:"name,omitempty"`
	CronJobName string `json:"cronjob_name,omitempty"` // set when Kind==Job and the Job is owned by a CronJob
}

// NodeConditions is a plain-Go projection of the node's relevant conditions.
type NodeConditions struct {
	Known          bool // false when node info was not collected
	MemoryPressure bool
	DiskPressure   bool
	PIDPressure    bool
}

// Inputs is everything the pure engine sees about one container of one pod.
// Collectors populate it; the engine performs no I/O.
type Inputs struct {
	Pod           string
	Namespace     string
	Container     string
	Kind          ContainerKind
	Image         string
	PodPhase      string // "Pending", "Running", "Failed", ...
	PodReason     string // pod status.reason, e.g. "Evicted"
	PodMessage    string
	QOSClass      string
	RestartPolicy string // "Always", "OnFailure", "Never"
	RestartCount  int32

	Deleting     bool // pod deletionTimestamp set
	DeletionTime time.Time
	OwnerRolling bool // best-effort: owner is mid rolling-update / scale-down

	LastTermination    TerminationState // lastState.terminated
	CurrentTermination TerminationState // state.terminated
	Waiting            WaitingState
	Running            RunningState
	RunningDuration    time.Duration // Now - Running.StartedAt when running, else 0

	ActiveDeadlineExceeded bool

	Liveness  ProbeSpec
	Startup   ProbeSpec
	Readiness ProbeSpec

	Requests map[string]string // resource -> quantity string, e.g. "memory" -> "128Mi"
	Limits   map[string]string

	Events          []Event
	LogTail         []string
	LogsUnavailable bool // log collection disabled / rate-limited / failed

	Node  NodeConditions
	Owner Owner

	Now                time.Time     // injected clock (engine purity)
	InitStuckThreshold time.Duration // default 10m, from flag
}
