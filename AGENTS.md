# AGENTS.md

This file provides guidance to AI coding agents (Claude Code, etc.) when working with code in
this repository. `CLAUDE.md` is a symlink to this file.

## What this is

`crashcause` is a Go CLI/kubectl plugin (`crashcause inspect`) plus an in-cluster controller
(`crashcause watch`, deployed via the Helm chart in `charts/crashcause/`) that diagnoses *why* a
Kubernetes pod crashed — OOM kill, failed probe, bad image pull, missing ConfigMap/Secret
reference, unschedulable, etc. — from live cluster state, with an optional opt-in AI layer for the
cases the rule engine can't fully interpret (`app_exit_nonzero` / `unknown`).

Every diagnosis resolves to one of 18 stable, snake_case `CauseCode` values defined in
`internal/engine/types.go`. **These codes are part of the external contract** (used in JSON output
and Prometheus metric labels) — never rename, renumber, or repurpose one; see the README's
"Cause codes" table for what each one means and its exact detection rule.

## Commands

Build requires Go 1.25 (`toolchain go1.25.0` pinned in `go.mod`; any Go ≥ 1.21 auto-downloads it).
The Makefile targets assume a Linux/bash-style shell (this is what CI runs) — on Windows use Git
Bash or WSL for `make`, or run the underlying `go`/`golangci-lint` commands directly.

```sh
make build          # -> ./bin/crashcause
make test           # go test ./...
make race           # go test -race ./...
make cover          # race + coverage.out + go tool cover -func
make fmt            # gofumpt -w . && goimports -w .   (requires gofumpt, goimports)
make lint           # golangci-lint run                (requires golangci-lint v2)
make check          # what CI gates on: lint, race, go vet, govulncheck
make helm-lint       # helm lint + template, asserts pods/log RBAC is present/absent correctly
make e2e            # kind-based end-to-end run (requires kind, kubectl, jq) — see hack/e2e.sh
```

Single test / single package:

```sh
go test ./internal/engine/...
go test -run TestClassify_OOMKilled ./internal/engine/...
go test -race -run TestName ./internal/collect/...
```

`make check` is what CI actually gates on for the `lint`/`test`/`build`/`govulncheck`/`helm` jobs
(see `.github/workflows/ci.yml`); `e2e-kind` in CI is best-effort/non-gating. Run `make check`
before considering a change done. golangci-lint config (`.golangci.yml`) enables `exhaustive`
switch-checking specifically over `engine.CauseCode` with `default-signifies-exhaustive: false` —
**adding a new CauseCode will force-fail every `switch` over CauseCode that doesn't handle it**,
by design. Bare `//nolint` (no reason comment) is a lint failure, not just a style nit.

## Architecture

Layers, from pure to impure, each with a one-way dependency on the one below it:

1. **`internal/engine`** — the pure classification core. Performs *no I/O whatsoever* (no k8s
   client, no filesystem, no network, no wall-clock reads — even "now" comes in via
   `Inputs.Now`). It must never import any `k8s.io/*` package. It consumes a plain-Go `Inputs`
   struct and returns `[]Diagnosis` from an ordered `Rule` table (`rules.go`): `Classify` walks
   the rules top-to-bottom, `Rules()`'s order is the priority order and is load-bearing, and
   `promotePrimary` reorders only to float a high-confidence hit to index 0 while preserving
   everyone else's relative order. An empty result means "normal, nothing to report" — never
   conflate that with `CauseUnknown`. `isLifecycleNoise` is a deliberate pre-rule guard: a SIGTERM
   (exit 143 / signal 15) during pod deletion or a rolling update is suppressed entirely so a
   normal deploy doesn't read as a stream of crashes.

