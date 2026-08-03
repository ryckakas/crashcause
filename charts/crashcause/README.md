# crashcause

Helm chart for **crashcause**, a controller that classifies why pods crash in
a Kubernetes cluster and reports the cause.

This chart deploys exactly one thing: `crashcause watch`, a long-running
controller that watches pods and events across the cluster, classifies each
crash (bad exit code, OOM, failed probe, image pull failure, eviction,
unschedulable, ...), and emits the diagnosis to whichever sinks you enable —
stdout JSON (always on, one object per line), Prometheus metrics, and/or
Loki. It deploys **nothing** for `crashcause inspect`: `inspect` is a
`kubectl` plugin that runs on your workstation with your own kubeconfig
credentials and needs no in-cluster component at all.

## Requirements

- Kubernetes >= 1.23
- Helm >= 3.8
- (Optional) the Prometheus Operator CRDs, only if you enable
  `serviceMonitor.enabled`

## Install / uninstall

There is no published chart repository yet (see
[Known limitations](#known-limitations-in-010)) — install from a checkout of
this repository.

```bash
helm install crashcause ./charts/crashcause -n crashcause --create-namespace
```

Apply your own overrides with a values file:

```bash
helm install crashcause ./charts/crashcause -n crashcause --create-namespace \
  -f my-values.yaml
```

Uninstall:

```bash
helm uninstall crashcause -n crashcause
```

### Example 1: minimal defaults

Watches every namespace, collects previous-container log tails, serves
Prometheus metrics on `:9090`, emits stdout JSON. No Loki, no AI, one replica,
no leader election.

```bash
helm install crashcause ./charts/crashcause -n crashcause --create-namespace
```

### Example 2: metrics + ServiceMonitor for kube-prometheus-stack

Same as above, plus a `ServiceMonitor` labeled so a kube-prometheus-stack
Prometheus picks it up automatically.

```bash
helm install crashcause ./charts/crashcause -n crashcause --create-namespace \
  --set serviceMonitor.enabled=true \
  --set serviceMonitor.labels.release=kube-prometheus-stack
```

### Example 3: locked down (no log reads, limited namespace scope)

Removes the pod-log read rule from the ClusterRole entirely and restricts
what the controller watches to two namespaces.

```bash
helm install crashcause ./charts/crashcause -n crashcause --create-namespace \
  --set logCollection.enabled=false \
  --set "watch.namespaces={team-a,team-b}"
```

Verify the RBAC actually shrank:

```bash
helm template crashcause ./charts/crashcause \
  --set logCollection.enabled=false | grep -c 'pods/log'
# -> 0
```

Note: even in this example the `ClusterRole` for `pods`, `events`, and
`nodes` is still cluster-scoped — see [Known limitations](#known-limitations-in-010)
for why `watch.namespaces` narrows what is *watched*, not what RBAC grants.

## Values

### Image / general

| Key | Type | Default | Description |
|---|---|---|---|
| `nameOverride` | string | `""` | Overrides the chart-name portion of generated resource names. |
| `fullnameOverride` | string | `""` | Overrides the entire generated resource name. |
| `image.repository` | string | `ghcr.io/ryckakas/crashcause` | Container image repository. |
| `image.tag` | string | `""` | Image tag; an empty string falls back to the chart's `appVersion`. |
| `image.pullPolicy` | string | `IfNotPresent` | Image pull policy. |
| `imagePullSecrets` | list | `[]` | Names of existing image pull secrets, e.g. `[{name: my-regcred}]`. |
| `replicas` | int | `1` | Number of controller replicas; keep at `1` unless `leaderElection.enabled=true` (see [Scaling / HA](#scaling--ha)). |
| `serviceAccount.create` | bool | `true` | Whether to create a ServiceAccount for the controller. |
| `serviceAccount.name` | string | `""` | ServiceAccount name to use; an empty string falls back to the generated fullname. |
| `serviceAccount.annotations` | map | `{}` | Extra annotations on the ServiceAccount. |
| `podAnnotations` | map | `{}` | Extra annotations on the pod template. |
| `podLabels` | map | `{}` | Extra labels on the pod template. |
| `logLevel` | string | `info` | Controller log verbosity: `debug`, `info`, `warn`, or `error`. |

### watch

| Key | Type | Default | Description |
|---|---|---|---|
| `watch.namespaces` | list | `[]` | Namespaces the controller watches; empty means all namespaces. Does not change what RBAC grants — see [Known limitations](#known-limitations-in-010). |
| `watch.selector` | string | `""` | Label selector further limiting which pods are watched, e.g. `tier!=batch`. |
| `watch.reemitInterval` | duration | `1h` | Minimum time before an unchanged diagnosis for the same dedup key is logged again; the Prometheus counter still increments on every observed crash regardless. |
| `watch.dedupTTL` | duration | `6h` | How long a dedup key is remembered, bounding controller memory and driving metric series cleanup. |
| `watch.previousLines` | int | `60` | Lines of previous-container log tail collected per crash, when log collection is enabled. |
| `watch.initStuckThreshold` | duration | `10m` | How long an init container may run before it is classified as stuck. |

### logCollection

| Key | Type | Default | Description |
|---|---|---|---|
| `logCollection.enabled` | bool | `true` | Whether the controller may read previous-container logs at all; setting this to `false` both passes `--collect-logs=false` and removes the `pods/log` RBAC rule entirely (see [Security](#security-every-privilege-is-individually-severable)). |
| `logCollection.namespaces` | list | `[]` | Controller-side allowlist of namespaces whose logs may ever be fetched; empty means every watched namespace. The RBAC grant itself remains cluster-wide whenever `enabled=true`. |
| `logCollection.rateLimitPerMinute` | int | `10` | Client-side token-bucket limit on pod-log fetches per minute, so a crashloop storm cannot turn the controller into an API-server DoS. |

### metrics + serviceMonitor

| Key | Type | Default | Description |
|---|---|---|---|
| `metrics.enabled` | bool | `true` | Serves Prometheus metrics and creates the metrics Service; disabling removes both the flag and the Service. |
| `metrics.addr` | string | `:9090` | Listen address for the metrics endpoint; the Service port is derived from it. |
| `metrics.service.type` | string | `ClusterIP` | Service type for the metrics Service. |
| `metrics.service.annotations` | map | `{}` | Extra annotations on the metrics Service. |
| `serviceMonitor.enabled` | bool | `false` | Creates a prometheus-operator `ServiceMonitor`; requires `metrics.enabled=true` and the `monitoring.coreos.com/v1` CRD in the cluster. |
| `serviceMonitor.labels` | map | `{}` | Extra labels on the ServiceMonitor, e.g. `{release: kube-prometheus-stack}` so your Prometheus selects it. |
| `serviceMonitor.interval` | string | `""` | Scrape interval override; empty uses the Prometheus Operator default. |
| `serviceMonitor.scrapeTimeout` | string | `""` | Scrape timeout override; empty uses the Prometheus Operator default. |
| `serviceMonitor.skipCapabilityCheck` | bool | `false` | Renders the ServiceMonitor even when `helm template` cannot detect the CRD (no cluster connection), for offline GitOps rendering. |

The `ServiceMonitor` template only renders when `serviceMonitor.enabled=true`
**and** `metrics.enabled=true` **and** either the `monitoring.coreos.com/v1`
CRD is detected in the target cluster or `skipCapabilityCheck=true`. If
`serviceMonitor.enabled=true` but neither condition is met, the template
calls Helm's `fail` with an explanatory message instead of silently rendering
nothing — worth knowing because `helm template` never has a cluster
connection, so its capability check always fails and `skipCapabilityCheck`
must be set to render offline.

### loki

| Key | Type | Default | Description |
|---|---|---|---|
| `loki.url` | string | `""` | Loki push URL, e.g. `http://loki:3100/loki/api/v1/push`; a base URL (`http://loki:3100`) also works — the `/loki/api/v1/push` path is appended automatically when not already present. Empty disables the Loki sink. |
| `loki.existingSecret` | string | `""` | Name of an existing Secret whose keys are injected verbatim as env vars; see [Secrets](#secrets) for the required key names. |

### ai

| Key | Type | Default | Description |
|---|---|---|---|
| `ai.enabled` | bool | `false` | Enables AI-generated crash summaries; off by default. |
| `ai.provider` | string | `anthropic` | AI provider: `anthropic`, `openai`, or `ollama`. |
| `ai.url` | string | `""` | Overrides the provider base URL, e.g. `http://ollama:11434` for an in-cluster Ollama. |
| `ai.model` | string | `""` | Overrides the model; empty uses the provider default (anthropic: `claude-haiku-4-5`, openai: `gpt-4o-mini`, ollama: `llama3.1`). Useful for pointing ollama at a smaller local model, e.g. `llama3.2:3b`. |
| `ai.timeout` | string | `""` | Per-summary timeout; empty uses the `15s` default, which is ample for a hosted API but often too short for a self-hosted ollama that must load several GB of weights on its first call. E.g. `60s`. |
| `ai.namespaces` | list | `[]` | Fail-closed allowlist of namespaces whose logs may be sent to the AI provider; an **empty list keeps AI summarization off in every namespace, even with `ai.enabled=true`**. Use `["*"]` to explicitly opt the whole cluster in. |
| `ai.redact` | bool | `true` | Enables best-effort redaction of sensitive-looking patterns before sending log content to the provider. |
| `ai.redactIPs` | bool | `false` | Additionally redacts IP-address-shaped patterns. |
| `ai.existingSecret` | string | `""` | Name of an existing Secret holding the provider API key; not needed for `ollama`. See [Secrets](#secrets). |
| `ai.secretKey` | string | `api-key` | Key within that Secret, mounted into the `CRASHCAUSE_AI_API_KEY` environment variable. |

### runtime / scheduling / security

| Key | Type | Default | Description |
|---|---|---|---|
| `leaderElection.enabled` | bool | `false` | Enables client-go leader election and grants a namespaced Role for Lease create/get/update; required before setting `replicas > 1`. |
| `healthAddr` | string | `:8081` | Listen address for the `/healthz` and `/readyz` endpoints; the probe port is derived from it. |
| `resources.requests.cpu` | string | `50m` | CPU request. |
| `resources.requests.memory` | string | `64Mi` | Memory request. |
| `resources.limits.memory` | string | `256Mi` | Memory limit. There is deliberately no CPU limit: throttling an informer-driven controller causes event-processing lag rather than saving anything useful. |
| `podSecurityContext.runAsNonRoot` | bool | `true` | Refuses to run the pod as root. |
| `podSecurityContext.runAsUser` | int | `65532` | UID the container runs as. |
| `podSecurityContext.runAsGroup` | int | `65532` | GID the container runs as. |
| `podSecurityContext.fsGroup` | int | `65532` | Filesystem group applied to mounted volumes. |
| `podSecurityContext.seccompProfile.type` | string | `RuntimeDefault` | Seccomp profile applied to the pod. |
| `securityContext.allowPrivilegeEscalation` | bool | `false` | Blocks privilege escalation inside the container. |
| `securityContext.privileged` | bool | `false` | Runs the container unprivileged. |
| `securityContext.readOnlyRootFilesystem` | bool | `true` | Mounts the container root filesystem read-only; only `/tmp` (an `emptyDir`) is writable. |
| `securityContext.capabilities.drop` | list | `[ALL]` | Drops every Linux capability. |
| `nodeSelector` | map | `{}` | Node selector for the pod. |
| `tolerations` | list | `[]` | Tolerations for the pod. |
| `affinity` | map | `{}` | Affinity rules for the pod. |
| `priorityClassName` | string | `""` | Priority class assigned to the pod. |
| `extraArgs` | list | `[]` | Extra CLI args appended verbatim after every chart-generated flag; escape hatch for flags this chart does not model yet. |
| `extraEnv` | list | `[]` | Extra environment variables, in raw Kubernetes `env` format. |

## How values map to flags

The Deployment's `args` are built entirely by the `crashcause.args` template
helper (`templates/_helpers.tpl`). Every flag whose value comes from
`values.yaml` is passed explicitly — nothing is left to rely on the binary's
own defaults — so `kubectl describe pod` shows exactly the configuration the
chart was rendered with. `--collect-logs` in particular is **always** passed
explicitly, in both the `true` and `false` case, precisely so the
log-collection state is readable off the Deployment without cross-referencing
chart defaults. `extraArgs` is appended verbatim at the end as an escape
hatch for anything this chart does not model.

| Value | Flag | Condition |
|---|---|---|
| (fixed) | `watch` | Always, as the first positional argument. |
| `logLevel` | `--log-level=<value>` | Always. |
| `watch.namespaces` | `--namespaces=<comma-joined>` | Only when non-empty. |
| `watch.selector` | `--selector=<value>` | Only when set. |
| `watch.reemitInterval` | `--reemit-interval=<value>` | Always. |
| `watch.dedupTTL` | `--dedup-ttl=<value>` | Always. |
| `watch.previousLines` | `--previous-lines=<value>` | Always. |
| `watch.initStuckThreshold` | `--init-stuck-threshold=<value>` | Always. |
| `logCollection.enabled` | `--collect-logs=<true\|false>` | Always, explicitly, in both directions. |
| `logCollection.rateLimitPerMinute` | `--log-rate-limit=<value>` | Only when `logCollection.enabled=true`. |
| `logCollection.namespaces` | `--log-namespaces=<comma-joined>` | Only when `logCollection.enabled=true` and non-empty. |
| `metrics.addr` | `--metrics-addr=<value>` | Only when `metrics.enabled=true`. |
| `loki.url` | `--loki-url=<value>` | Only when set. |
| `healthAddr` | `--health-addr=<value>` | Always. |
| `leaderElection.enabled` | `--leader-elect` | Only when `true` (boolean presence flag, no value). |
| `leaderElection.enabled` (derived) | `--leader-election-id=<release fullname>` | Only when `leaderElection.enabled=true`; the ID is always the chart's fully-qualified release name, not a separate value. |
| `ai.enabled` | `--ai` | Only when `true`. |
| `ai.provider` | `--ai-provider=<value>` | Only when `ai.enabled=true`. |
| `ai.url` | `--ai-url=<value>` | Only when `ai.enabled=true` and set. |
| `ai.model` | `--ai-model=<value>` | Only when `ai.enabled=true` and set. |
| `ai.timeout` | `--ai-timeout=<value>` | Only when `ai.enabled=true` and set. |
| `ai.redact` | `--ai-redact=<true\|false>` | Only when `ai.enabled=true`. |
| `ai.redactIPs` | `--ai-redact-ips=<true\|false>` | Only when `ai.enabled=true`. |
| `ai.namespaces` | `--ai-namespaces=<comma-joined>` | Only when `ai.enabled=true` and non-empty. |
| `extraArgs` | appended verbatim, one entry each | Always, in list order, after every flag above. |

## Security: every privilege is individually severable

The chart grants a single cluster-scoped `ClusterRole` (plus an optional
namespaced `Role` for leader election). No rule in it contains a write verb,
none of it touches Secrets or ConfigMaps, and the only rule that can see
workload *contents* rather than metadata — the pod-log read — can be removed
from the RBAC object itself, not merely switched off in configuration.

| Permission | Why it is needed | How to remove it | What breaks / degrades |
|---|---|---|---|
| `pods` get/list/watch | Container statuses, `lastState.terminated` (exit code, reason, signal), restart counts, QoS class, resource requests/limits — the primary evidence source for every diagnosis. | Cannot be removed. This is the tool. | Nothing to remove without disabling the controller entirely. |
| `events` get/list/watch | `Killing`, `Unhealthy`, `BackOff`, `Failed`, `FailedScheduling`, `Evicted`, `FailedMount`, and similar event reasons. | Not exposed as a chart toggle; would require editing `templates/rbac.yaml` directly. | Every events-only cause is lost: `unschedulable`, `volume_mount_failure`, `probe_liveness_failure`, `probe_startup_failure`, `image_pull_*`. |
| `nodes` get/list/watch | `MemoryPressure` / `DiskPressure` conditions on the crashed pod's node — read-only metadata on the Node object, not access to the node itself. | Not exposed as a chart toggle; would require editing `templates/rbac.yaml` directly. | Only weakens evidence attached to `evicted` and `sigkill_unattributed`; nothing stops working. |
| `pods/log` get (previous-container log tail) | Log tail capped at `watch.previousLines`, rate-limited to `logCollection.rateLimitPerMinute` fetches/minute — the log-pattern evidence source. | `--set logCollection.enabled=false`. This removes the rule from the `ClusterRole` entirely, not just the flag. | `app_exit_nonzero` still fires from exit codes and events, just without log-pattern hints. The AI layer becomes inert: there is nothing left to send it. |
| `coordination.k8s.io` leases create/get/update | Leader election, so multiple replicas do not each independently emit every crash. | Default; only granted at all when `leaderElection.enabled=true`. Leave it at `false` (the default) to grant nothing. | With it absent, `replicas` must stay at `1` (see [Scaling / HA](#scaling--ha)). |

Verify what is actually granted directly against the rendered manifests or a
live cluster:

```bash
helm template crashcause ./charts/crashcause \
  --set logCollection.enabled=false | grep -c 'pods/log'
# -> 0

kubectl get clusterrole crashcause -o yaml
```

**Rate limiting.** `logCollection.rateLimitPerMinute` (default `10`) caps
pod-log fetches client-side. Fetches over budget are skipped — not queued —
and counted in the `crashcause_log_fetches_skipped_total` metric; the
diagnosis for that crash still proceeds, just without log evidence.

**Pod security context.** The container runs as a non-root, fixed UID/GID
(`65532`), with a read-only root filesystem — the only writable path is an
`emptyDir` mounted at `/tmp` — all Linux capabilities dropped, and the
`RuntimeDefault` seccomp profile applied. See the
[runtime / scheduling / security](#runtime--scheduling--security) values
table for the exact settings.

**`crashcause inspect` needs none of this.** It is a `kubectl` plugin that
runs on your workstation using your own kubeconfig and RBAC; it does not use
the ServiceAccount, ClusterRole, or any resource this chart creates.

## Secrets

This chart **never creates a Secret and never accepts a plaintext credential
in `values.yaml`**, because any value set there is stored in the Helm release
(readable via `helm get values`) and, if `values.yaml` is checked in,
permanently in git history. Both credential-bearing sinks instead reference
a Secret you create and manage yourself.

### AI provider API key

Only mounted when both `ai.enabled=true` and `ai.existingSecret` is set — and
not needed at all for `ai.provider=ollama`. The key is injected into the
`CRASHCAUSE_AI_API_KEY` environment variable, never passed as a flag (process
listings leak flags) and never logged.

```bash
kubectl create secret generic crashcause-ai \
  --from-literal=api-key=sk-... -n crashcause
```

```yaml
ai:
  enabled: true
  provider: anthropic
  existingSecret: crashcause-ai
  secretKey: api-key   # default; change only if your secret key is named differently
```

### Loki credentials

Referenced via `loki.existingSecret`. Its keys are injected verbatim as
environment variables (`envFrom.secretRef`), so they must be named exactly
`LOKI_USERNAME` + `LOKI_PASSWORD` (basic auth) or `LOKI_BEARER_TOKEN`.

```bash
kubectl create secret generic crashcause-loki \
  --from-literal=LOKI_USERNAME=... \
  --from-literal=LOKI_PASSWORD=... \
  -n crashcause
```

```yaml
loki:
  url: http://loki:3100/loki/api/v1/push
  existingSecret: crashcause-loki
```

A base URL (`http://loki:3100`) works too — the `/loki/api/v1/push` path is
appended automatically when not already present.

## AI notes

AI summarization is off by default (`ai.enabled=false`). When enabled:

- `ai.namespaces` is the **primary control**, and it is fail-closed: leaving
  it empty keeps AI summarization off in every namespace, even with
  `ai.enabled=true`. Set it to the specific namespaces you want summarized,
  or to `["*"]` to explicitly opt every watched namespace in.
- `ai.redact` / `ai.redactIPs` are **defense-in-depth**: best-effort pattern
  matching over log content, not a guarantee that sensitive data will never
  be sent. Do not enable AI summarization for workloads whose logs may
  contain secrets you cannot afford to send off-cluster, relying on
  redaction alone.
- `ai.provider=ollama` pointed at an in-cluster `ai.url` (e.g.
  `http://ollama:11434`) is the recommended option when data must not leave
  the cluster: nothing is sent to an external provider.

## Scaling / HA

One replica is the normal, and only fully correct, configuration.

- `replicas > 1` **requires** `leaderElection.enabled=true`. Without leader
  election every replica runs its own informers and its own in-memory dedup
  cache, so N replicas means N independent observations and N independent
  emissions of every crash — duplicate log lines and inflated Prometheus
  counters, not more throughput.
- With `leaderElection.enabled=true`, extra replicas sit idle as hot
  standbys; only the elected leader is active. They exist for availability
  during a leader restart, not for horizontal scaling.
- The Deployment's update strategy is `Recreate` (not `RollingUpdate`), so
  the old and new pod never briefly run at once and double-emit during a
  rollout. The controller sits outside any request path, so the resulting
  few seconds of downtime cost nothing.
- The Deployment injects `POD_NAMESPACE` via the downward API, and the
  controller's `--leader-election-namespace` defaults to `$POD_NAMESPACE`, so
  the Lease is always created in the release namespace — which is exactly why
  the leader-election grant in RBAC is a namespaced `Role` rather than another
  cluster-wide rule.

## Observability

Two Prometheus metrics are exposed on `metrics.addr` (default `:9090`) when
`metrics.enabled=true`:

- `crashcause_diagnoses_total{namespace, owner_kind, owner_name, cause}` —
  counter, incremented on every observed crash regardless of re-emit
  deduplication.
- `crashcause_log_fetches_skipped_total` — counter of pod-log fetches
  skipped because `logCollection.rateLimitPerMinute` was exceeded.

Check the endpoint directly:

```bash
kubectl port-forward -n crashcause svc/crashcause-metrics 9090:9090
curl localhost:9090/metrics | grep crashcause_
```

A ready-made dashboard and alert rules ship in the repository, not in this
chart:

- `../../examples/grafana-dashboard.json`
- `../../examples/prometheus-alerts.yaml`

## Known limitations in 0.1.0

- **RBAC is always cluster-scoped**, even when `watch.namespaces` restricts
  what is watched. This is because the controller also reads cluster-scoped
  Node conditions (for memory/disk-pressure evidence) and because
  cross-namespace informer setup is the only tested code path. Per-namespace
  `Role` generation (in place of a `ClusterRole`) is a candidate for a later
  chart version, not something achievable today via values alone.
- **Dedup state is in-memory only.** A controller restart (rollout, crash,
  node drain) forgets all dedup keys, so each currently-active crash key is
  re-emitted once after restart.
- **The chart is not published to a chart repository yet.** Install from a
  checked-out copy of this repository, as shown above.

## Uninstall

```bash
helm uninstall crashcause -n crashcause
```

This removes every object the chart created, including the cluster-scoped
`ClusterRole` and `ClusterRoleBinding` (and the leader-election `Role` /
`RoleBinding` if it was created). Nothing persists afterward: the chart
defines no CRDs and no PersistentVolumeClaims.
