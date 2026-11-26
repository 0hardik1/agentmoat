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
`scan -> preflight -> plan -> apply -> rollback -> verify -> explain`
(Phases 0-3, shipped), plus `probe nvproxy` for GPU clusters, and `patch`,
which makes the same move as a change to manifest files (for a repository
that a pipeline deploys from; no cluster). The CLI binary
and the MCP binary are intentionally thin shells over a single library at
`pkg/agentmoat`; both surfaces call the same orchestrator functions (`Scan`,
`Preflight`, `Probe`, `Plan`, `Patch`, `Apply`, `Rollback`, `Verify`,
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
| `make release-snapshot` | Runs GoReleaser in snapshot mode: every release archive, the Homebrew and krew manifests, and the two container images, built locally. Publishes nothing. Needs Docker. Run it after any change to `.goreleaser.yaml`, `Dockerfile`, or `Dockerfile.alpine`. |
| `make check-gvisor-version` | Asserts the gVisor release pin agrees across packer/kind/scripts and that all release artifacts exist. CI runs it on every PR. |
| `make check-gvisor-latest`  | Same, plus exit 2 when a newer gVisor release is published. The weekly `gvisor-drift.yml` workflow runs this and opens (or updates) a `gvisor-drift` issue when the pin is behind; the run fails only on a real error, and it does not open a PR. See `docs/gvisor-version.md`. |
| `make check-device-cgroup` | Checks that every runsc sandbox on a node is confined to a device cgroup (gVisor 20261116.0 and newer): `/dev/null` opens, `/dev/kmsg` is refused. `NODE=<kind node>` runs it through `docker exec`; `make e2e` runs it on the kind worker. |
| `make kind-up`    | Builds the gVisor-enabled kind worker image and creates the local cluster. Idempotent; an existing cluster is reused as is, so run `make kind-down` first after a gVisor pin bump. |
| `make e2e`        | Builds, brings up kind, runs scan/plan/apply/verify/rollback end-to-end against a real `runsc` runtime, and the move through files (`patch`, a deploy, `verify -f`), tears down. Honors `KEEP_CLUSTER=1`. |
| `make offline-smoke` | Builds, then runs `scan -f`, the `--fail-on` gate, the Markdown and SARIF outputs, and `patch` on the manifest fixtures in `test/testdata/manifests` and `test/testdata/patch` with no cluster. For `patch --kustomize` it builds the component with `kubectl kustomize` (and `helm template`) when the tools are installed. Then it runs the script of the GitHub Action (`scripts/action-smoke.sh`) with a model of the runner. Needs `jq`, `curl`, and `tar`. CI runs it on every PR. Run it after changes to the manifest scanner, the gate, `pkg/patcher`, `pkg/output`, `action.yml`, or `scripts/github-action.sh`. |
| `make kind-down`  | Tear down the kind cluster from `make kind-up`. |

CI (`.github/workflows/ci.yml`) gates on `make lint`, `make test`, and
`make offline-smoke`. Keep them green locally before pushing.

## Repository layout (where to look)

- `cmd/agentmoat/` and `cmd/agentmoat-mcp/`: thin shells. CLI uses Cobra;
  the MCP binary serves 11 tools over stdio (Phase 4, shipped). Add new
  subcommands here as `<verb>.go` next to the existing files; global flags
  live in `root.go`.
- `pkg/agentmoat/`: orchestrator. One public function per verb
  (`Scan`, `Plan`, `Apply`, `Rollback`, ...). Both binaries call these.
  `Scan` has two sources: a cluster, or manifest files
  (`scan_manifests.go`, `scan -f`). `gate.go` holds the `--fail-on` decision
  (`EvaluateGate`), so the exit code logic is not in `cmd/`. `managed.go`
  reads the markers of deploy tools (Argo CD, Flux, Helm) into
  `managedBy`, and makes the `source-managed` plan warning from them.
  `patch.go` is `Patch`: it reads manifests as the file scan does
  (`readManifestSet`), selects workloads with the rule of the planner, calls
  `pkg/patcher` for each file, and writes the files (`writeManifest`: the
  file must be as it was read, then a temporary file and a rename).
  `PatchExitCode` is the exit code. `patch_kustomize.go` is the form with
  `--kustomize <dir>`: it reads the directory (`readKustomizeDir`, which
  refuses a directory with files of a person), plans the patch files and
  the list, and writes the patch files first and the list last.
  `verify_manifests.go` is the source of `verify -f`: it makes the list of
  workloads to check from manifest files (each workload that asks for the
  class), and `VerifyExitCode` in `verify.go` is the exit code of a report.
- `pkg/scanner/`: returns canonical `[]scanner.Workload` from one of two
  sources. `Enumerate` lists a cluster through `client-go`. `FromManifests`
  decodes YAML or JSON text with no cluster (`ReadManifestPaths` reads the
  files, directories, or stdin), and also accepts other kinds that carry a
  pod template, such as an Argo `Rollout`. Each workload from a manifest has
  its position (`ManifestOrigin`: file, document, and the number of the
  object in the document); `pkg/patcher` finds the object again by that
  position, so keep the reading order of `readValue` and the order in
  `patcher.collectObjects` the same. Each one also has its identity as the
  file has it (`ManifestResult.Written`, a `ManifestObject`: apiVersion,
  kind, name, namespace, the path of the pod spec, and `spec.selector`),
  which is not the same as the names in a report; the Kustomize patches and
  `verify -f` use it.
- `pkg/classifier/`: pure-function rule engine. `Classify(w, registry)`
  returns a `Verdict`; `ClassifyWithFacts` adds `schema.ClusterFacts` so a
  rule's `Refine` hook can move its severity (`gpu-passthrough` in
  `gpu_refine.go`, `systemd-init` in `systemd_init.go`). Built-in rules in
  `builtin_rules.go`; YAML overrides in `internal/rules/gvisor.yaml`.
