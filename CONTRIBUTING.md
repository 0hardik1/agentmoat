# Contributing to agentmoat

Thanks for your interest in agentmoat. This is an educational, proof-of-concept
project that aims to make gVisor adoption on EKS (and any K8s) tractable. PRs,
issues, and design discussions are all welcome.

## Dev setup

Requirements:

- **Go 1.26 or newer.** (Matches the `go` directive in `go.mod`.)
- **Docker** (for running the kind-based e2e tests).
- **Packer 1.10+** (only if you are working on the AMI build).

Common workflow:

```bash
# Compile the CLI and the MCP server.
make build

# Run unit tests.
make test

# Run the linters configured in .golangci.yml.
make lint

# Verify the pinned gVisor release (three files must agree; artifacts must exist).
make check-gvisor-version
```

The Makefile targets are the source of truth: if you find yourself running a
raw `go test` invocation more than twice, please add or extend a target.

## Project layout

The high-level layout (`cmd/`, `pkg/`, `internal/`, `packer/`, `deploy/`,
`kind/`, `docs/`, `test/`) is summarized in the [README](README.md) and in
[`CLAUDE.md`](CLAUDE.md). The short version:

- `cmd/agentmoat/` and `cmd/agentmoat-mcp/` are thin shells.
- `pkg/agentmoat/` (with `scanner/`, `classifier/`, `planner/`, `applier/`,
  `verifier/`, `explainer/`, `output/`) is the importable core library.
- `internal/` holds private helpers (kube client wrappers, containerd parsers,
  schema generation).
- `docs/` is the public documentation tree (educational, intended to be linked
  from README and from the `agentmoat explain` subcommand).

## PR workflow

1. **Branch or fork.** External contributors fork; maintainers can push
   feature branches directly. Target `main`.
2. **Open a draft PR early** if you want feedback on direction before
   investing in tests.
3. **Conventional Commits** for commit messages. Use one of: `feat:`, `fix:`,
   `docs:`, `refactor:`, `test:`, `chore:`. Scope is optional but encouraged
   (e.g., `feat(scanner): support DaemonSet enumeration`).
4. **DCO sign-off is optional** at this stage (this may change before the
   first tagged release). If you do sign off, append `Signed-off-by:` lines
   via `git commit -s`.
5. **Pass CI.** Lint, unit tests, and (eventually) the kind e2e suite must be
   green before merge.

## Code style

- **`gofmt` and `goimports`** are non-negotiable. CI will fail the build
  otherwise.
- **`golangci-lint`** with the linters enabled in `.golangci.yml` is the
  source of truth for non-style issues.
- **Generous comments preferred.** This is an educational project. If a
  function exists because of a non-obvious K8s, containerd, or gVisor
  behavior, write the comment that explains *why*, not just *what*. A reader
  who is a K8s expert but new to gVisor should be able to follow the code.
- **Avoid em dashes.** Use colons, commas, parentheses, or split into two
  sentences. En dashes and hyphens are fine.

## Testing

- **Unit tests are required for every new package.** Aim for table-driven
  tests over golden files; both are acceptable.
- **`testdata/`** is the place for fixture YAML, Pod specs, and golden
  outputs. Keep fixtures minimal and commented.
- **E2E tests on kind** run via `make e2e` (the harness under `test/e2e/`
  driven by `scripts/e2e.sh`). Every new mutating operation should get an
  e2e case.
- **Determinism.** Classifier rules are pure functions over `PodSpec` and OCI
  image labels. Tests must not depend on cluster state or wall-clock time.

## Questions

Open an issue with the `question` label. If your question is about gVisor
itself rather than agentmoat, [gvisor.dev](https://gvisor.dev/) and the gVisor
GitHub Discussions are usually faster.
