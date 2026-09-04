# CLAUDE.md

Project-specific guidance for Claude Code (and any other AI coding assistant
that reads this file) when working in this repository. Read this first; it
is short on purpose.

For the human-facing project overview, see [README.md](README.md). For
contributor mechanics (PR workflow, commit style, etc.), see
[CONTRIBUTING.md](CONTRIBUTING.md). This file complements both, it does
not duplicate them.

## What this project is, in one paragraph

`agentmoat` is a Go CLI plus an MCP server that moves Kubernetes
workloads from `runc` to gVisor (`runsc`). The pipeline is
`scan -> plan -> apply -> rollback -> verify -> explain` (Phases 0-3,
shipped). The CLI binary and the MCP binary are intentionally thin shells
over a single library at `pkg/agentmoat`; both surfaces call the same
orchestrator functions (`Scan`, `Plan`, `Apply`, `Rollback`, `Verify`,
`Explain`) so they cannot drift. See
[`docs/architecture.md`](docs/architecture.md) for the diagram and the
package map.

## Canonical commands

The Makefile is the source of truth. If you would run a raw `go ...`
invocation more than twice, prefer extending a target.

| Command           | What it does                                                |
| ----------------- | ----------------------------------------------------------- |
| `make build`      | Builds `./bin/agentmoat` and `./bin/agentmoat-mcp` with version + git SHA injected via `-ldflags`. |
| `make test`       | `go test -race -cover ./...`. Race detector is non-negotiable here (concurrent K8s + MCP code). |
| `make lint`       | `golangci-lint run ./...` against [`.golangci.yml`](.golangci.yml). CI runs the same command. |
| `make tidy`       | `go mod tidy`. Run after any import change. |
| `make check-gvisor-version` | Asserts the gVisor release pin agrees across packer/kind/scripts and that all release artifacts exist. CI runs it on every PR. |
| `make check-gvisor-latest`  | Same, plus exit 2 when a newer gVisor release is published. The weekly `gvisor-drift.yml` workflow runs this and opens a bump PR; see `docs/gvisor-version.md`. |
| `make kind-up`    | Builds the gVisor-enabled kind worker image and creates the local cluster. Idempotent; an existing cluster is reused as is, so run `make kind-down` first after a gVisor pin bump. |
| `make e2e`        | Builds, brings up kind, runs scan/plan/apply/rollback end-to-end against a real `runsc` runtime, tears down. Honors `KEEP_CLUSTER=1`. |
| `make kind-down`  | Tear down the kind cluster from `make kind-up`. |

CI (`.github/workflows/ci.yml`) gates on `make lint` and `make test`. Keep
them green locally before pushing.

## Repository layout (where to look)

- `cmd/agentmoat/` and `cmd/agentmoat-mcp/`: thin shells. CLI uses Cobra;
  the MCP binary serves 7 tools over stdio (Phase 4, shipped). Add new
  subcommands here as `<verb>.go` next to the existing files; global flags
  live in `root.go`.
- `pkg/agentmoat/`: orchestrator. One public function per verb
  (`Scan`, `Plan`, `Apply`, `Rollback`, ...). Both binaries call these.
- `pkg/scanner/`: cluster enumeration via `client-go`. Returns canonical
  `[]scanner.Workload`.
- `pkg/classifier/`: pure-function rule engine. `Classify(w, registry)`
  returns a `Verdict`. Built-in rules in `builtin_rules.go`; YAML
  overrides in `internal/rules/gvisor.yaml`.
- `pkg/planner/`: pure function from `ScanReport` to `MigrationPlan`.
  Same scan in, same plan and same SHA-256 `planHash` out. Don't break
  determinism.
- `pkg/applier/`: the only package that mutates the cluster. Strategic
  merge patch with `runtimeClassName`, namespace plan-hash annotation,
  event emission, audit log append.
- `pkg/verifier/` and `pkg/explainer/`: Phase 3 (shipped). `verify` checks
  that live pods match the plan; `explain` renders the embedded docs and
  per-workload evidence.
- `pkg/output/`: table / JSON / YAML renderers. The renderer dispatches
  on concrete type with a switch; extend it when adding a new schema type.
- `internal/schema/`: versioned wire types (`agentmoat.io/v1alpha1`).
  This is the public-ish surface for JSON/YAML output. Touch carefully.
- `internal/kube/`: client-go construction (kubeconfig + in-cluster).
- `internal/audit/`: `~/.agentmoat/audit.jsonl` append-only log.
- `docs/`: public docs. Some are stubs marked as such.
- `kind/`, `packer/`, `deploy/`, `scripts/`: cluster + AMI plumbing.
- `test/e2e/`: e2e harness invoked by `scripts/e2e.sh`.

## Architectural invariants (do not break)

1. **`scan` and `plan` never mutate.** They must be safe to run against
   production from a read-only kubeconfig.