- `pkg/preflight/`: reads one RuntimeClass and the node list into
  `schema.ClusterFacts` (`Collect`), turns facts into findings with stable
  IDs (`Evaluate`, pure), and wraps both (`Run`). `apply` calls it as a gate
  and refuses to run on an error finding (exit 5); `scan` stores the facts
  and `plan` derives warnings from them. `gpu.go` adds the GPU inventory
  from GPU Feature Discovery labels. `sandbox.go` lists the pods on nodes
  that run sandboxed pods (gVisor or EKS Nitro Sandboxes) and warns when
  runc workloads share them; a failed pod list degrades to an info finding.
  See `docs/preflight.md`.
- `pkg/probe/`: the nvproxy probe. Creates one pod on a gVisor node that
  runs the host `runsc` (hostPath, read-only, non-root, under runc), parses
  `runsc --version` and `runsc nvproxy list-supported-drivers`, deletes the
  pod. When that `runsc` needs its sidecars (gVisor 20261019.0 and newer), a
  second pod also mounts `gvisor-bin/` read-only. Dry-run by default. Output
  is a `PreflightReport`; `scan --facts` reads it. See `docs/gpu-nvproxy.md`.
- `pkg/seccompprobe/`: the seccomp probe (`probe seccomp`). Runs each
  migrated workload once as a trial pod on the `gvisor-seccomp-audit`
  RuntimeClass (runsc with oci-seccomp on and the Sentry debug log on),
  deletes it, then reads the blocked syscalls from the node's log with a
  runc reader pod (read-only hostPath, uid 65534). Dry-run by default.
  Output is a `PreflightReport` with `spec.facts.seccomp`; the findings are
  in `pkg/preflight/seccomp.go`. See `docs/preflight.md#seccomp-probe`.
- `pkg/planner/`: pure function from `ScanReport` to `MigrationPlan`.
  Same scan in, same plan and same SHA-256 `planHash` out. Don't break
  determinism. A step is a change: a workload that already has a
  RuntimeClass gets no step (`runtimeClassBlocker`), because `rollback`
  undoes a plan by removing the class. `ExclusionCause` is the selection
  rule; `patch` uses it too, so the two verbs select the same workloads
  (only the kind blocker for a live Pod or Job is for `plan` alone).
- `pkg/patcher/`: the text edit of `patch`. Pure: YAML text and targets in,
  new text out. `AddRuntimeClass` adds one line after the `spec:` key of the
  pod spec; the YAML node tree gives only the line and the column, and the
  text is never encoded again from objects. `check.go` proves each edit
  before it is used (the new text decodes to the old objects plus the
  class, and the scanner finds the class on each target). `diff.go` makes
  the unified diff from the added line numbers, and the diff of a file that
  is new or longer (`FileDiff`). `kustomize.go` is the form that edits no
  source: the text of one strategic merge patch for an object
  (`KustomizePatch`), its file name, and the `kustomization.yaml` of kind
  `Component` (`ComponentAdd`). The rules for how Kustomize finds the target
  of a patch are in its header; `scripts/offline-smoke.sh` builds real trees
  to keep them true.
