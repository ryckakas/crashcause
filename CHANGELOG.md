# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `--ai-model` flag on `inspect` and `watch` (and `ai.model` in the Helm
  chart) to override the provider's default model. The `ai` package already
  supported it; only the CLI wiring was missing, which pinned ollama users
  to the 4.7 GB `llama3.1` default with no way to choose a smaller local
  model such as `llama3.2:3b`.
- `--ai-timeout` flag (and `ai.timeout` in the chart) bounding each summary,
  plus `ai.NewSummarizerWithTimeout`. The 15s default suits a hosted API but
  is routinely too short for a self-hosted ollama, where the first call also
  pays for loading several GB of weights.
- Go module and dependency pinning (`github.com/ryckakas/crashcause`, Go 1.23,
  toolchain go1.23.1).
- Shared engine type contracts: `CauseCode` taxonomy, `Diagnosis`, and `Inputs` types.
- Cobra CLI skeleton with `inspect` and `watch` commands.
- Apache-2.0 license.
- CI workflows: lint, test (with race detector and coverage), cross-build matrix,
  govulncheck, helm, and a best-effort kind e2e job.
- goreleaser release configuration: multi-arch `ghcr.io` container image and SBOM
  generation.
- kind-demo example manifests.
- Helm chart at `charts/crashcause/` for running `watch` mode in-cluster:
  Deployment, ServiceAccount, RBAC, optional metrics Service and
  ServiceMonitor, NOTES.txt, and a chart README documenting every value.
- Severable RBAC in the chart: `logCollection.enabled=false` removes the
  `pods/log` rule from the ClusterRole entirely (not just the
  `--collect-logs` flag), and leader election's `leases` permission is a
  namespaced Role granted only when `leaderElection.enabled=true`.
- Chart secret handling: `ai.existingSecret`/`ai.secretKey` mounted into
  `CRASHCAUSE_AI_API_KEY` and `loki.existingSecret` injected as env; the
  chart never creates a Secret and never accepts a plaintext key.
- `examples/grafana-dashboard.json` — importable dashboard (crash causes over
  time, top crashing workloads, per-cause stats, log-fetch rate-limit
  saturation) with datasource/namespace/cause template variables.
- `examples/prometheus-alerts.yaml` — plain Prometheus rules file (also
  usable as a `PrometheusRule` body) covering OOM kills, sustained
  crashlooping, image-pull auth failures, and log-fetch rate-limit
  saturation.
- Full project README: install paths (krew manifest and Helm), `inspect` and
  `watch` usage, sink documentation, the AI layer's privacy notice, and the
  severable-privileges security section.
- `hack/e2e.sh`, a kind-based end-to-end harness: it applies every scenario
  manifest in `examples/kind-demo/`, creates an additional healthy control
  pod, bounded-polls each pod for its expected diagnosable state, and runs
  `crashcause inspect --output json` against each one, asserting both the
  expected cause code and exit code — including the healthy-control case,
  where `diagnoses` must be empty and the exit code must be `2`. Both
  `make e2e` and the CI `e2e-kind` job drive this script; it remains
  best-effort (non-gating, `continue-on-error: true`) rather than a merge
  blocker.

### Changed

- krew manifest: valid 64-character sha256 placeholders (a well-formed but
  never-matching digest, so an un-rewritten manifest fails the checksum check
  instead of installing something), a documented release-time rewrite
  contract, LICENSE included in the extracted files, and a description
  consistent with the README.
- Replaced the milestone-1 placeholder e2e job/target with the real
  `hack/e2e.sh` harness wired into `make e2e` and CI, and trued up the
  README status block and the `examples/kind-demo/README.md` walkthrough to
  match the now-implemented `inspect`/`watch` commands and completed Helm
  chart.
- Go toolchain moved from 1.23.1 to a pinned 1.25.0, pulled forward by the
  security upgrades below.

- Kubernetes dependency train upgraded from v0.31.4 to v0.35.2 (client-go,
  api, apimachinery, cli-runtime); tests moved off the deprecated
  `fake.NewSimpleClientset` to `fake.NewClientset`. v0.36+ is deliberately
  held back via a Dependabot ignore rule: it requires Go 1.26, which
  golangci-lint does not support yet.

### Security

- Upgraded indirect dependencies `golang.org/x/text` (v0.16.0 → v0.40.0) and
  `golang.org/x/net` (v0.26.0 → v0.57.0) to clear govulncheck findings
  GO-2026-5970, GO-2026-5026 and GO-2026-4918, all reachable through the AI
  layer's HTTP client.

### Fixed

- `.golangci.yml`: removed the `exhaustive.check-generated` setting, which
  golangci-lint v2.12 no longer accepts (config schema validation failed in
  CI).