2. **`apply` and `rollback` default to `--dry-run=true`.** A user must
   explicitly pass `--dry-run=false` to mutate. If you add a mutating
   command, follow the same convention.
3. **`MigrationPlan` is deterministic.** Same `ScanReport` in, same
   `planHash` out. The planner is a pure function. Don't introduce wall
   clock or map iteration order into the plan.
4. **CLI and MCP are thin shells.** New logic goes in `pkg/agentmoat` (or
   a sub-package), not in `cmd/`. The two binaries must keep calling
   identical functions.
5. **No `kubectl` shell-out.** Everything goes through typed `client-go`.
6. **Idempotency.** `apply` writes `agentmoat.io/plan-hash` on the
   namespace; re-running an applied plan reports every step as
   `already-applied` and exits 0. Preserve this on any new apply path.
7. **Deterministic exit codes.** Documented in
   [`docs/exit-codes.md`](docs/exit-codes.md). New commands pick from the
   existing codes (0/1/2/3/4) or add a new one to that table, do not
   silently invent codes.
8. **Schema stability.** `agentmoat.io/v1alpha1` is the active wire
   version. Add fields, do not rename or remove without bumping. Update
   `internal/schema/types_test.go` whenever shapes change.

## Conventions (mirror the existing code)

- **Generous comments preferred.** This is an educational project. Every
  file gets a header comment; non-obvious code gets a "why" comment. A
  K8s expert new to gVisor should be able to follow.
- **Conventional Commits.** `feat:`, `fix:`, `docs:`, `refactor:`,
  `test:`, `chore:`, optional scope (e.g. `feat(scanner): ...`).
- **`gofmt` + `goimports`** are non-negotiable. CI fails otherwise.
  `golangci-lint` (config in `.golangci.yml`) is the source of truth for
  non-style issues.
- **Table-driven tests** preferred over golden files. Fixtures belong in
  `test/testdata/`. Classifier and planner tests must be deterministic:
  no cluster state, no wall clock.
- **Stable identifiers.** Rule IDs (`raw-socket`, `host-network`, ...)
  are a public surface; they appear in JSON output, in `--rules`
  overrides, and in [`docs/compatibility-checklist.md`](docs/compatibility-checklist.md).
  Don't rename without a deprecation path.

## Phase status (as of last update)

- **Phase 0** (Foundation), **Phase 1** (read-only scan + classifier),
  **Phase 2** (planner + applier + rollback): shipped and exercised by
  `make e2e`.
- **Phase 3** (verifier + explainer): shipped. `verify` (pod
  `runtimeClassName` check, optional `--in-pod-probe`) and `explain`
  (embedded docs plus `explain namespace` / `explain workload` deep scans)
  are wired in `cmd/agentmoat/` and exercised by `make e2e`.
- **Phase 4** (MCP server): shipped. `cmd/agentmoat-mcp/` serves 7 tools
  over stdio (`scan_cluster`, `assess_workload`, `propose_plan`,
  `apply_plan`, `rollback_plan`, `verify_migration`, `explain`). See
  [`docs/mcp-integration.md`](docs/mcp-integration.md).
- **Phase 5+**: EKS recipe, additional Packer variants.

When the phase shifts, update this section.

## Things to do (and not do) by default

- **Do** run `make lint` and `make test` after non-trivial edits.
  Run `make e2e` after changes to scanner / planner / applier / verifier
  (it is the only check that exercises the real client-go path against a
  cluster).
- **Do** ask before mutating a real cluster from any command other than
  `apply --dry-run=false` and `rollback --dry-run=false`. The dry-run
  default is load-bearing.
- **Don't** push to `origin` without being asked. Local branches and
  commits are fine; pushing publishes them.
- **Don't** add a new top-level dependency casually. `go.mod` is short
  on purpose. If you need a new third-party package, raise it first.
- **Don't** introduce wall-clock or random data into the planner; the
  hash determinism guarantee depends on it.

## When in doubt

Read the relevant doc:

- Architecture and package map: [`docs/architecture.md`](docs/architecture.md)
- Rule catalog and `--rules` override schema: [`docs/compatibility-checklist.md`](docs/compatibility-checklist.md)
- Exit codes: [`docs/exit-codes.md`](docs/exit-codes.md)
- gVisor primer: [`docs/gvisor-101.md`](docs/gvisor-101.md)
- RuntimeClass primer: [`docs/runtimeclass-101.md`](docs/runtimeclass-101.md)
- Threat model and CVE backdrop: [`docs/threat-model.md`](docs/threat-model.md)
- Local kind quickstart: [`docs/kind-quickstart.md`](docs/kind-quickstart.md)

If a doc is missing or stale, fix it as part of the same PR rather than
filing a follow-up. Future-you will thank present-you.
