# ----------------------------------------------------------------------------
# agentmoat: top-level Makefile
# ----------------------------------------------------------------------------
#
# This Makefile is the canonical entry point for local developer workflows and
# CI. It deliberately stays simple: every target shells out to standard Go
# tooling so contributors do not have to learn project-specific abstractions.
#
# Conventions used here:
#   * Every target is .PHONY (none of them produce a file at the literal target
#     name), so `make` never short-circuits because of a stale timestamp.
#   * Each target is annotated with a `## description` comment on the same
#     line. The `help` target parses this file with awk to print a list of
#     targets and their descriptions. This is the standard self-documenting
#     Makefile pattern. To add a target to the help output, just put a `##`
#     comment after the target name.
#   * `help` is the default target so `make` with no arguments prints usage
#     instead of silently building. This is friendlier for newcomers.
#
# Conventions for variables:
#   * VERSION defaults to `dev` so unversioned local builds are clearly tagged
#     as such. CI / goreleaser pass in a real semver tag at release time.
#   * GIT_SHA shells out to `git rev-parse --short HEAD`. The redirection of
#     stderr to /dev/null and the fallback to `unknown` keeps `make` working
#     in environments without a `.git` directory (for example, a vendored
#     source tarball or a sandbox without git).
# ----------------------------------------------------------------------------

# VERSION is the human-readable build label (e.g. v0.1.0). Override on the
# command line: `make build VERSION=v0.1.0`.
VERSION ?= dev

# GIT_SHA is the short commit hash baked into the binary at build time, used
# by `agentmoat version` for support and bug reports. Computed lazily by Make.
GIT_SHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# LDFLAGS injects build metadata into the `main` package of each binary. The
# CLI and MCP server both expose `Version` and `GitSHA` package-level vars.
# Keep these names in sync with the goreleaser config (.goreleaser.yaml).
LDFLAGS := -X main.Version=$(VERSION) -X main.GitSHA=$(GIT_SHA)

# BIN_DIR is the output directory for locally built binaries. CI artifacts and
# release tarballs go elsewhere (goreleaser owns those paths).
BIN_DIR := ./bin

# Default goal: print help. Newcomers running a bare `make` see usage, not a
# 60-second test run.
.DEFAULT_GOAL := help

# Declare every target as phony. None of these targets produce a file at the
# literal name of the target, so Make should always run the recipe.
.PHONY: help build test lint tidy clean version kind-up kind-down e2e

help: ## Print this help message (default target).
	@# The awk pattern below scans this Makefile for lines of the form
	@# `target: ## description` and prints them in two columns. This keeps the
	@# help text in sync with the recipes automatically: there is no second
	@# place to update when a target is added or renamed.
	@awk 'BEGIN {FS = ":.*?## "; printf "Usage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} \
		/^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build both binaries (agentmoat, agentmoat-mcp) into ./bin.
	@# We build each binary in its own invocation so that a failure on one
	@# produces a clear error message. Both binaries embed Version and GitSHA
	@# via -ldflags so `agentmoat version` and `agentmoat-mcp version` report
	@# accurate provenance even for local dev builds.
	@mkdir -p $(BIN_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/agentmoat ./cmd/agentmoat
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/agentmoat-mcp ./cmd/agentmoat-mcp

test: ## Run unit tests with race detector and coverage.
	@# -race catches data races early (cheap insurance for a concurrent
	@# tool that talks to the K8s API and an MCP transport).
	@# -cover prints a per-package coverage summary; CI uploads the profile
	@# in a later phase when coverage gating is added.
	go test -race -cover ./...

lint: ## Run golangci-lint against the whole module.
	@# Configuration lives in .golangci.yml at the repo root. Run this before
	@# every PR; CI gates on the same command.
	golangci-lint run ./...

tidy: ## Run `go mod tidy` to prune and align go.mod / go.sum.
	@# Run after adding or removing imports. Keeping go.mod tidy avoids noisy
	@# diffs and surprise transitive upgrades.
	go mod tidy

clean: ## Remove build artifacts under ./bin.
	@# Intentionally narrow: we only remove our own outputs, never the Go
	@# build cache or module cache. Use `go clean -cache` if you need that.
	rm -rf $(BIN_DIR)

version: ## Print the version string that build would embed.
	@# Useful when reproducing a CI build locally, or when verifying that a
	@# tagged build picked up the expected git SHA.
	@echo "VERSION=$(VERSION)"
	@echo "GIT_SHA=$(GIT_SHA)"

# ---------------------------------------------------------------------------
# STUB targets: these are wired up here so the contract is visible from
# Phase 0, but the implementation arrives in later phases (see plan.md
# Section 14). Each prints a TODO marker and exits 0 so CI can call them
# without failing the build before the feature lands.
# ---------------------------------------------------------------------------

kind-up: ## (stub) Bring up a local kind cluster with gVisor preinstalled.
	@echo "TODO: filled by later phases"

kind-down: ## (stub) Tear down the local kind cluster.
	@echo "TODO: filled by later phases"

e2e: ## (stub) Run the Ginkgo e2e suite against the kind cluster.
	@echo "TODO: filled by later phases"
