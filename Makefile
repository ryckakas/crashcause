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
	@echo "  e2e        - kind-based end-to-end run (requires kind + kubectl + jq)"
	@echo "  helm-lint  - lint + template charts/crashcause, incl. pods/log RBAC assertions"
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

# e2e stands up a throwaway kind cluster, applies examples/kind-demo/, and
# asserts the cause code and exit code crashcause produces for every scenario
# (plus a healthy control pod that must exit 2). All of that logic lives in
# hack/e2e.sh, which CI runs too - the only difference is that CI creates the
# cluster itself via the kind action, so it omits --create-cluster.
#
# Local runs cost a few minutes: the cluster boot plus the bounded per-pod
# waits (the stuck-init scenario alone needs ~35s of genuine init time).
e2e:
	$(call check_tool,kind,go install sigs.k8s.io/kind@latest)
	$(call check_tool,kubectl,see https://kubernetes.io/docs/tasks/tools/#kubectl)
	$(call check_tool,jq,see https://jqlang.github.io/jq/download/)
	@test -d examples/kind-demo || { echo "crashcause: examples/kind-demo/ not found"; exit 1; }
	@test -f hack/e2e.sh || { echo "crashcause: hack/e2e.sh not found"; exit 1; }
	bash hack/e2e.sh --create-cluster

# helm-lint mirrors the CI helm job exactly, including its grep assertions on
# the severable pods/log RBAC rule - "green locally = green in CI" only holds
# if this target checks the same contract, not just that the chart renders.
helm-lint:
	@test -d charts/crashcause || { echo "crashcause: charts/crashcause/ not found"; exit 1; }
	$(call check_tool,helm,see https://helm.sh/docs/intro/install/)
	helm lint charts/crashcause
	@echo "crashcause: rendering with default values (log collection enabled)..."
	@helm template charts/crashcause > /tmp/crashcause-rendered-default.yaml
	@grep -q "pods/log" /tmp/crashcause-rendered-default.yaml || \
		{ echo "FAIL: expected the 'pods/log' RBAC verb in the default render, but it was not found"; exit 1; }
	@echo "crashcause: OK - pods/log is present by default"
	@echo "crashcause: rendering with logCollection.enabled=false..."
	@helm template charts/crashcause --set logCollection.enabled=false > /tmp/crashcause-rendered-nolog.yaml
	@if grep -q "pods/log" /tmp/crashcause-rendered-nolog.yaml; then \
		echo "FAIL: 'pods/log' RBAC verb should be removed when logCollection.enabled=false, but it was found"; \
		exit 1; \
	fi
	@echo "crashcause: OK - pods/log is absent when logCollection.enabled=false"

# check mirrors what CI runs: green here should mean green in CI.
check: lint race
	$(call check_tool,govulncheck,go install golang.org/x/vuln/cmd/govulncheck@latest)
	go vet ./...
	govulncheck ./...

clean:
	rm -rf bin dist coverage.out coverage.txt coverage.html cover.out