- `pkg/applier/`: the only package that mutates the cluster. Strategic
  merge patch with `runtimeClassName`, the two namespace annotations
  (`idempotency.go`: `plan-hash` when an apply is complete, `apply-started`
  before its first patch), event emission, audit log append.
- `pkg/verifier/` and `pkg/explainer/`: Phase 3 (shipped). `verify` checks
  that live pods match the plan and run on nodes the RuntimeClass
  `nodeSelector` selects; `explain` renders the embedded docs and
  per-workload evidence. In the verifier, `target.go` reads one workload
  (its pods, its pod template, and whether it wants pods now), `wait.go` is
  the `--wait` loop, and `readiness.go` says why a pod is not Ready. The
  list of what to check is a plan or `Options.Targets` (`verify -f`). A
  target of a kind with no reader has the selector of its manifest, and the
  verifier lists pods by it. A target with no verdict (that selector has no
  pods, or a Job or a Pod from a manifest is not in the cluster) goes to
  `spec.notChecked` and never to the results.
- `pkg/output/`: the renderers. Table, JSON, and YAML for every document;
  Markdown (`markdown.go`, `markdown_pipeline.go`) for a CI job summary or a
  pull-request comment; SARIF (`sarif.go`) for `scan` only. `patch.go` has
  the two renderers of a `PatchResult`, with the diff. The renderer
  dispatches on concrete type with a switch; extend it when adding a new
  schema type. `formatKinds` in `renderer.go` is the list of which document
  takes which format, and each command checks it first with `ParseFor`. See
  `docs/output-formats.md`.
- `internal/schema/`: versioned wire types (`agentmoat.io/v1alpha1`).
  This is the public-ish surface for JSON/YAML output. Touch carefully.
- `internal/kube/`: client-go construction (kubeconfig + in-cluster).
- `internal/audit/`: `~/.agentmoat/audit.jsonl` append-only log.
- `docs/`: public docs. Some are stubs marked as such. It is also a Go
  package: `embed.go` embeds the files, and `rules.go` gives the title and
  the explanation of each rule (`RuleTitle`, `RuleExplanation`) to `explain`
  and to the SARIF output. A new rule needs a title there and a file in
  `docs/explanations/`; a test in `docs/rules_test.go` checks it.
- `kind/`, `packer/`, `deploy/`, `scripts/`: cluster + AMI plumbing.
- `Dockerfile`, `Dockerfile.alpine`, `.goreleaser.yaml`: the release. Each
  tag publishes the archives and two images to `ghcr.io/0hardik1/agentmoat`:
  the default one (distroless, no shell, non-root) and `-alpine` (has a
  shell, for CI systems that run a script in the job image). The Dockerfiles
  only copy prebuilt binaries; GoReleaser builds them.
- `action.yml`, `scripts/github-action.sh`: the GitHub Action (a pull
  request check: `scan -f` with the `--fail-on` gate, no cluster).
  `action.yml` is only the interface; the script has the logic and its five
  rules in the header. `scripts/action-smoke.sh` tests it with a model of
  the runner, and `.github/workflows/action-test.yml` runs it on real
  runners when the action changes.
- `examples/ci/`: the pipeline files that users copy (the GitHub pull
  request check, the GitHub migration pipeline, the GitHub workflow that
  runs `patch` and opens a pull request, GitLab CI). `docs/ci-cd.md`
  describes them and says which parts a test proves.
- `test/e2e/`: e2e harness invoked by `scripts/e2e.sh`.

## Architectural invariants (do not break)

1. **`scan` and `plan` never mutate.** They must be safe to run against
   production from a read-only kubeconfig.
