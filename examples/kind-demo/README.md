# crashcause kind demo

This directory holds a disposable [kind](https://kind.sigs.k8s.io/) cluster
config plus a set of Kubernetes manifests that each break in exactly one,
unambiguous way — one manifest per crash-cause code in crashcause's
taxonomy (OOM kill, a broken liveness probe, a missing Secret reference, a
bad image tag, an unschedulable resource request, and a stuck init
container). They exist so there is always a real, reproducible broken pod
to point `crashcause inspect` at.

## STATUS

**`crashcause inspect` is not implemented yet.** The project is at
milestone 1 (scaffold only): the binary builds, `inspect` parses its flags,
but `runInspect` just returns `"inspect: not yet implemented"`
(see `internal/cli/inspect.go`). Today, these manifests are useful for:

- seeing the broken pod states directly with `kubectl` (events, container
  statuses, `Init:0/1`, etc.), and
- giving CI's best-effort e2e job real fixtures to stand a kind cluster up
  against.

The `crashcause inspect ...` steps in section 5 below are written for
**milestone 3 and onward**, once classification is wired up. Until then,
running them will just print the "not yet implemented" error — that's
expected, not a bug in these manifests.

## Prerequisites

- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation) —
  not bundled with this repo, install separately.
- [kubectl](https://kubernetes.io/docs/tasks/tools/#kubectl) — not bundled
  with this repo, install separately.
- Go 1.23.1 (matches the `toolchain` line in `go.mod`) — needed to build
  the `crashcause` binary from source; no prebuilt releases exist yet.

## Steps

All commands assume you're in the repository root
(`D:\projects\crashcause`) and use forward-slash paths, which `kubectl`,
`kind`, and Go all accept fine on Windows.

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

### 5. Run the diagnosis on each pod

**These only produce real output from milestone 3 onward.** Right now they
will print `Error: inspect: not yet implemented`. The cause codes below are
what each scenario is *expected* to classify as once `inspect` is
implemented — this is not a preview of actual output.

```sh
./bin/crashcause inspect oom-demo -n crashcause-demo
# expected (once implemented): cause=oom_killed

./bin/crashcause inspect bad-probe-demo -n crashcause-demo
# expected (once implemented): cause=probe_liveness_failure

./bin/crashcause inspect missing-secret-demo -n crashcause-demo
# expected (once implemented): cause=config_missing_reference

./bin/crashcause inspect $(kubectl get pods -n crashcause-demo \
  -l crashcause.dev/scenario=image-pull-not-found \
  -o jsonpath='{.items[0].metadata.name}') -n crashcause-demo
# expected (once implemented): cause=image_pull_not_found

./bin/crashcause inspect unschedulable-demo -n crashcause-demo
# expected (once implemented): cause=unschedulable

./bin/crashcause inspect stuck-init-demo -n crashcause-demo --init-stuck-threshold 30s
# expected (once implemented): cause=init_container_stuck
# (--init-stuck-threshold defaults to 10m; pass a lower value here so the
# demo doesn't require waiting 10 minutes for the default threshold to
# elapse — see the comment in stuck-init.yaml)
```

No fabricated sample output is shown above on purpose — the exact report
format (human/JSON) isn't implemented yet, so there's nothing real to paste.

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
  `--init-stuck-threshold` is 10 minutes, so a *real* diagnosis (once
  `inspect` exists) needs the pod to have actually been stuck that long, or
  you need to pass a lower threshold (e.g. `--init-stuck-threshold 30s`) as
  shown in step 5 to see it classified immediately.