2. **`internal/collect`** — turns a `*corev1.Pod` (plus its events/logs/node) into one or more
   `engine.Inputs` values via `kubernetes.Interface`. Collection is deliberately forgiving: only
   the pod GET failing, or an unknown `--container` filter, is a hard error — events, logs, and
   node conditions are all best-effort and degrade gracefully. A pod-level problem (unschedulable,
   evicted) produces exactly one *synthetic* target rather than one per container, so it isn't
   fanned out into N duplicate diagnoses. `collect.Options` carries an injectable clock (`Now`)
   and an optional `*rate.Limiter` for `pods/log` fetches — `inspect` runs unthrottled (single
   pod, user's own credentials); `watch` throttles (crash storms across a whole cluster).

3. **`internal/ai`** — the optional AI-summary layer, gated behind `Provider` (anthropic / openai
   / ollama — plain HTTP clients, no vendor SDKs). Bring-your-own-key via
   `CRASHCAUSE_AI_API_KEY`, never a flag or config file. AI failure is always non-fatal: it can
   only leave `Diagnosis.AISummary` nil, never withhold or fail a diagnosis. Only
   `app_exit_nonzero` and `unknown` primary causes are AI-eligible (`aiCauses` in
   `internal/inspect/inspect.go`, mirrored in `watch/controller.go`'s `maybeSummarize`) — rule-based
   causes already have their explanation; only an app-level crash has a log tail worth
   summarizing.

4. **Two driving modes, both built on the three layers above:**
   - **`internal/inspect`** — one-shot `crashcause inspect`. Cobra-free (`Run(ctx, client, Options)`
     takes an already-fully-resolved `Options`, so the whole mode is testable with a fake
     clientset). Returns a process exit code as part of the contract, not a Go error: `0`
     diagnosed, `1` error, `2` "pod exists, nothing to diagnose" (see README's Exit codes table).
   - **`internal/watch`** — long-running controller: informer-driven (`client-go/informers`),
     fingerprint-based change detection (`observe`/`podFingerprint`) so unchanged pod state
     doesn't re-trigger collection, a bounded worker pool draining a dropped-on-full queue, a
     dedup cache (`internal/watch/dedup.go`) gating *emission* while the Prometheus counter still
     increments on every observed crash regardless of dedup, and optional leader election
     (`leader.go`) so only one replica is active. Watch reports only the *primary* diagnosis per
     container (not the full ranked list `--verbose` gives `inspect`).

5. **`internal/sinks`** — `watch`'s output destinations (stdout NDJSON always on, Prometheus,
   Loki), each independently enabled and independently fail-safe: one sink failing must never
   affect another or stall the controller (`emitTimeout` bounds each `Emit` call).

6. **`internal/cli`** — cobra wiring only (root command, flags, `--log-level`). No inspection or
   watch logic lives here; `internal/cli/inspect.go` / `watch.go` just parse flags and call into
   `internal/inspect` / `internal/watch`.

Metric/RBAC design note worth knowing before touching the chart: `logCollection.enabled=false`
doesn't just turn log collection off in config, it removes the `pods/log` RBAC verb from the
rendered `ClusterRole` entirely (`charts/crashcause/templates/rbac.yaml`) — CI and `make
helm-lint` both assert this by grepping the rendered manifest, so a chart change that breaks that
severability will fail loudly.

## Comments

No comments, strictly, on hand-written code. Names and small functions carry the meaning; if a
comment feels needed, that's usually a sign the code should be restructured or renamed instead.

Two narrow exceptions:

- **Auto-generated code** (mocks, protobuf/codegen output, etc.) is exempt — leave whatever the
  generator produces alone.
- **"Why" comments**, only when absolutely necessary: a non-obvious invariant, a workaround for a
  specific upstream bug/quirk, or a constraint that isn't visible at the call site. Never a "what"
  comment restating what the code already says. This repo's existing doc comments on exported
  package/type/rule identifiers (e.g. `internal/engine`'s package doc, `Rule`, `Rules()`) are this
  kind of load-bearing "why", not restatement — keep that standard, don't add narration on top of
  it.

## Testing conventions

Tests are colocated per package (`*_test.go`, `helpers_test.go` for shared fixtures). The engine's
rule table is directly testable per-rule (`rules_test.go`, `classify_test.go`) since `Rule.Match`
is a pure `Inputs -> *Diagnosis` function — prefer adding a case to the existing table-driven tests
over writing a new test file when adding rule coverage. `collect` and `watch` tests drive
`k8s.io/client-go/kubernetes/fake` with an injected/frozen clock rather than real time.
