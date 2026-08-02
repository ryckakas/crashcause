# crashcause

Automatically answers "why did this pod crash?" — from live cluster state, not copy-pasted output.

> **Status: under construction (scaffold phase).** This project is in Milestone 1 of 10.
> There is no release yet, and neither command does anything useful today: both
> `crashcause inspect` and `crashcause watch` currently return "not yet implemented".
> Nothing below should be read as "you can use this now" unless explicitly marked as working.

## What it does

Diagnosing a crashing pod today is a manual ritual: `kubectl describe`, `kubectl logs
--previous`, digging through events, and eyeballing metrics to correlate a restart with an
OOM kill or a failed probe. `crashcause` aims to automate that correlation and turn the
*cause* of a crash — not just the fact that one happened — into a first-class, queryable
signal.

Two things differentiate it from "paste your logs into a UI" tools:

- **Zero copy-paste, live-cluster.** It talks to the Kubernetes API directly — pod status,
  events, previous-container logs, and node conditions — instead of asking you to paste
  `describe`/`logs` output somewhere.
- **Native observability integration.** Crash *causes* (`oom_killed`, `probe_liveness_failure`,
  `config_missing_reference`, etc.), not just crash counts, become Prometheus metric labels
  and Loki structured log lines. Where `kube-state-metrics` tells you a pod is in
  `CrashLoopBackOff`, `crashcause` aims to tell you *why*, as a queryable dimension.

## Planned install paths

None of these exist yet. They are the intended distribution channels once there's something
to ship:

- **PLANNED:** `kubectl krew install crashcause` (krew plugin index)
- **PLANNED:** Helm chart for running `watch` mode in-cluster
- **PLANNED:** Prebuilt binaries via GitHub Releases
- **PLANNED:** Container image at `ghcr.io/ryckakas/crashcause`

**What works today:** building from source.

```sh
go build ./cmd/crashcause
```

## Status / roadmap

- [x] **1. Scaffold** — cobra CLI skeleton, CI, goreleaser config, kind-demo manifests *(in progress — this repo)*
- [ ] 2. Collectors + engine with 5 core classification rules + tests
- [ ] 3. `inspect` working end-to-end against kind-demo (first usable artifact, `v0.1.0-alpha`)
- [ ] 4. Remaining rules + `unknown` fallback
- [ ] 5. `watch` mode + stdout sink + dedup
- [ ] 6. Prometheus sink + example dashboard/alerts
- [ ] 7. Loki sink
- [ ] 8. Optional AI log-summarization layer (redactor first, then providers)
- [ ] 9. Helm chart + krew manifest + install docs
- [ ] 10. README polish, demo, `v0.1.0`

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
