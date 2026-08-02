# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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
