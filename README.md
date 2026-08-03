# crashcause

Answers "why did this pod crash?" from live cluster state — no copy-pasting `describe`
or `logs` output anywhere.

[![CI](https://github.com/ryckakas/crashcause/actions/workflows/ci.yml/badge.svg)](https://github.com/ryckakas/crashcause/actions/workflows/ci.yml)
![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue)

> **Status: pre-1.0.** CLI flags and chart values may still change between minor
> versions; the 18 cause codes are the stable part of the contract. The tool has not yet
> been exercised against real production clusters at scale. Not yet in the
> [krew index](https://github.com/kubernetes-sigs/krew-index), so
> `kubectl krew install crashcause` does not work yet — install from the released plugin
> manifest as shown under [Install](#install). See [`CHANGELOG.md`](./CHANGELOG.md) for
> what shipped.

## What it is, and why

Diagnosing a crashing pod today is a manual ritual: `kubectl describe`, `kubectl logs
--previous`, scrolling through events, and eyeballing metrics to correlate a restart with
an OOM kill or a failed probe. `crashcause` automates that correlation and turns the
*cause* of a crash — not just the fact that one happened — into a first-class, queryable
signal.

Two things differentiate it from "paste your logs into a UI" tools:

1. **Zero copy-paste, live-cluster.** It talks to the Kubernetes API directly: pod status,
   container states, events, previous-container logs, and node conditions. You point it at
   a pod (or a cluster, for `watch`); it collects the evidence itself.
2. **Native observability integration.** Crash *causes* — `oom_killed`,
   `probe_liveness_failure`, `config_missing_reference`, and so on — become a Prometheus
   metric label and a structured log field, not just prose in a report. Where
   `kube-state-metrics` tells you a pod is in `CrashLoopBackOff`, `crashcause` tells you
   *why*, as a queryable dimension: `oom_killed` vs. `probe_liveness_failure` vs.
   `config_missing_reference`, filterable and alertable in Grafana with no extra tooling.

## Cause codes

Every diagnosis carries one of 18 stable, snake_case cause codes
(`internal/engine/types.go`). They are part of the tool's external contract: once
released, a code's meaning does not change.

<details>
<summary><b>All 18 cause codes (the stable contract)</b></summary>

| Cause code | Meaning |
|---|---|
| `oom_killed` | Container's last termination reason was exactly `OOMKilled` (kernel cgroup attribution). Never inferred from a bare exit 137 — usage data isn't recoverable after the fact from the Kubernetes API. |
| `sigkill_unattributed` | Exit code 137 (SIGKILL) **without** `reason=OOMKilled` and **without** the pod being deleted. Suspects: a node-level OOM kill that wasn't cgroup-attributed, or an external SIGKILL. |
| `evicted` | Pod status reason is `Evicted`; the eviction message is parsed for the pressured resource (memory/disk/pids). |
| `probe_liveness_failure` | A `Killing` event plus `Unhealthy` (liveness) events — the liveness probe killed the container. |
| `probe_startup_failure` | `Unhealthy` (startup) events plus a restart; flags a too-tight `failureThreshold` x `periodSeconds` against observed startup time when derivable. |
| `image_pull_auth` | `ErrImagePull`/`ImagePullBackOff` with a message matching authorization failure (401/403, "pull access denied"). |
| `image_pull_not_found` | Image pull failure with a message matching "not found" / "manifest unknown". |
| `image_pull_other` | Any other pull failure: timeout, TLS error, registry quota, etc. |
| `security_context_violation` | `CreateContainerConfigError` where the kubelet message shows a securityContext rejection: `runAsNonRoot: true` against an image that runs as root (or declares a non-numeric user). The image pulled fine — the container was refused by a pre-start config check and never started. |
| `config_missing_reference` | `CreateContainerConfigError` — a referenced ConfigMap/Secret/key doesn't exist; the message identifies which one. securityContext rejections are reported as `security_context_violation`, not this code. |
| `volume_mount_failure` | `FailedMount` / `FailedAttachVolume` events. |
| `init_container_failure` | An init container terminated non-zero; the failure is re-classified using only init-applicable rules. |
| `init_container_stuck` | An init container has been `Running` for longer than `--init-stuck-threshold` (default 10m) without completing, or `activeDeadlineSeconds` was exceeded. Not a crash — a pod stuck at `Init:N/M`. |
| `unschedulable` | Pod is `Pending` with `FailedScheduling` events; the message is parsed to distinguish insufficient resources, node-affinity mismatch, untolerated taints, or volume zone conflicts. Requires no logs and no extra RBAC. |
| `app_exit_nonzero` | A clean application-level crash: exit code 1, 2, or another non-zero code with no Kubernetes-side cause. Log-tail patterns (`panic:`, `Fatal`, `ECONNREFUSED`, `OutOfMemoryError`, `MODULE_NOT_FOUND`, segfault/exit 139, etc.) refine the explanation. This is the primary input to the optional AI layer. |
| `sigkill_after_grace` | Exit 137 **on a pod with a `deletionTimestamp` set** — the app did not stop on `SIGTERM` before its termination grace period expired. Application-only, high confidence. |
| `completed_restart_loop` | Exit code 0 with `restartPolicy: Always` on what looks like a run-to-completion workload — it keeps "succeeding" and restarting forever. |
| `unknown` | Nothing matched confidently. Evidence collected so far is still reported; the AI layer (if enabled) is the suggested next step. |

Two related behaviors worth calling out explicitly because they are easy to get wrong by
hand: exit code **143 during a rolling deploy, scale-down, or deletion is normal pod
lifecycle** (the pod has a `deletionTimestamp`, or its owner is mid rolling-update) and is
deliberately **not reported at all** — otherwise every deploy would look like a stream of
crash findings. Exit 143 *outside* any deletion context (something inside the container
sent itself a `SIGTERM`) is folded into `app_exit_nonzero` evidence at low confidence
instead of being invented as its own cause.

</details>

## 5-minute demo

The full walkthrough — with a `kind` cluster config and one manifest per cause code
(OOM kill, bad liveness probe, missing Secret reference, bad image tag, an unschedulable
resource request, and a stuck init container) — lives in
[`examples/kind-demo/README.md`](./examples/kind-demo/README.md). Headline commands:

```sh
# 1. stand up a disposable cluster
kind create cluster --config examples/kind-demo/kind-config.yaml

# 2. build the binary
go build -o bin/crashcause ./cmd/crashcause

# 3. break some pods on purpose
kubectl apply -f examples/kind-demo/namespace.yaml
kubectl apply -f examples/kind-demo/oom.yaml -f examples/kind-demo/bad-probe.yaml \
  -f examples/kind-demo/missing-secret.yaml -f examples/kind-demo/bad-image.yaml \
  -f examples/kind-demo/unschedulable.yaml -f examples/kind-demo/stuck-init.yaml

# 4. ask crashcause why
./bin/crashcause inspect oom-demo -n crashcause-demo
```

See the demo README for the full manifest-to-cause-code mapping, timing notes for each
scenario, and cleanup instructions.

## Install

### As a kubectl plugin (for `inspect`)

crashcause is not in the krew index yet, so `kubectl krew install crashcause` does not
work. Until the index entry lands, install from the plugin manifest published with each
release — the `latest` URL below always resolves to the newest release's manifest, which
points at that release's archives and verifies their sha256. (`--manifest-url` is krew's
development-only install path; it works fine here, it just isn't how a published plugin
is normally fetched.)

```sh
kubectl krew install --manifest-url=https://github.com/ryckakas/crashcause/releases/latest/download/crashcause.yaml
```

Prebuilt `linux`/`darwin` `amd64`/`arm64` archives (plus `checksums.txt` and SBOMs) are
attached to every release at
[github.com/ryckakas/crashcause/releases](https://github.com/ryckakas/crashcause/releases)
if you would rather drop the binary on your `PATH` yourself — name it `kubectl-crashcause`
to get the `kubectl crashcause` subcommand form without krew.

Or build/install from source with Go 1.25+ (any Go ≥ 1.21 also works — the
`go` command auto-downloads the toolchain pinned in `go.mod`):

```sh
go install github.com/ryckakas/crashcause/cmd/crashcause@latest

# or, from a checkout:
go build -o bin/crashcause ./cmd/crashcause
```

### In-cluster `watch` mode (Helm)

The chart is published as an OCI artifact to GitHub Container Registry:

```sh
helm install crashcause oci://ghcr.io/ryckakas/charts/crashcause \
  -n crashcause --create-namespace \
  -f my-values.yaml
```

Helm resolves the newest published chart version; add `--version X.Y.Z` to pin a
specific release. `helm show values oci://ghcr.io/ryckakas/charts/crashcause` prints the
full default values, and [`charts/crashcause/README.md`](./charts/crashcause/README.md)
is the full values reference.

<details>
<summary><b>Example values files and notable keys</b></summary>

To install from a checkout instead — for local development, or to try chart changes
before they are released:

```sh
helm install crashcause ./charts/crashcause -n crashcause --create-namespace \
  -f my-values.yaml
```

A minimal values file, watching everything with the default sinks:

```yaml
# my-values.yaml
watch:
  namespaces: []          # empty = all namespaces
  reemitInterval: 1h
  dedupTTL: 6h

logCollection:
  enabled: true
  rateLimitPerMinute: 10

metrics:
  enabled: true
  addr: ":9090"
```

A locked-down values file for a namespace you don't want the controller reading logs
from at all — note that `logCollection.enabled: false` removes the `pods/log` RBAC verb
from the chart's ClusterRole entirely, it does not just leave it configured off:

```yaml
# my-values.locked-down.yaml
watch:
  namespaces: ["payments", "checkout"]
  selector: "team=platform"

logCollection:
  enabled: false          # controller CANNOT read pod logs; RBAC rule is omitted

ai:
  enabled: false          # no AI layer for this namespace scope

metrics:
  enabled: true
  addr: ":9090"

serviceMonitor:
  enabled: false
```

Notable `values.yaml` keys: `watch.namespaces`, `watch.selector`, `watch.reemitInterval`,
`watch.dedupTTL`, `watch.previousLines`, `watch.initStuckThreshold`,
`logCollection.enabled`, `logCollection.namespaces`, `logCollection.rateLimitPerMinute`,
`metrics.enabled`, `metrics.addr`, `serviceMonitor.enabled`, `serviceMonitor.labels`,
`serviceMonitor.skipCapabilityCheck`, `loki.url`, `loki.existingSecret`, `ai.enabled`,
`ai.provider`, `ai.url`, `ai.namespaces` (empty = nothing summarized, `["*"]` = whole
cluster — see [AI layer](#ai-layer-optional-default-off) below), `ai.redact`,
`ai.redactIPs`, `ai.existingSecret`, `ai.secretKey`, `leaderElection.enabled`,
`replicas`. `serviceMonitor.skipCapabilityCheck` (default `false`) lets `helm template`
render the `ServiceMonitor` without a live cluster connection — useful for GitOps
pipelines that template offline, where the chart can't check whether the
`monitoring.coreos.com/v1` CRD is actually installed; without it (or a real CRD check
passing), enabling `serviceMonitor` against an offline template run fails loudly rather
than rendering a resource the cluster can't accept. See
[`charts/crashcause/README.md`](./charts/crashcause/README.md) for the full reference,
and the [AI layer](#ai-layer-optional-default-off) and
[Security & RBAC](#security--rbac) sections below for what those keys actually control.

</details>

## `inspect` usage

```sh
crashcause inspect <pod> [flags]
```

Exit codes: `0` diagnosed, `2` pod exists but isn't crashing (nothing to diagnose), `1`
error. See the [Exit codes](#exit-codes) section for the full table.

<details>
<summary><b><code>inspect</code> flags</b></summary>

| Flag | Default | Meaning |
|---|---|---|
| `-c, --container` | (unset) | Restrict the diagnosis to one container. By default every container that warrants diagnosis is reported. |
| `--output` | `human` | `human` or `json`. |
| `--previous-lines` | `60` | Lines to fetch from the previous container's log tail. |
| `--init-stuck-threshold` | `10m` | How long an init container may run before it's reported as `init_container_stuck`. |
| `--verbose` | `false` | Report every rule that matched, not just the primary diagnosis. |
| `--ai` | `false` | Enable the optional AI summary (see [AI layer](#ai-layer-optional-default-off)). |
| `-n, --namespace`, `--kubeconfig`, `--context`, ... | — | Standard `k8s.io/cli-runtime` kubeconfig flags (krew-compatible). |
| `--log-level` (root, persistent) | `info` | `debug\|info\|warn\|error`. |

</details>

The human report is compact and evidence-first: cause, confidence, a plain-language
explanation, evidence bullets, and suggested next steps. **The output below is an
illustration of the report format, not a transcript of a real run:**

```text
crashcause-demo/oom-demo container app — oom_killed (high confidence)

  Container was OOM-killed: last termination reason was OOMKilled with exit code 137.
  Memory limit was 16Mi.

  Evidence:
    - last state: terminated, reason=OOMKilled, exitCode=137
    - restartCount: 4
    - limit: memory=16Mi

  Next steps:
    - Raise the memory limit or reduce the workload's memory footprint
    - Check node MemoryPressure conditions if this recurs across pods
```

<details>
<summary><b><code>--output json</code> shape</b></summary>

`--output json` emits one `Report` document per invocation, with the field names as they
appear in `internal/engine/types.go`'s `Diagnosis`:

```json
{
  "pod": "oom-demo",
  "namespace": "crashcause-demo",
  "diagnoses": [
    {
      "cause": "oom_killed",
      "confidence": "high",
      "explanation": "Container was OOM-killed: last termination reason was OOMKilled with exit code 137. Memory limit was 16Mi.",
      "evidence": [
        "last state: terminated, reason=OOMKilled, exitCode=137",
        "restartCount: 4",
        "limit: memory=16Mi"
      ],
      "next_steps": [
        "Raise the memory limit or reduce the workload's memory footprint",
        "Check node MemoryPressure conditions if this recurs across pods"
      ],
      "container": "app",
      "pod": "oom-demo",
      "namespace": "crashcause-demo",
      "owner": {"kind": "Deployment", "name": "oom-demo"},
      "timestamp": "2026-08-02T12:00:00Z",
      "ai_summary": null
    }
  ]
}
```

`ai_summary` is always present in the JSON shape (nullable) and is only non-null when
`--ai` was passed and the provider call succeeded. A healthy pod (`exit 2`) still emits a
well-formed document with `"diagnoses": []` in JSON mode, so `jq '.diagnoses | length'`
never breaks on a healthy pod.

</details>

## `watch` mode + sinks

```sh
crashcause watch [flags]
```

Every diagnosis goes to stdout as one JSON object per line — always on, so any team
already scraping container stdout (promtail, vector, fluent-bit) gets structured
crash-cause data with no direct-push configuration at all. `--metrics-addr` adds a
Prometheus endpoint with exactly two intentionally low-cardinality metrics
(`crashcause_diagnoses_total{namespace, owner_kind, owner_name, cause}` and
`crashcause_log_fetches_skipped_total`), and `--loki-url` adds a direct Loki push.
Emission for the log-style sinks is deduplicated per workload; the Prometheus counter
increments on every observed crash regardless. The full sink reference — cardinality
discipline, Loki auth and backpressure, and the dedup model — lives in
[`docs/sinks.md`](./docs/sinks.md). Example Grafana dashboard and Prometheus alert
rules: [`examples/grafana-dashboard.json`](./examples/grafana-dashboard.json),
[`examples/prometheus-alerts.yaml`](./examples/prometheus-alerts.yaml).

<details>
<summary><b><code>watch</code> flags</b></summary>

| Flag | Default | Meaning |
|---|---|---|
| `--namespaces` | (all) | Namespaces to watch (allowlist). |
| `--selector` | (unset) | Label selector to filter watched pods. |
| `--metrics-addr` | (disabled) | Address to serve `/metrics` on, e.g. `:9090`. Enables the Prometheus sink. |
| `--loki-url` | (disabled) | Loki push URL. Enables the Loki sink. |
| `--reemit-interval` | `1h` | Minimum interval before re-emitting an unchanged diagnosis for the same container. |
| `--dedup-ttl` | `6h` | How long a diagnosis is remembered for dedup purposes. |
| `--collect-logs` | `true` | Whether to fetch previous-container logs at all. |
| `--log-namespaces` | (all) | Namespaces log collection is allowed in. |
| `--log-rate-limit` | `10` | Max pod-log fetches per minute (client-side token bucket). |
| `--previous-lines` | `60` | Log tail length passed to the engine and (if enabled) the AI layer. |
| `--init-stuck-threshold` | `10m` | Same meaning as in `inspect`. |
| `--leader-elect` | `false` | Enable leader election so only one replica is active. |
| `--leader-election-namespace` | `$POD_NAMESPACE`, else `default` | Namespace holding the leader-election Lease. |
| `--leader-election-id` | `crashcause` | Name of the leader-election Lease. |
| `--health-addr` | `:8081` | Address serving `/healthz` and `/readyz`. |
| `--ai-namespaces` | (empty: AI off everywhere) | Namespace allowlist for AI summarization. Empty keeps AI inert in every namespace, even with `--ai` set; pass `"*"` to opt the whole cluster in. |
| `--ai`, `--ai-provider`, `--ai-url`, `--ai-model`, `--ai-timeout`, `--ai-redact`, `--ai-redact-ips`, `--ai-redact-extra` | see [`docs/ai-layer.md`](./docs/ai-layer.md) | Shared AI flags, identical to `inspect`. |

</details>

## AI layer (optional, default off)

`--ai` / `ai.enabled` turn on an optional layer that sends the (already-truncated) log
tail and diagnosis evidence to an LLM for a short natural-language summary — useful
mainly when the rule engine lands on `app_exit_nonzero` or `unknown`, where the "cause"
is an application bug the rules can't interpret further. Three properties are
load-bearing: it is **off by default**; the per-namespace allowlist (`--ai-namespaces` /
`ai.namespaces`) is **fail-closed** — with `--ai` set but the allowlist empty, nothing is
sent anywhere; and redaction (`--ai-redact`, default on) is **best-effort pattern
matching, not a guarantee** — do not enable AI on workloads whose logs may contain
secrets you cannot afford to send to a third-party provider. Providers are `anthropic`,
`openai`, and keyless self-hosted `ollama` (the recommended path for privacy-sensitive
environments). Key handling, exactly what is and is never sent, redaction coverage, and
failure behavior: [`docs/ai-layer.md`](./docs/ai-layer.md).

## Security & RBAC

Every privilege `crashcause` asks for is individually severable — you can turn each one
off and see, precisely, what capability you lose. `crashcause inspect` needs none of
this: it runs with the invoking user's own kubeconfig credentials against a single pod,
so the RBAC discussion applies only to `watch`.

<details>
<summary><b>Permission-by-permission table</b></summary>

| Permission | Why it's needed | How to remove it / what degrades |
|---|---|---|
| `pods` get/list/watch | Core input: container statuses, restart counts, owner metadata. | Not removable without disabling `watch` entirely — this is the minimum the controller needs to exist. |
| `events` get/list/watch | Probe failures, scheduling failures, image pull failures, volume mount failures all surface as events. | Same as above — required for most cause codes. |
| `nodes` get/list/watch | Node conditions (`MemoryPressure`, `DiskPressure`) as context for `evicted`/`sigkill_unattributed`. | Not independently toggleable today; absence just means node-condition evidence is empty. |
| `pods/log` get | Previous-container log tail, used by log-pattern hints and the AI layer. | Set `logCollection.enabled=false`. The chart **omits the RBAC rule entirely** in that case — the controller provably cannot read logs, not merely configured not to. `app_exit_nonzero` still fires from exit codes and events; it just loses log-pattern hints, and the AI layer becomes inert (nothing to send). |
| `coordination.k8s.io` `leases` create/get/update | Leader election, so only one replica is active when running more than one. | Only requested when `leaderElection.enabled=true` (required if `replicas > 1`); otherwise the Role isn't created at all. Granted as a **namespaced Role**, not another cluster-wide grant: the chart sets `POD_NAMESPACE` via the downward API and passes `--leader-election-id=<release fullname>`, so the Lease always lands in the release's own namespace. |

Nothing in the RBAC surface is a write verb against workload state, and nothing is
secret-shaped: the chart never creates or reads arbitrary Secrets — the one Secret
reference it accepts (`ai.existingSecret`) is a name you provide, mounted as an
environment variable, never inspected or logged by the chart itself.

Pod-log fetches in `watch` mode are additionally protected by a client-side token-bucket
rate limiter (`--log-rate-limit`, default 10/minute) so a crash-storm across many pods
cannot turn the controller into an unintentional API-server DoS. Fetches beyond budget are
skipped, counted in `crashcause_log_fetches_skipped_total`, and diagnosis proceeds without
log evidence rather than blocking or failing.

</details>

Full permission-by-permission reference: [`charts/crashcause/README.md`](./charts/crashcause/README.md).

## Exit codes

| Code | Meaning | Applies to |
|---|---|---|
| `0` | Diagnosed: at least one container's crash was classified and reported. | `inspect` |
| `1` | Error: bad flags, unreachable API server, pod/container not found, and similar failures. | `inspect` |
| `2` | Nothing to diagnose: the pod exists and was inspected successfully, but it isn't crashing. | `inspect` |

## Roadmap / not in v1

Deliberately out of scope for the first release:

- Cloud-provider enrichment (GKE/EKS/AKS-specific signals, cloud monitoring correlation)
- Stuck-state forensics beyond `unschedulable` / `init_container_stuck`: Terminating-forever
  / finalizer analysis, `ContainerCreating` stuck for reasons other than a volume mount
- Auto-remediation of any kind — this tool diagnoses, it never acts on a cluster
- Historical storage or a database — the sinks (stdout/Prometheus/Loki) *are* the storage;
  `crashcause` itself is stateless
- Multi-cluster federation
- A web UI

## Development

Prerequisites to build: **Go 1.25** (pinned as `toolchain go1.25.0` in
`go.mod`; any Go ≥ 1.21 auto-downloads it).

Prerequisites for the full check loop (not required just to build): **golangci-lint v2**,
**helm**, **kind**.

```sh
make fmt lint test race cover build e2e helm-lint check
```

`make check` mirrors what CI runs — if it's green locally, CI should be green too.

## License

Apache-2.0. See [LICENSE](./LICENSE).
