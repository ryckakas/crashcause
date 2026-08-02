#!/usr/bin/env bash
#
# crashcause end-to-end test against a live Kubernetes cluster.
#
# It applies examples/kind-demo/*.yaml (every scenario manifest breaks in
# exactly one way), waits for each pod to actually reach its diagnosable
# state, runs `crashcause inspect --output json` against it, and asserts that
# the engine produced the cause code that manifest is supposed to produce.
# A healthy control pod is created in-script to assert the other half of the
# contract: a pod with nothing wrong must exit 2 with an empty diagnoses list.
#
# By design this assumes a cluster already exists and kubectl points at it --
# that is how CI runs it (the kind cluster is created by the kind action).
# Pass --create-cluster to have the script stand up and tear down its own kind
# cluster, which is the convenient path on a dev machine.
#
# Usage:
#   ./hack/e2e.sh [--create-cluster] [--keep-cluster] [--help]
#
# Environment:
#   CRASHCAUSE_BIN             path to a prebuilt binary (default: build
#                              ./bin/crashcause with `go build`)
#   E2E_TIMEOUT                per-scenario readiness timeout in seconds
#                              (default 120)
#   E2E_INIT_STUCK_THRESHOLD   --init-stuck-threshold for the stuck-init
#                              scenario (default 30s; the tool's own default
#                              is 10m, which is far too long for a test)
#
# Exit status: 0 when every scenario passed, 1 otherwise (including setup
# failures). Scenario failures never abort the run -- every scenario is
# attempted and all failures are reported together at the end.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

MANIFEST_DIR="${REPO_ROOT}/examples/kind-demo"
KIND_CONFIG="${MANIFEST_DIR}/kind-config.yaml"
NAMESPACE="crashcause-demo"
CLUSTER_NAME="crashcause-demo"
HEALTHY_POD="healthy-control"
# Any image that starts and stays up without doing anything. pause is the
# smallest thing in the ecosystem that qualifies and is already present on
# every kind node, so this costs no pull.
HEALTHY_IMAGE="registry.k8s.io/pause:3.9"

TIMEOUT="${E2E_TIMEOUT:-120}"
INIT_STUCK_THRESHOLD="${E2E_INIT_STUCK_THRESHOLD:-30s}"
# Seconds the init container must have been running before crashcause will
# call it stuck. Kept slightly above the 30s threshold so the diagnosis is
# unambiguous rather than racing the clock.
INIT_STUCK_WAIT=35

CREATE_CLUSTER=false
KEEP_CLUSTER=false
CLUSTER_CREATED=false

# Scenario table: name|target|expected_cause|also_accepted|extra inspect args
#
# target is either "pod:<name>" (a bare pod) or "label:<selector>" (resolved
# to a pod name at run time -- bad-image is a Deployment, so its pod name
# carries two generated hashes).
#
# also_accepted is a second cause code that is tolerated (with a WARN) rather
# than failed. It is unset for every scenario: the engine picks the most
# informative pull-failure message regardless of event-timestamp ties, so
# bad-image asserts image_pull_not_found exactly. The mechanism is kept in
# case a future scenario has genuinely environment-dependent output.
SCENARIOS=(
  "oom|pod:oom-demo|oom_killed||"
  "bad-probe|pod:bad-probe-demo|probe_liveness_failure||"
  "missing-secret|pod:missing-secret-demo|config_missing_reference||"
  "bad-image|label:crashcause.dev/scenario=image-pull-not-found|image_pull_not_found||"
  "unschedulable|pod:unschedulable-demo|unschedulable||"
  "stuck-init|pod:stuck-init-demo|init_container_stuck||--init-stuck-threshold ${INIT_STUCK_THRESHOLD}"
)

# Results collected across the run; printed as a table at the end.
RESULT_LINES=()
FAILED=0

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------

