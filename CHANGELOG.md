# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- New cause code `security_context_violation` (the 18th): a
  `CreateContainerConfigError` caused by a securityContext rejection —
  `runAsNonRoot: true` against an image that runs as root, an image with a
  non-numeric user, or an explicit `runAsUser: 0` — is now reported as its own
  high-confidence cause instead of being folded into
  `config_missing_reference` (#17).
- `inspect` against an unreachable cluster (expired or stale credentials, a
  missing kubeconfig exec plugin, DNS or connection failures) now prints a
  one-line "cannot reach or authenticate to cluster <server>" hint naming the
  API server and pointing at the kubeconfig context, instead of leaking a raw
  client-go error chain. The exit code stays 1 (#17).

### Fixed

- A pod in `CreateContainerConfigError` whose Failed event message merely
  contained the word "image" ("container has runAsNonRoot and image will run
  as root") was misclassified as `image_pull_other` — a verdict refuted by the
  successful `Pulled` event in the same event list. The `image_pull_*` rules
  now stand down whenever the container's waiting reason names a non-pull
  failure family; `SignatureValidationFailed` was added to the pull-reason set
  so signature-verification failures keep classifying as pull failures (#17).
- `config_missing_reference`'s fallback explanation no longer asserts that a
  ConfigMap/Secret reference is missing when the kubelet message was not
  parseable as one; it now points at the quoted kubelet message for the actual
  rejection.

## [0.1.0] - 2026-08-03

First public release.

### Added

- `--ai-model` flag on `inspect` and `watch` (and `ai.model` in the Helm
  chart) to override the provider's default model. The `ai` package already
  supported it; only the CLI wiring was missing, which pinned ollama users
  to the 4.7 GB `llama3.1` default with no way to choose a smaller local
  model such as `llama3.2:3b`.
- Two log-tail patterns: `dns resolution failure` (Python `gaierror`, glibc
  `Name or service not known`, Go `no such host`, Node `EAI_NONAME`, libpq
  `could not translate host name`) and `python traceback`. A Python service
  failing to resolve its database — one of the most common shapes of failure
  in a cluster — previously produced "no known crash pattern matched".
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

- OpenAI provider sends `max_completion_tokens` instead of the deprecated
  `max_tokens` parameter.

### Security

- Upgraded indirect dependencies `golang.org/x/text` (v0.16.0 → v0.40.0) and
  `golang.org/x/net` (v0.26.0 → v0.57.0) to clear govulncheck findings
  GO-2026-5970, GO-2026-5026 and GO-2026-4918, all reachable through the AI
  layer's HTTP client.

### Fixed

- Flaky Loki test that could hang CI for ten minutes.
  `TestLokiEmitNeverBlocksAndDropsOnFullBuffer` asserted a 100ms per-call
  latency bound, which a non-blocking channel send can exceed purely from
  scheduler starvation when `-race` and a parallel package run saturate the
  CPU. Worse, the assertion fired before the test released the wedged HTTP
  handler, so `httptest.Server.Close` waited on the in-flight request and the
  failure became a test-binary timeout. The property is now checked with a
  watchdog over the whole batch, and the test server releases parked handlers
  on cleanup regardless of how the test exits. The sink itself was correct.
- Probe explanations quoted false arithmetic: `probeBudget` folded
  `initialDelaySeconds` into the returned budget, so `probe_startup_failure`
  printed `failureThreshold=2 x periodSeconds=5 = 15s` (that is 10s), and
  `probe_liveness_failure` called the initial delay part of the time spent
  "of failing probes" — no probe runs during it. The helper now returns the
  probing window and the total separately.
- Log-hint evidence quoted the wrong line: `findLine` scanned lines before
  needles, so a Python DNS failure was evidenced by the intermediate stack
  frame `for res in getaddrinfo(...)` instead of the `Name does not resolve`
  line. Needle order (most specific first) now decides, and within a needle
  the last match wins, since a crash log's decisive line is at its end.
- `probe_liveness_failure` emitted a broken suggested command: it stripped the
  leading space from the container flag, welding it onto the namespace
  (`kubectl exec pod -n prod-c api -- ...`), so the command failed if pasted.
  Found while running the tool against a live kind cluster. Regression tests
  now assert that no rule's suggested commands concatenate flags.

- `.golangci.yml`: removed the `exhaustive.check-generated` setting, which
  golangci-lint v2.12 no longer accepts (config schema validation failed in
  CI).
- Loki sink now accepts both a base URL and a full push URL; previously the
  documented full-URL form (`http://loki:3100/loki/api/v1/push`) silently
  failed because the push path was appended a second time.
- Statically-broken pods (unschedulable, stuck init) now re-emit per
  re-emit interval and keep their Prometheus metric series alive for as
  long as they stay broken.
- Deleting one finished Job run no longer resets the shared CronJob metric
  series.
- Standby replicas no longer stall 10 seconds at shutdown with a misleading
  drain warning.
- Events are now attributed to their container, preventing cross-container
  misdiagnosis in multi-container pods.

[Unreleased]: https://github.com/ryckakas/crashcause/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/ryckakas/crashcause/releases/tag/v0.1.0
