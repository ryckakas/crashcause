# crashcause kind demo

This directory holds a disposable [kind](https://kind.sigs.k8s.io/) cluster
config plus a set of Kubernetes manifests that each break in exactly one,
unambiguous way — one manifest per crash-cause code in crashcause's
taxonomy (OOM kill, a broken liveness probe, a missing Secret reference, a
bad image tag, an unschedulable resource request, and a stuck init
container). They exist so there is always a real, reproducible broken pod
to point `crashcause inspect` at.

## Prerequisites

- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) —
  not bundled with this repo, install separately.
- [kubectl](https://kubernetes.io/docs/tasks/tools/#kubectl) — not bundled
  with this repo, install separately.
- Go 1.25 (matches the `toolchain` line in `go.mod`) — needed to build
  the `crashcause` binary from source; no prebuilt releases exist yet.
- [jq](https://jqlang.github.io/jq/) — optional, only needed for the
  `--output json | jq ...` steps below and for running `hack/e2e.sh`.

## Steps

All commands assume you're in the repository root and use forward-slash
paths, which `kubectl`, `kind`, and Go all accept fine on Windows.

### 1. Create the cluster

```sh
kind create cluster --config examples/kind-demo/kind-config.yaml
```

This creates a single control-plane-only cluster named `crashcause-demo`
(see the comments in `kind-config.yaml` for why one node is enough). Do
**not** `kubectl apply` this file — it's a kind cluster config, not a
Kubernetes resource.

### 2. Build the binary

```sh
go build -o bin/crashcause ./cmd/crashcause
```

### 3. Apply the manifests

The namespace must exist before the workloads (they set
`metadata.namespace: crashcause-demo` explicitly, so create order across the
*workload* files doesn't matter — but the namespace itself has to come
first). Do **not** run `kubectl apply -f examples/kind-demo/` against the
whole directory — that would also try to apply `kind-config.yaml`
(a kind config, not a valid Kubernetes object) and fail. Instead:

```sh
kubectl apply -f examples/kind-demo/namespace.yaml
kubectl apply \
  -f examples/kind-demo/oom.yaml \
  -f examples/kind-demo/bad-probe.yaml \
  -f examples/kind-demo/missing-secret.yaml \
  -f examples/kind-demo/bad-image.yaml \
  -f examples/kind-demo/unschedulable.yaml \
  -f examples/kind-demo/stuck-init.yaml
```

Optionally, add a healthy control pod so you can see the "nothing wrong
here" case too:

```sh
kubectl run healthy-control -n crashcause-demo --image=registry.k8s.io/pause:3.9 \
  --restart=Always --dry-run=client -o yaml | kubectl apply -f -
```

### 4. Watch the pods break

```sh
kubectl get pods -n crashcause-demo -w
```

What to look for per pod (see the timing table below for how long each
takes):

- `oom-demo` — `RESTARTS` climbs; `kubectl describe pod oom-demo -n
  crashcause-demo` shows `Last State: Terminated, Reason: OOMKilled, Exit
  Code: 137`.
- `bad-probe-demo` — `RESTARTS` climbs; `kubectl get events -n
  crashcause-demo --field-selector involvedObject.name=bad-probe-demo`
  shows repeating `Unhealthy` (probe failed: connection refused) and
  `Killing` events.
- `missing-secret-demo` — `STATUS` shows `CreateContainerConfigError` and
  never changes; `kubectl describe pod missing-secret-demo -n
  crashcause-demo` shows a message naming `does-not-exist-secret`.
- `bad-image-demo-<hash>-<hash>` — this one is a Deployment, so find the
  pod name first with
  `kubectl get pods -n crashcause-demo -l crashcause.dev/scenario=image-pull-not-found`.
  `STATUS` cycles `ErrImagePull` → `ImagePullBackOff`; events show
  "manifest unknown" / "not found" for the bogus tag.
- `unschedulable-demo` — `STATUS` stays `Pending` forever; `kubectl
  describe pod unschedulable-demo -n crashcause-demo` shows a
  `FailedScheduling` event citing "Insufficient cpu". No logs will ever
  exist for this one.
- `stuck-init-demo` — `STATUS` stays `Init:0/1` forever; it is not
  restarting or erroring, just stuck.
- `healthy-control` (if you created it) — `STATUS` goes `Running` and
  stays there; nothing to diagnose.

### 5. Run the diagnosis on each pod

`crashcause inspect` exits `0` when it produced a diagnosis, `2` when the
pod is healthy (nothing to diagnose), and `1` on error. Each command below
works either as a plain human-readable report or, with `--output json`,
as a JSON document shaped like:

```json
{
  "pod": "...", "namespace": "...",
  "diagnoses": [
    {
      "cause": "...", "confidence": "...", "explanation": "...",
      "evidence": [...], "next_steps": [...],
      "container": "...", "pod": "...", "namespace": "...",
      "owner": {...}, "timestamp": "...", "ai_summary": null
    }
  ]
}
```

Without `--verbose`, `diagnoses` has exactly one entry per diagnosed
container (the primary rule match); `--verbose` adds every secondary rule
match too. On a healthy pod, `diagnoses` is `[]` and the exit code is `2`.

```sh
./bin/crashcause inspect oom-demo -n crashcause-demo
./bin/crashcause inspect oom-demo -n crashcause-demo --output json | jq -r '.diagnoses[].cause'
# cause=oom_killed

./bin/crashcause inspect bad-probe-demo -n crashcause-demo
./bin/crashcause inspect bad-probe-demo -n crashcause-demo --output json | jq -r '.diagnoses[].cause'
# cause=probe_liveness_failure

./bin/crashcause inspect missing-secret-demo -n crashcause-demo
./bin/crashcause inspect missing-secret-demo -n crashcause-demo --output json | jq -r '.diagnoses[].cause'
# cause=config_missing_reference

POD=$(kubectl get pods -n crashcause-demo \
  -l crashcause.dev/scenario=image-pull-not-found \
  -o jsonpath='{.items[0].metadata.name}')
./bin/crashcause inspect "$POD" -n crashcause-demo
./bin/crashcause inspect "$POD" -n crashcause-demo --output json | jq -r '.diagnoses[].cause'
# cause=image_pull_not_found

./bin/crashcause inspect unschedulable-demo -n crashcause-demo
./bin/crashcause inspect unschedulable-demo -n crashcause-demo --output json | jq -r '.diagnoses[].cause'
# cause=unschedulable

./bin/crashcause inspect stuck-init-demo -n crashcause-demo --init-stuck-threshold 30s
./bin/crashcause inspect stuck-init-demo -n crashcause-demo --init-stuck-threshold 30s --output json | jq -r '.diagnoses[].cause'
# cause=init_container_stuck
# (--init-stuck-threshold defaults to 10m; pass a lower value here so the
# demo doesn't require waiting 10 minutes for the default threshold to
# elapse — see the comment in stuck-init.yaml)

./bin/crashcause inspect healthy-control -n crashcause-demo --output json | jq -r '.diagnoses'
# [] and the process exits 2 — nothing to diagnose
```

### 6. Cleanup

```sh
kubectl delete ns crashcause-demo
kind delete cluster --name crashcause-demo
```

## Manifest → cause code mapping

| Manifest              | Expected cause code          | What makes it fail                                                                 |
|------------------------|-------------------------------|--------------------------------------------------------------------------------------|
| `oom.yaml`             | `oom_killed`                  | 16Mi memory limit vs. `stress --vm 1 --vm-bytes 128M`; cgroup OOM killer kills it   |
| `bad-probe.yaml`       | `probe_liveness_failure`      | Liveness `httpGet` targets port 9999, which nothing listens on                     |
| `missing-secret.yaml`  | `config_missing_reference`    | `envFrom.secretRef` names a Secret (`does-not-exist-secret`) that is never created  |
| `bad-image.yaml`       | `image_pull_not_found`        | Image tag `busybox:this-tag-does-not-exist-crashcause-demo` doesn't exist on Hub    |
| `unschedulable.yaml`   | `unschedulable`                | Requests `cpu: "1000"`, more than any node has                                      |
| `stuck-init.yaml`      | `init_container_stuck`        | Init container runs `sleep 3600` and never exits, so `Init:0/1` never advances      |
| (script-created)       | none — exit code `2`          | `healthy-control`, a plain `registry.k8s.io/pause` pod that never breaks           |

**A note on `bad-image.yaml` event ordering:** a real kubelet emits three
`Failed` events for a bad image tag — `Failed to pull image "...": ... not
found` (the informative one), `Error: ErrImagePull`, and `Error:
ImagePullBackOff` — and Kubernetes event timestamps only have one-second
granularity, so their order is arbitrary. crashcause therefore selects the
most informative pull-failure message rather than the first one, so this
scenario classifies as `image_pull_not_found` regardless of how the events
happen to sort. `hack/e2e.sh` asserts that exactly.

## Timing notes

- `oom.yaml` — near-instant. `stress` allocates memory immediately; expect
  the first OOMKilled restart within a few seconds of pod start.
- `bad-probe.yaml` — ~15s to the first kill
  (`initialDelaySeconds: 5` + `failureThreshold: 2` × `periodSeconds: 5`),
  then it repeats.
- `missing-secret.yaml` — near-instant; kubelet fails config resolution
  before ever attempting to start the container, so it lands in
  `CreateContainerConfigError` within a couple of seconds and stays there
  (no backoff needed since the container process never runs).
- `bad-image.yaml` — the first `ErrImagePull` shows up within seconds, but
  `ImagePullBackOff` uses an exponential backoff (roughly 10s, 20s, 40s,
  ... capped at 5 minutes), so give it a minute or two to settle into a
  steady `ImagePullBackOff` state.
- `unschedulable.yaml` — near-instant `Pending` + `FailedScheduling`; it
  never resolves on its own (there's no node it could ever fit on), so
  there's no need to wait longer.
- `stuck-init.yaml` — near-instant `Init:0/1`, but crashcause's default
  `--init-stuck-threshold` is 10 minutes, so a *real* diagnosis needs the
  pod to have actually been stuck that long, or you need to pass a lower
  threshold (e.g. `--init-stuck-threshold 30s`) as shown in step 5 to see
  it classified immediately.

## Everything above, automated

`hack/e2e.sh` runs this entire walkthrough end to end: it applies every
manifest in this directory (skipping `kind-config.yaml`), creates the
`healthy-control` pod itself, bounded-polls each pod for its expected
diagnosable state (no blind sleeps), runs `crashcause inspect --output
json` against each one, and asserts both the expected `cause` code and the
expected exit code — including the `healthy-control` exit-2, empty-list
case.

One-shot local run, including cluster creation and teardown:

```sh
./hack/e2e.sh --create-cluster
```

or, equivalently:

```sh
make e2e
```

Pass `--keep-cluster` to leave the kind cluster running afterwards for
manual poking. If you already have a cluster and just want to run the
scenarios against the current `kubectl` context, drop `--create-cluster`
— this is how CI's `e2e-kind` job invokes it, since that job creates the
kind cluster separately via the kind GitHub Action first.

Useful env knobs:

- `CRASHCAUSE_BIN` — path to a prebuilt `crashcause` binary; if unset, the
  script builds `bin/crashcause` itself.
- `E2E_TIMEOUT` — per-scenario readiness wait, in seconds (default `120`).
- `E2E_INIT_STUCK_THRESHOLD` — threshold passed to the stuck-init check
  (default `30s`).

CI runs the same script in the `e2e-kind` job on a best-effort basis
(`continue-on-error: true`) — a failure there is a signal, not a merge
blocker.
