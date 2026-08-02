# Makefile for crashcause.
#
# Targets here assume a Linux/CI-style shell environment (this is what runs
# in GitHub Actions); it is not meant to be Windows-native.
#
# Several targets depend on tools that are NOT installed by default on a
# fresh dev machine: golangci-lint, gofumpt, goimports, helm, kind,
# govulncheck. Each such target checks for its tool first and fails with an
# install hint rather than a confusing downstream error.

SHELL := /bin/bash

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: help fmt lint test race cover build e2e helm-lint check clean

## help: show this help (default goal)
help:
	@echo "crashcause make targets:"
	@echo "  fmt        - gofumpt + goimports (auto-fix, local dev)"
	@echo "  lint       - golangci-lint run"
	@echo "  test       - go test ./..."
	@echo "  race       - go test -race ./..."
	@echo "  cover      - race tests + coverage report (coverage.out)"
	@echo "  build      - build ./cmd/crashcause into ./bin/crashcause"
	@echo "  e2e        - kind-based end-to-end run (requires kind + kubectl)"
	@echo "  helm-lint  - lint + template charts/crashcause (skips if chart absent)"
	@echo "  check      - the set CI runs: lint, race, vet, govulncheck"
	@echo "  clean      - remove build/coverage artifacts"

.DEFAULT_GOAL := help

# check_tool: fail with an install hint if $(1) is not on PATH.
# Usage: $(call check_tool,<binary-name>,<install-hint>)
define check_tool
@command -v $(1) >/dev/null 2>&1 || { echo "crashcause: '$(1)' is required but not installed."; echo "  install: $(2)"; exit 1; }
endef

fmt:
	$(call check_tool,gofumpt,go install mvdan.cc/gofumpt@latest)
	$(call check_tool,goimports,go install golang.org/x/tools/cmd/goimports@latest)
	gofumpt -w .
	goimports -w .

lint:
	$(call check_tool,golangci-lint,official install script at https://golangci-lint.run/welcome/install/ or: brew install golangci-lint)
	golangci-lint run

test:
	go test ./...

race:
	go test -race ./...

cover:
	go test -race -coverprofile=coverage.out -covermode=atomic ./...
	go tool cover -func=coverage.out

build:
	@mkdir -p bin
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/crashcause ./cmd/crashcause

# e2e is intentionally simple and honest: there is no working inspect/watch
# yet (milestone 1 = scaffold only), so this does not claim to validate
# behavior. It exists so the target is wired up consistently for when
# examples/kind-demo/ becomes a real end-to-end check in milestone 3.
e2e:
	$(call check_tool,kind,go install sigs.k8s.io/kind@latest)
	$(call check_tool,kubectl,see https://kubernetes.io/docs/tasks/tools/#kubectl)
	@test -d examples/kind-demo || { echo "crashcause: examples/kind-demo/ not found"; exit 1; }
	@echo "crashcause: e2e is not implemented beyond this guard yet (inspect/watch are not yet implemented)."
	@echo "crashcause: kind-demo manifests are present at examples/kind-demo/ for future use."

# helm-lint: the chart lands in milestone 9 and does not exist yet, so this
# skips cleanly (exit 0) rather than failing, unlike the other tool guards.
helm-lint:
	@if [ ! -d charts/crashcause ]; then \
		echo "crashcause: charts/crashcause not present yet (planned for milestone 9), skipping helm-lint."; \
		exit 0; \
	fi
	$(call check_tool,helm,see https://helm.sh/docs/intro/install/)
	helm lint charts/crashcause
	helm template charts/crashcause
	helm template charts/crashcause --set logCollection.enabled=false

# check mirrors what CI runs: green here should mean green in CI.
check: lint race
	$(call check_tool,govulncheck,go install golang.org/x/vuln/cmd/govulncheck@latest)
	go vet ./...
	govulncheck ./...

clean:
	rm -rf bin dist coverage.out coverage.txt coverage.html cover.out
