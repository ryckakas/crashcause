# Sinks and the dedup model

The deep-dive behind the README's [`watch` mode + sinks](../README.md#watch-mode--sinks)
summary: what each sink emits, the cardinality rules that keep them safe to run for
months, and how deduplication decides when a diagnosis is worth another line.

## stdout (default, always on)

One JSON object per line. This sink alone is worth documenting loudly: any team already
scraping container stdout — promtail, vector, or fluent-bit shipping to Loki, CloudWatch,
or anything else — gets structured, Loki-shaped crash-cause data for free, with **no
direct-push configuration at all**. If you already have a log pipeline, you may not need
`--loki-url` or a Prometheus scrape target to get value out of `watch`.

## Prometheus (`--metrics-addr :9090`)

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

Example Grafana dashboard and Prometheus alert rules: [`../examples/grafana-dashboard.json`](../examples/grafana-dashboard.json),
[`../examples/prometheus-alerts.yaml`](../examples/prometheus-alerts.yaml).

## Loki push (`--loki-url ...`)

Pushes the full `Diagnosis` (including `ai_summary`, if any) as one JSON log line. Stream
labels are `{app="crashcause", namespace, cause}` only — same cardinality discipline as
Prometheus. Auth via `LOKI_USERNAME`/`LOKI_PASSWORD` or `LOKI_BEARER_TOKEN` environment
variables. On backpressure, pushes are dropped with a counter rather than blocking the
watch loop — a slow or unavailable Loki must never stall crash classification.

## Dedup model

Emission for the log-style sinks (stdout, Loki) is keyed on `(namespace, owner, container,
cause)` — deliberately **not** pod name and **not** restart count, so a ReplicaSet
replacing pods, or a container stuck in a backoff loop, doesn't spam a log line every few
seconds. A key re-emits on first occurrence, on a cause change for the same workload, and
at most once per `--reemit-interval` while the condition persists. The Prometheus counter
ignores this entirely and increments on every observed crash.

Dedup state is an in-memory cache with TTL eviction (`--dedup-ttl`) — there is no
persistence. After a controller restart, every key re-emits once, since the controller has
no memory of what it already reported. This is accepted, documented behavior, not a bug.
