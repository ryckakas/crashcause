# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Go module and dependency pinning (`github.com/ryckakas/crashcause`, Go 1.23,
  toolchain go1.23.1).
- Shared engine type contracts: `CauseCode` taxonomy, `Diagnosis`, and `Inputs` types.
- Cobra CLI skeleton with `inspect` and `watch` commands (both not yet implemented).
- Apache-2.0 license.
- CI workflows: lint, test (with race detector and coverage), cross-build matrix,
  govulncheck, helm, and a best-effort kind e2e job.
- goreleaser release configuration: multi-arch `ghcr.io` container image and SBOM
  generation.
- kind-demo example manifests.