log()  { printf '%s\n' "$*"; }
info() { printf '==> %s\n' "$*"; }
warn() { printf 'WARN: %s\n' "$*" >&2; }
err()  { printf 'ERROR: %s\n' "$*" >&2; }

die() {
  err "$*"
  exit 1
}

usage() {
  cat <<'EOF'
crashcause end-to-end test against a live Kubernetes cluster.

Usage:
  ./hack/e2e.sh [--create-cluster] [--keep-cluster] [--help]

Options:
  --create-cluster  create a kind cluster from examples/kind-demo/kind-config.yaml
                    first, and delete it again at the end. Without this flag the
                    script uses whatever cluster kubectl currently points at
                    (this is how CI runs it).
  --keep-cluster    do not delete the cluster this script created. Useful for
                    poking at a failing scenario by hand afterwards.
  -h, --help        show this help.

Environment:
  CRASHCAUSE_BIN            path to a prebuilt binary (default: build
                            ./bin/crashcause with `go build`)
  E2E_TIMEOUT               per-scenario readiness timeout in seconds (default 120)
  E2E_INIT_STUCK_THRESHOLD  --init-stuck-threshold for the stuck-init scenario
                            (default 30s; the tool's own default is 10m)

Exit status: 0 when every scenario passed, 1 otherwise.
EOF
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------

require_tool() {
  local tool="$1" hint="$2"
  if ! command -v "$tool" >/dev/null 2>&1; then
    die "'${tool}' is required but not installed. Install it: ${hint}"
  fi
}

parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      --create-cluster) CREATE_CLUSTER=true ;;
      --keep-cluster)   KEEP_CLUSTER=true ;;
      -h|--help)        usage; exit 0 ;;
      *)                err "unknown argument: $1"; usage; exit 1 ;;
    esac
    shift
  done
}

preflight() {
  # Arrays plus `set -u` need a modern bash: stock macOS /bin/bash is 3.2,
  # where expanding an empty array is an "unbound variable" error. Fail with
  # that sentence rather than with a cryptic one 200 lines later.
  if [ "${BASH_VERSINFO[0]:-0}" -lt 4 ]; then
    die "bash 4 or newer is required (found ${BASH_VERSION:-unknown}). On macOS: brew install bash"
  fi

  require_tool kubectl "https://kubernetes.io/docs/tasks/tools/#kubectl"
  # jq is preinstalled on GitHub's ubuntu runners; on a dev machine it may not
  # be, and every assertion in this script goes through it.
  require_tool jq "https://jqlang.github.io/jq/download/ (apt install jq / brew install jq / choco install jq)"

  if [ "$CREATE_CLUSTER" = true ]; then
    require_tool kind "https://kind.sigs.k8s.io/docs/user/quick-start/#installation"
  fi

  [ -d "$MANIFEST_DIR" ] || die "demo manifests not found at ${MANIFEST_DIR}"
}

# resolve_binary sets BIN to the crashcause binary to test, building it when
# the caller did not supply one.
resolve_binary() {
  if [ -n "${CRASHCAUSE_BIN:-}" ]; then
    [ -x "$CRASHCAUSE_BIN" ] || die "CRASHCAUSE_BIN=${CRASHCAUSE_BIN} is not an executable file"
    BIN="$CRASHCAUSE_BIN"
    info "using prebuilt binary: ${BIN}"
    return
  fi

  BIN="${REPO_ROOT}/bin/crashcause"
  if [ -x "$BIN" ]; then
    info "using existing binary: ${BIN}"
    return
  fi

  require_tool go "https://go.dev/dl/"
  info "building ${BIN}"
  (cd "$REPO_ROOT" && CGO_ENABLED=0 go build -o "$BIN" ./cmd/crashcause)
}

# ---------------------------------------------------------------------------
# Cluster lifecycle
# ---------------------------------------------------------------------------

cleanup() {
  local rc=$?
  if [ "$CLUSTER_CREATED" = true ] && [ "$KEEP_CLUSTER" != true ]; then
    info "deleting kind cluster ${CLUSTER_NAME}"
    kind delete cluster --name "$CLUSTER_NAME" || true
  fi
  return $rc
}

