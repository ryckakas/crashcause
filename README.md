# crashcause

Answers "why did this pod crash?" from live cluster state — no copy-pasting `describe`
or `logs` output anywhere.

[![CI](https://github.com/ryckakas/crashcause/actions/workflows/ci.yml/badge.svg)](https://github.com/ryckakas/crashcause/actions/workflows/ci.yml)
![Go 1.23+](https://img.shields.io/badge/go-1.23%2B-00ADD8)
![License: Apache 2.0](https://img.shields.io/badge/license-Apache--2.0-blue)

> **Status: feature-complete for v0.1.0, pre-first-release.**
>
> - `crashcause inspect` and `crashcause watch` are both implemented end-to-end and
>   unit-tested under `go test -race`: the full rule engine, human and `--output json`
>   report formats, workload-keyed dedup, the stdout/Prometheus/Loki sinks, optional
>   leader election, and the opt-in (off-by-default) AI layer.
> - The Helm chart at `charts/crashcause/` is complete: Deployment, RBAC, Service,
>   ServiceAccount, ServiceMonitor, NOTES.txt, and a chart README.
> - A kind-based e2e harness (`hack/e2e.sh`) exercises every demo scenario in
>   `examples/kind-demo/` against a live cluster and runs best-effort (non-gating) in CI.
> - There is no GitHub Release yet, so there is no krew-index entry, no downloadable
>   binary, and no chart repository to `helm repo add`. Build from source, or install the
>   kubectl plugin / Helm chart from a local checkout of this repository. The tool has not
>   yet been exercised against real production clusters.
>
> Track progress via the milestones in [`CHANGELOG.md`](./CHANGELOG.md) and the CI badge
> above.

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

Every diagnosis carries one of these 17 stable, snake_case cause codes (`internal/engine/types.go`).
They are part of the tool's external contract: once released, a code's meaning does not
change.

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
| `config_missing_reference` | `CreateContainerConfigError` — a referenced ConfigMap/Secret/key doesn't exist; the message identifies which one. |
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

No release has been published yet, so the krew-index path (`kubectl krew install
crashcause`) is not available. Until then:

```sh
# from a local checkout of this repository
kubectl krew install --manifest=deploy/krew/crashcause.yaml
```

Once a GitHub Release exists, prebuilt binaries will also be attached to it directly
(`https://github.com/ryckakas/crashcause/releases`) for manual download, and the
`deploy/krew/crashcause.yaml` manifest will point at those release archives instead of
placeholders.

Or build/install from source with Go 1.23+:

```sh
go install github.com/ryckakas/crashcause/cmd/crashcause@latest

# or, from a checkout:
go build -o bin/crashcause ./cmd/crashcause
```

### In-cluster `watch` mode (Helm)

There is no published chart repository yet, so install from the checkout:

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
cluster — see "AI layer" below), `ai.redact`, `ai.redactIPs`, `ai.existingSecret`,
`ai.secretKey`, `leaderElection.enabled`, `replicas`. `serviceMonitor.skipCapabilityCheck`
(default `false`) lets `helm template` render the `ServiceMonitor` without a live cluster
connection — useful for GitOps pipelines that template offline, where the chart can't
check whether the `monitoring.coreos.com/v1` CRD is actually installed; without it (or a
real CRD check passing), enabling `serviceMonitor` against an offline template run fails
loudly rather than rendering a resource the cluster can't accept. See
[`charts/crashcause/README.md`](./charts/crashcause/README.md) for the full reference, and
the "AI layer" and "Security & RBAC" sections below for what those keys actually control.

## `inspect` usage

```sh
crashcause inspect <pod> [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `-c, --container` | (unset) | Restrict the diagnosis to one container. By default every container that warrants diagnosis is reported. |
| `--output` | `human` | `human` or `json`. |
| `--previous-lines` | `60` | Lines to fetch from the previous container's log tail. |
| `--init-stuck-threshold` | `10m` | How long an init container may run before it's reported as `init_container_stuck`. |
| `--verbose` | `false` | Report every rule that matched, not just the primary diagnosis. |
| `--ai` | `false` | Enable the optional AI summary (see below). |
| `-n, --namespace`, `--kubeconfig`, `--context`, ... | — | Standard `k8s.io/cli-runtime` kubeconfig flags (krew-compatible). |
| `--log-level` (root, persistent) | `info` | `debug\|info\|warn\|error`. |

Exit codes: `0` diagnosed, `2` pod exists but isn't crashing (nothing to diagnose), `1`
error. See the [Exit codes](#exit-codes) section for the full table.

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

## `watch` mode + sinks

> As noted in the status block above, `crashcause watch` is not yet wired up to a
> running command. This section documents the target flags and sink behavior.

```sh
crashcause watch [flags]
```

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
| `--ai`, `--ai-provider`, `--ai-url`, `--ai-redact`, `--ai-redact-ips`, `--ai-redact-extra` | see below | Shared AI flags, identical to `inspect`. |

### stdout (default, always on)

One JSON object per line. This sink alone is worth documenting loudly: any team already
scraping container stdout — promtail, vector, or fluent-bit shipping to Loki, CloudWatch,
or anything else — gets structured, Loki-shaped crash-cause data for free, with **no
direct-push configuration at all**. If you already have a log pipeline, you may not need
`--loki-url` or a Prometheus scrape target to get value out of `watch`.

### Prometheus (`--metrics-addr :9090`)

Exactly two metrics, both intentionally low-cardinality:

| Metric | Type | Labels |
|---|---|---|
| `crashcause_diagnoses_total` | counter | `namespace`, `owner_kind`, `owner_name`, `cause` |
| `crashcause_log_fetches_skipped_total` | counter | (none) |

Cardinality discipline: label values come only from the fixed `CauseCode` set plus a
normalized workload identity. Never a pod name, never explanation text, never AI output.
`confidence` is deliberately **not** a label (a confidence shift for the same cause would
otherwise split one logical series in two) — it lives only in the stdout/Loki payloads.
`crashcause_diagnoses_total` increments on **every** observed crash, independent of the
dedup model below; series for a workload are dropped from the registry when its dedup key
expires or the workload is deleted, so `/metrics` stays bounded over weeks of churn.

Example Grafana dashboard and Prometheus alert rules: [`examples/grafana-dashboard.json`](./examples/grafana-dashboard.json),
[`examples/prometheus-alerts.yaml`](./examples/prometheus-alerts.yaml).

### Loki push (`--loki-url ...`)

Pushes the full `Diagnosis` (including `ai_summary`, if any) as one JSON log line. Stream
labels are `{app="crashcause", namespace, cause}` only — same cardinality discipline as
Prometheus. Auth via `LOKI_USERNAME`/`LOKI_PASSWORD` or `LOKI_BEARER_TOKEN` environment
variables. On backpressure, pushes are dropped with a counter rather than blocking the
watch loop — a slow or unavailable Loki must never stall crash classification.

### Dedup model

Emission for the log-style sinks (stdout, Loki) is keyed on `(namespace, owner, container,
cause)` — deliberately **not** pod name and **not** restart count, so a ReplicaSet
replacing pods, or a container stuck in a backoff loop, doesn't spam a log line every few
seconds. A key re-emits on first occurrence, on a cause change for the same workload, and
at most once per `--reemit-interval` while the condition persists. The Prometheus counter
ignores this entirely and increments on every observed crash.

Dedup state is an in-memory cache with TTL eviction (`--dedup-ttl`) — there is no
persistence. After a controller restart, every key re-emits once, since the controller has
no memory of what it already reported. This is accepted, documented behavior, not a bug.

## AI layer (optional, default off)

`--ai` / `ai.enabled` turn on an optional layer that sends the log tail and diagnosis
evidence to an LLM for a short (2-4 sentence) natural-language summary — useful mainly
when the rule engine lands on `app_exit_nonzero` or `unknown`, where the "cause" is an
application bug the rules can't interpret further. It is **BYO-key**: you bring your own
provider API key, the tool ships every provider client, and you implement nothing.

Providers: `anthropic`, `openai`, `ollama` (`--ai-provider`). **`ollama` is the recommended
path for privacy-sensitive environments** — it's keyless and self-hosted, so nothing
leaves your machine or cluster. The API key, when one is needed, comes **only** from the
`CRASHCAUSE_AI_API_KEY` environment variable — never a command-line flag (flags leak via
`ps`), and never logged. In Helm, the key is mounted from a Kubernetes Secret you provide
(`ai.existingSecret` + `ai.secretKey`, default key name `api-key`) into that environment
variable; the chart never creates the Secret and never accepts a plaintext key in
`values.yaml`.

**What is sent** (only if `--ai` is enabled): the already-truncated log tail, the cause's
evidence strings, and the container image name.
**What is never sent**: environment variables, Secrets, the full pod spec, or node info.

> Redaction (`--ai-redact`, default on) is **best-effort pattern matching, not a
> guarantee**. Do not enable AI on workloads whose logs may contain secrets you cannot
> afford to send to a third-party provider. The per-namespace allowlist
> (`--ai-namespaces` / `ai.namespaces`) is the primary control; redaction is
> defense-in-depth, not the safety mechanism itself. This notice is also printed to
> stderr the first time `--ai` is used in a given invocation.

The allowlist is **fail-closed**, which is what makes it the primary control rather than a
convenience filter: with `--ai` set but `--ai-namespaces` left empty (or `ai.namespaces:
[]` in the chart), AI summarization is inert in every namespace — nothing is sent
anywhere. You must explicitly pass `--ai-namespaces "*"` (or set `ai.namespaces: ["*"]`)
to opt the whole cluster in; there is no "on by default once `--ai`/`ai.enabled` is set"
behavior to accidentally trigger.

What redaction covers: bearer/api-key/password/token `key=value` pairs, AWS-style access
key IDs, JWT-shaped strings, and credentials embedded in a URL (`://user:pass@host`
becomes `://***@host` — the password is scrubbed, but **host and port are deliberately
kept**, because they're diagnosis, not secret). What it deliberately does not cover by
default: bare IP addresses — "connection refused to 10.2.3.4:5432" is often *the*
diagnostic fact, so IPs survive unless you opt in with `--ai-redact-ips`.
`--ai-redact-extra <regex-file>` adds your own organization-specific patterns (one regex
per line, `#`-comments allowed). The exact built-in pattern list is published in code
(`internal/ai/redact.go`) rather than kept secret, on the theory that auditability beats
false assurance.

AI failures never fail a diagnosis: on a provider error or timeout you get the rules-only
result plus a single one-line notice on stderr, not a hard failure. The Prometheus sink
ignores AI output entirely — `ai_summary` never becomes a label or influences a metric.

## Security & RBAC

Every privilege `crashcause` asks for is individually severable — you can turn each one
off and see, precisely, what capability you lose.

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

`crashcause inspect` needs none of this: it runs with the invoking user's own kubeconfig
credentials against a single pod, so the RBAC discussion above applies only to `watch`.

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

Prerequisites to build: **Go 1.23.1**.

Prerequisites for the full check loop (not required just to build): **golangci-lint v2**,
**helm**, **kind**.

```sh
make fmt lint test race cover build e2e helm-lint check
```

`make check` mirrors what CI runs — if it's green locally, CI should be green too.

## License

Apache-2.0. See [LICENSE](./LICENSE).