2. **`apply` and `rollback` default to `--dry-run=true`.** A user must
   explicitly pass `--dry-run=false` to mutate. If you add a mutating
   command, follow the same convention. `patch` follows it for the files
   that it writes, and the MCP tool `patch_manifests` does too.
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
   The stamp is written only when every step in the namespace is done.
   **Record first, then mutate:** before its first patch in a namespace,
   `apply` writes `agentmoat.io/apply-started`, and it patches nothing when
   that write fails. `rollback` acts only where one of the two annotations
   has the hash of its plan, never from the workloads alone, so it can undo
   an apply that did not finish and it changes nothing for a plan that was
   never applied. The annotations hold one plan for a namespace: an apply
   replaces the annotation of an older plan in the same write that sets
   `apply-started` (a stamp that stays could become false), and a rollback
   removes only the annotations of its own plan. A plan with no `planHash`
   cannot be recorded, so `apply` and `rollback` refuse it
   (`applier.CheckPlan`).
7. **Deterministic exit codes.** Documented in
   [`docs/exit-codes.md`](docs/exit-codes.md). New commands pick from the
   existing codes (0/1/2/3/4/5) or add a new one to that table, do not
   silently invent codes.
8. **Schema stability.** `agentmoat.io/v1alpha1` is the active wire
   version. Add fields, do not rename or remove without bumping. Update
   `internal/schema/types_test.go` whenever shapes change.
9. **`scan -f` and `patch` read no cluster.** With manifest paths, `Scan`
   builds no Kubernetes client and reads no kubeconfig. `Patch` has no
   option for a client or a kubeconfig at all; keep it so. Do not use a fake client
   either: the report must not name a cluster that it did not read. A test
   in `pkg/agentmoat/scan_manifests_test.go` sets a kubeconfig that points
   at a dead server and a client that fails on any call. `verify -f` is not
   in this rule: it reads the files with the same code and then reads the
   cluster, which is its job. It reads and refuses the input before it
   builds the client (a test in `pkg/agentmoat/verify_manifests_test.go`).
10. **A file scan fails closed.** Exit 0 from `scan -f` means the input was
    read and nothing in it failed the gate. A document that does not parse
    is an error, not a skip. A workload kind with no containers (an overlay
    patch) is a partial object, never a compatible workload. `--fail-on
    sandboxed` refuses input that is not rendered. Do not weaken these to
    make a scan pass. `patch` reads with the same code, so one document that
    does not parse stops the run before a file is written.
11. **Pipeline output states the gate, and trusts no text from the input.**
    A job summary shows no stderr and no exit code, so `scan -o markdown`
    starts with the gate result, for every outcome (a gate that checked
    nothing or could not answer is not a pass). A SARIF result has the level
    `error` only when its workload makes the gate fail, so an upload is not
    a second gate. Every string that comes from a manifest (a name, a kind,
    a path, a description) goes through `mdCode`, `mdCodeCell`, `mdText`, or
    a fenced block; a pull request must not be able to add structure, a
    link, or a mention to its own report. The size has a limit for any
    input (a job summary on GitHub takes 1 MiB); a test in
    `pkg/output/markdown_test.go` checks it. The GitHub Action has the same
    two rules for the runner: every way out that is not exit 0 writes to the
    job summary first, and the step log is a command channel (the runner
    runs a line that starts with `::` or has `##[` in it), so the script
    prints the output of a tool only inside a `stop-commands` block and
    makes each annotation with the encoding of the runner.
    `scripts/action-smoke.sh` checks it with hostile names.
12. **Exit 4 from `verify --wait` is a rollback signal.** With a wait time,
    a step is ok only when all its pods have the class, are Ready, and
    stayed Ready for the settle time (the same pods, no restart). One look
    that finds a pod Ready is not sufficient: a pod in a crash loop is Ready
    for a moment in each restart. The wait time is checked between looks and
    is never a deadline on the API calls, so a report after a timeout has
    the reason for each step. A workload that wants no pods now is checked
    on its pod template and does not use the wait time (the exception: a
    Job with the condition Failed is a mismatch with a wait time, and is
    looked at again like each other step that is not ok). Without `--wait`,
    verify looks one time and does not read readiness. The CLI installs no
    signal handler: an interrupt must end the process by the signal, so
    that Ctrl-C on `verify --wait 3m || rollback --dry-run=false` does not
    start a rollback (a test in `cmd/agentmoat/verify_test.go` checks it).
    For `verify -f` the same signal has one more rule: exit 4 comes only
    from what the cluster shows. Where the files do not name what is in the
    cluster, verify does not guess. It refuses the run before a cluster read
    (input that is not rendered, a workload with no namespace, no workload
    that asks for the class), or it lists the one workload as not checked
    (an object with `generateName`, a kind with no reader and no selector,
    a selector with no pods, a Job or a bare Pod that is not in the
    cluster: a time to live or a hook delete policy removes those after a
    deploy that worked). Each of the two is exit 1, and the report says
    "not a pass" in words. `status` keeps its three values. A pod that ran
    to its end is checked on its class only, never on readiness.
    `agentmoat.VerifyExitCode` is the one place for this code.