create_cluster() {
  [ "$CREATE_CLUSTER" = true ] || return 0

  [ -f "$KIND_CONFIG" ] || die "kind config not found at ${KIND_CONFIG}"

  if kind get clusters 2>/dev/null | grep -qxF "$CLUSTER_NAME"; then
    info "kind cluster ${CLUSTER_NAME} already exists, reusing it (it will not be deleted)"
    kubectl config use-context "kind-${CLUSTER_NAME}" >/dev/null ||
      die "cluster ${CLUSTER_NAME} exists but its kubectl context does not. Recreate it: kind delete cluster --name ${CLUSTER_NAME}"
    return 0
  fi

  info "creating kind cluster ${CLUSTER_NAME}"
  kind create cluster --config "$KIND_CONFIG"
  CLUSTER_CREATED=true
}

check_cluster() {
  kubectl cluster-info >/dev/null 2>&1 ||
    die "kubectl cannot reach a cluster. Point your kubeconfig at one, or re-run with --create-cluster."
  info "cluster: $(kubectl config current-context)"
}

# ---------------------------------------------------------------------------
# Workload setup
# ---------------------------------------------------------------------------

apply_manifests() {
  info "applying demo manifests from examples/kind-demo/"

  # The namespace has to exist before the workloads that name it.
  kubectl apply -f "${MANIFEST_DIR}/namespace.yaml"

  # kind-config.yaml is a kind Cluster config, NOT a Kubernetes object:
  # applying the directory wholesale would fail on it. Enumerate instead.
  local files=() f base
  shopt -s nullglob
  for f in "${MANIFEST_DIR}"/*.yaml; do
    base="$(basename "$f")"
    case "$base" in
      kind-config.yaml|namespace.yaml) continue ;;
    esac
    files+=("$f")
  done
  shopt -u nullglob

  [ ${#files[@]} -gt 0 ] || die "no workload manifests found in ${MANIFEST_DIR}"
  for f in "${files[@]}"; do
    kubectl apply -f "$f"
  done
}

# create_healthy_pod adds the control case: a pod with nothing wrong with it.
# It is generated here rather than checked into examples/kind-demo/ because
# that directory is documented as "one manifest per crash cause" and a healthy
# pod is not a crash cause. Piping through `kubectl apply` (rather than a bare
# `kubectl run`) keeps re-runs idempotent.
create_healthy_pod() {
  info "creating healthy control pod ${NAMESPACE}/${HEALTHY_POD}"
  kubectl run "$HEALTHY_POD" \
    --namespace "$NAMESPACE" \
    --image "$HEALTHY_IMAGE" \
    --restart Always \
    --dry-run=client -o yaml |
    kubectl apply -f -
}

# ---------------------------------------------------------------------------
# Readiness waits
#
# Every wait polls real pod/event state; none of them is a blind sleep. Each
# is bounded by $TIMEOUT so a broken scenario fails fast with a dump instead
# of hanging the job.
# ---------------------------------------------------------------------------

# wait_until <description> <predicate-function> [args...]
# Polls the predicate once a second until it succeeds or $TIMEOUT elapses.
wait_until() {
  local desc="$1"; shift
  local deadline=$(( $(date +%s) + TIMEOUT ))

  while :; do
    if "$@"; then
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      err "timed out after ${TIMEOUT}s waiting for: ${desc}"
      return 1
    fi
    sleep 2
  done
}

# jsonpath <pod> <path> -- empty string when the pod or field does not exist.
jsonpath() {
  kubectl get pod "$1" -n "$NAMESPACE" -o jsonpath="$2" 2>/dev/null || true
}

# events_exist <pod> <reason>
events_exist() {
  local out
  out="$(kubectl get events -n "$NAMESPACE" \
    --field-selector "involvedObject.name=$1,reason=$2" \
    -o name 2>/dev/null || true)"
  [ -n "$out" ]
}

pod_exists() {
  kubectl get pod "$1" -n "$NAMESPACE" >/dev/null 2>&1
}

# --- per-scenario conditions ------------------------------------------------

# oom: the container must have actually been OOM-killed at least once, which
# is what puts reason=OOMKilled on lastState.terminated (the ONLY signal the
# oom_killed rule accepts).
cond_oom() {
  local pod="$1" reason restarts
  reason="$(jsonpath "$pod" '{.status.containerStatuses[0].lastState.terminated.reason}')"
  [ "$reason" = "OOMKilled" ] && return 0
  restarts="$(jsonpath "$pod" '{.status.containerStatuses[0].restartCount}')"
  [ -n "$restarts" ] && [ "$restarts" -ge 1 ] 2>/dev/null
}

# bad-probe: the liveness probe must have failed enough times to kill the
# container once (restartCount>=1), and both event kinds the rule keys on
# (Unhealthy + Killing) must be visible.
cond_probe() {
  local pod="$1" restarts
  restarts="$(jsonpath "$pod" '{.status.containerStatuses[0].restartCount}')"
  [ -n "$restarts" ] && [ "$restarts" -ge 1 ] 2>/dev/null || return 1
  events_exist "$pod" Unhealthy
}

# missing-secret: kubelet cannot resolve envFrom, so the container never
# starts and parks in CreateContainerConfigError.
cond_config_error() {
  [ "$(jsonpath "$1" '{.status.containerStatuses[0].state.waiting.reason}')" = "CreateContainerConfigError" ]
}

# bad-image: either pull-failure waiting reason is enough -- by the time
# either is set, the kubelet has already emitted the Failed event carrying
# the registry's message, which is what the image_pull_* rules classify.
cond_image_pull() {
  local reason
  reason="$(jsonpath "$1" '{.status.containerStatuses[0].state.waiting.reason}')"
  [ "$reason" = "ErrImagePull" ] || [ "$reason" = "ImagePullBackOff" ]
}

# unschedulable: this pod has no container status at all (nothing ever
# started), so the readiness signal is the scheduler's FailedScheduling
# event -- the same events-only evidence the rule itself consumes.
cond_unschedulable() {
  local phase
  phase="$(jsonpath "$1" '{.status.phase}')"
  [ "$phase" = "Pending" ] || return 1
  events_exist "$1" FailedScheduling
}

# stuck-init: the init container must be Running AND have been running for
# longer than the threshold we pass to inspect. Preferred measurement is the
# kubelet's own startedAt (GNU/BSD date); when `date -d` is unavailable we
# fall back to timing from the first observation, which only ever waits
# longer, never shorter.
INIT_FIRST_SEEN=""
cond_init_stuck() {
  local pod="$1" started now start_epoch elapsed
  started="$(jsonpath "$pod" '{.status.initContainerStatuses[0].state.running.startedAt}')"
  [ -n "$started" ] || return 1

  now="$(date +%s)"
  if start_epoch="$(date -d "$started" +%s 2>/dev/null)"; then
    elapsed=$(( now - start_epoch ))
  else
    [ -n "$INIT_FIRST_SEEN" ] || INIT_FIRST_SEEN="$now"
    elapsed=$(( now - INIT_FIRST_SEEN ))
  fi
  [ "$elapsed" -ge "$INIT_STUCK_WAIT" ]
}

# healthy control: Ready, and stays that way.
cond_healthy() {
  [ "$(jsonpath "$1" '{.status.conditions[?(@.type=="Ready")].status}')" = "True" ]
}

# wait_for_scenario <name> <pod>
wait_for_scenario() {
  local name="$1" pod="$2"
  case "$name" in
    oom)            wait_until "${pod} to be OOM-killed"                       cond_oom "$pod" ;;
    bad-probe)      wait_until "${pod} to be restarted by its liveness probe"  cond_probe "$pod" ;;
    missing-secret) wait_until "${pod} to reach CreateContainerConfigError"    cond_config_error "$pod" ;;
    bad-image)      wait_until "${pod} to reach ErrImagePull/ImagePullBackOff" cond_image_pull "$pod" ;;
    unschedulable)  wait_until "${pod} to be Pending with FailedScheduling"    cond_unschedulable "$pod" ;;
    stuck-init)     wait_until "${pod} init container to be stuck >${INIT_STUCK_WAIT}s" cond_init_stuck "$pod" ;;
    healthy)        wait_until "${pod} to become Ready"                        cond_healthy "$pod" ;;
    *)              err "no readiness condition defined for scenario '${name}'"; return 1 ;;
  esac
}

# ---------------------------------------------------------------------------
# Assertions
# ---------------------------------------------------------------------------

# resolve_pod <target> -- "pod:<name>" verbatim, "label:<selector>" looked up.
resolve_pod() {
  local target="$1" kind value name
  kind="${target%%:*}"
  value="${target#*:}"

  case "$kind" in
    pod)
      printf '%s' "$value"
      ;;
    label)
      # The Deployment-owned pod does not exist the instant the Deployment
      # does, so give the ReplicaSet a bounded moment to create it.
      local deadline=$(( $(date +%s) + TIMEOUT ))
      while :; do
        name="$(kubectl get pods -n "$NAMESPACE" -l "$value" \
          -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
        [ -n "$name" ] && { printf '%s' "$name"; return 0; }
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 2
      done
      ;;
    *)
      return 1
      ;;
  esac
}

# dump_debug <pod> [extra inspect args...] -- everything a reader needs to
# understand a failure without re-running anything.
dump_debug() {
  local pod="$1"; shift
  log ""
  log "---- debug: kubectl describe pod ${NAMESPACE}/${pod} ----"
  kubectl describe pod "$pod" -n "$NAMESPACE" 2>&1 || true
  log "---- debug: events for ${pod} ----"
  kubectl get events -n "$NAMESPACE" --field-selector "involvedObject.name=${pod}" 2>&1 || true
  log "---- debug: crashcause inspect ${pod} (human output) ----"
  "$BIN" inspect "$pod" -n "$NAMESPACE" "$@" 2>&1 || true
  log "---- end debug ----"
  log ""
}

record() {
  RESULT_LINES+=("$1|$2|$3")
}

pass() {
  log "PASS: $1 -- $2"
  record "PASS" "$1" "$2"
}

fail() {
  log "FAIL: $1 -- $2"
  record "FAIL" "$1" "$2"
  FAILED=$(( FAILED + 1 ))
}

# run_scenario <name> <target> <expected> <also_accepted> <extra args...>
#
# Never returns non-zero: a failing scenario is recorded and the run
# continues, so one broken cause code does not hide the other five.
run_scenario() {
  local name="$1" target="$2" expected="$3" accepted="$4"
  shift 4
  local extra=("$@")

  log ""
  info "scenario: ${name} (expecting ${expected})"

  local pod
  if ! pod="$(resolve_pod "$target")" || [ -z "$pod" ]; then
    fail "$name" "could not resolve a pod for target '${target}'"
    return 0
  fi
  log "    pod: ${NAMESPACE}/${pod}"

  if ! wait_for_scenario "$name" "$pod"; then
    fail "$name" "pod never reached its diagnosable state within ${TIMEOUT}s"
    dump_debug "$pod" ${extra[@]+"${extra[@]}"}
    return 0
  fi

  local out rc=0
  out="$("$BIN" inspect "$pod" -n "$NAMESPACE" --output json ${extra[@]+"${extra[@]}"} 2>/dev/null)" || rc=$?

  if [ "$rc" -ne 0 ]; then
    fail "$name" "inspect exited ${rc}, want 0"
    dump_debug "$pod" ${extra[@]+"${extra[@]}"}
    return 0
  fi

  local causes
  if ! causes="$(printf '%s' "$out" | jq -r '.diagnoses[].cause' 2>/dev/null)"; then
    fail "$name" "inspect did not emit parseable JSON"
    log "    raw output: ${out}"
    dump_debug "$pod" ${extra[@]+"${extra[@]}"}
    return 0
  fi
  local flat
  flat="$(printf '%s' "$causes" | tr '\n' ' ' | sed 's/ *$//')"

  if printf '%s\n' "$causes" | grep -qxF "$expected"; then
    pass "$name" "cause=${expected} (exit 0)"
    return 0
  fi

  if [ -n "$accepted" ] && printf '%s\n' "$causes" | grep -qxF "$accepted"; then
    warn "${name}: got '${accepted}' instead of '${expected}' -- accepted, see the note in examples/kind-demo/README.md"
    pass "$name" "cause=${accepted} (accepted fallback for ${expected})"
    return 0
  fi

  fail "$name" "got causes [${flat:-none}], want ${expected}"
  dump_debug "$pod" ${extra[@]+"${extra[@]}"}
  return 0
}

# run_healthy_scenario asserts the other half of the exit-code contract: a pod
# that is not crashing must exit 2 with an empty diagnoses array, NOT exit 0
# with some speculative finding.
run_healthy_scenario() {
  local name="healthy" pod="$HEALTHY_POD"

  log ""
  info "scenario: ${name} (expecting no diagnosis, exit 2)"
  log "    pod: ${NAMESPACE}/${pod}"

  if ! pod_exists "$pod"; then
    fail "$name" "control pod was not created"
    return 0
  fi

  if ! wait_for_scenario "$name" "$pod"; then
    fail "$name" "control pod never became Ready within ${TIMEOUT}s"
    dump_debug "$pod"
    return 0
  fi

  local out rc=0
  out="$("$BIN" inspect "$pod" -n "$NAMESPACE" --output json 2>/dev/null)" || rc=$?

  if [ "$rc" -ne 2 ]; then
    fail "$name" "inspect exited ${rc}, want 2 (nothing to diagnose)"
    dump_debug "$pod"
    return 0
  fi

  local count
  count="$(printf '%s' "$out" | jq -r '.diagnoses | length' 2>/dev/null || echo "?")"
  if [ "$count" != "0" ]; then
    fail "$name" "exit 2 but diagnoses had ${count} entries, want 0"
    dump_debug "$pod"
    return 0
  fi

  pass "$name" "no diagnoses (exit 2)"
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

summary() {
  local line status name detail
  log ""
  log "======================================================================"
  log "crashcause e2e summary"
  log "======================================================================"
  printf '%-6s %-16s %s\n' "RESULT" "SCENARIO" "DETAIL"
  if [ ${#RESULT_LINES[@]} -eq 0 ]; then
    log "(no scenarios ran)"
    return 1
  fi
  for line in "${RESULT_LINES[@]}"; do
    status="${line%%|*}"
    name="${line#*|}"; name="${name%%|*}"
    detail="${line##*|}"
    printf '%-6s %-16s %s\n' "$status" "$name" "$detail"
  done
  log "======================================================================"

  if [ "$FAILED" -gt 0 ]; then
    log "${FAILED} scenario(s) FAILED"
    return 1
  fi
  log "all ${#RESULT_LINES[@]} scenarios passed"
  return 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
  parse_args "$@"
  preflight
  trap cleanup EXIT

  create_cluster
  check_cluster
  resolve_binary

  apply_manifests
  create_healthy_pod

  local entry name target expected accepted extra
  for entry in "${SCENARIOS[@]}"; do
    IFS='|' read -r name target expected accepted extra <<<"$entry"
    # shellcheck disable=SC2086 # extra is a deliberately word-split flag list
    run_scenario "$name" "$target" "$expected" "$accepted" $extra
  done
  run_healthy_scenario

  summary
}

main "$@"