13. **`patch` adds one line for each workload and changes nothing else.**
    The edit is a text edit: comments, key order, quotes, indent, and line
    endings stay. Do not encode YAML from objects to write a file. A form
    that the edit cannot do safely (JSON, flow style, an anchor or alias on
    the way to the pod spec) is reported as `not-edited` with the reason,
    never guessed. The check in `pkg/patcher/check.go` runs on every edit at
    run time and is not only a test: an edit that does not pass is not
    written. A second run changes nothing. The diff is what a reviewer
    approves, so it must be exact (`git apply` reads it in the tests). A
    line of a diff is text from a manifest. The Markdown renderer puts it in
    a fenced block with limits (invariant 11). The table renderer replaces
    control characters, so that a line cannot write over the report on a
    terminal, and does no more: in a GitHub step log a line of the table or
    of the diff can still be a runner command, so a pipeline sends the
    Markdown to the job summary, or prints inside a `stop-commands` block.
    With `--kustomize <dir>` the rule is: no source file changes, and a run
    only adds. It writes patch files that are not there, and entries at the
    end of its own list (`kustomization.yaml`, kind `Component`, with the
    marker line first). It changes no file that is there, and it refuses a
    directory that has files of a person before it writes. It does not edit
    the kustomization of a person, and it does not run Kustomize: it cannot
    know which kustomization will use the component, so no output of it
    says that a build passes.

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
  `runtimeClassName` check, optional `--in-pod-probe`, `--wait` for a
  pipeline that runs it directly after `apply`, `-f` to check the workloads
  of manifest files with no plan) and `explain`
  (embedded docs plus `explain namespace` / `explain workload` deep scans)
  are wired in `cmd/agentmoat/` and exercised by `make e2e`.
- **Phase 4** (MCP server): shipped. `cmd/agentmoat-mcp/` serves 11 tools
  over stdio (`scan_cluster`, `preflight_cluster`, `probe_nvproxy`,
  `probe_seccomp`,
  `assess_workload`, `propose_plan`, `patch_manifests`, `apply_plan`,
  `rollback_plan`, `verify_migration`, `explain`). See
  [`docs/mcp-integration.md`](docs/mcp-integration.md).
- **Phase 5+**: EKS recipe, additional Packer variants.

When the phase shifts, update this section.

## Things to do (and not do) by default

- **Do** run `make lint` and `make test` after non-trivial edits.
  Run `make e2e` after changes to scanner / planner / applier / verifier
  (it is the only check that exercises the real client-go path against a
  cluster).
- **Do** ask before mutating a real cluster from any command other than
  `apply --dry-run=false`, `rollback --dry-run=false`, and
  `probe nvproxy --dry-run=false`, and `probe seccomp --dry-run=false`.
  The dry-run default is load-bearing.
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
- Pipelines (the GitHub Action, the migration pipeline, the move through the repository with `patch`, what `rollback` does in each case): [`docs/ci-cd.md`](docs/ci-cd.md)
- Exit codes: [`docs/exit-codes.md`](docs/exit-codes.md)
- Output formats (Markdown, SARIF, the gate line): [`docs/output-formats.md`](docs/output-formats.md)
- Preflight findings and the apply gate: [`docs/preflight.md`](docs/preflight.md)
- GPU workloads, nvproxy support, the driver probe: [`docs/gpu-nvproxy.md`](docs/gpu-nvproxy.md)
- gVisor primer: [`docs/gvisor-101.md`](docs/gvisor-101.md)
- RuntimeClass primer: [`docs/runtimeclass-101.md`](docs/runtimeclass-101.md)
- Threat model and CVE backdrop: [`docs/threat-model.md`](docs/threat-model.md)
- Local kind quickstart: [`docs/kind-quickstart.md`](docs/kind-quickstart.md)

If a doc is missing or stale, fix it as part of the same PR rather than
filing a follow-up. Future-you will thank present-you.
