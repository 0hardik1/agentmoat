# MCP integration

`agentmoat-mcp` is the Model Context Protocol server for the agentmoat
toolkit. It speaks JSON-RPC over stdio per the
[MCP specification](https://modelcontextprotocol.io/), and is a thin
shell over the same `pkg/agentmoat` orchestrator library that powers
the CLI: every MCP tool maps one-to-one to a CLI verb, so the human
operator surface and the LLM-agent surface cannot drift.

## 1. What MCP is, in two sentences

MCP is a JSON-RPC protocol that lets an LLM client (Claude Code, Cursor,
Continue) talk to a local subprocess that exposes tools, resources,
and prompts. The MCP server is just a binary the client launches with
`exec`, hands a stdio pair to, and drives with JSON-RPC requests.

## 2. Sibling-binary architecture (why agentmoat-mcp is its own binary)

The agentmoat repo ships two binaries built from the same module:

- `agentmoat` (the CLI): the human-facing surface. Cobra-based commands,
  table/JSON/YAML output, color-aware.
- `agentmoat-mcp`: the MCP-over-stdio server. Same orchestrator calls;
  outputs JSON via the protocol's tool-result envelope.

Both call `pkg/agentmoat.Scan`, `Plan`, `Apply`, `Rollback`, `Verify`,
`Explain`, and the new single-workload `AssessWorkload` introduced in
Phase 4. The CLI's `--output json` and the MCP tool-result body are
byte-identical: same Go struct, same `json:` tags, same APIVersion
(`agentmoat.io/v1alpha1`).

## 3. Tools, resources, prompts

### Tools (10)

| Tool                | Orchestrator                  | Mutating | Notes                                                                |
|---------------------|-------------------------------|----------|----------------------------------------------------------------------|
| `scan_cluster`      | `agentmoat.Scan`              | no       | Returns a `ScanReport` (with `metadata.clusterFacts`; `no_cluster_facts` skips them, `facts_path` loads them from a saved report). With `manifest_paths` it reads manifest files or directories and contacts no cluster (the `scan -f` form): use it to check a repository. Standard input is not available; pass rendered output as a file. |
| `preflight_cluster` | `agentmoat.Preflight`         | no       | Returns a `PreflightReport`. `spec.summary.ready=false` means `apply_plan` will refuse to run. |
| `probe_nvproxy`     | `agentmoat.Probe`             | YES      | Creates and deletes one pod on a gVisor node to read the `runsc` nvproxy driver list (a second pod when that `runsc` needs its sidecar binaries). Defaults to dry-run; `"dry_run": false` runs it. Returns a `PreflightReport` with `spec.facts.gpu.nvproxy`. |
| `assess_workload`   | `agentmoat.AssessWorkload`    | no       | Returns a `WorkloadResult` for one named workload. Accepts `facts_path`. |
| `propose_plan`      | `agentmoat.Plan`              | no       | Same scan in -> same `planHash`. Accepts `scan_report_path` to reuse a saved scan, `facts_path` for the facts, `add_toleration` for the pod toleration. |
| `patch_manifests`   | `agentmoat.Patch`             | YES (files) | Adds `runtimeClassName` to the manifest files of the workloads that can run in gVisor (the `patch` command), so that the move goes through a pull request. Contacts no cluster. Defaults to dry-run and returns a `PatchResult` with the unified diff of each file; `"dry_run": false` writes the files. With `kustomize_dir` it edits no source file and writes the change into that directory as a Kustomize component (the `--kustomize` form). |
| `apply_plan`        | `agentmoat.Apply`             | YES      | Defaults to dry-run. Must set `"dry_run": false` explicitly to mutate. Runs the preflight first; `skip_preflight` bypasses it. |
| `rollback_plan`     | `agentmoat.Rollback`          | YES      | Same dry-run gate as `apply_plan`.                                   |
| `verify_migration`  | `agentmoat.Verify`            | no       | Takes one source: `plan_path` (the steps of a plan, after `apply_plan`), or `manifest_paths` (the `verify -f` form, after a change that came through a repository: the tool checks each workload of the files that asks for the RuntimeClass). With `manifest_paths`, `namespace` is the namespace of the objects that name none, and a report with `spec.summary.notChecked` above 0 is not a pass (`spec.notChecked` has the workloads and the reasons; a tool result has no exit code). Standard input is not available. Set `in_pod_probe=true` to also exec a /proc-and-uname probe. Set `wait` (a duration such as `"2m"`) directly after `apply_plan`: the tool then looks again until every step is ok and its pods stay Ready, and the report has `metadata.wait`. The call does not return before that, so keep `wait` below the time limit that your client has for a tool call. |
| `explain`           | `agentmoat.Explain`           | no       | Offline; reads the embedded docs.                                    |

**Safety gate.** The mutating tools (`apply_plan`, `rollback_plan`,
`probe_nvproxy`, and `patch_manifests`) inspect the raw JSON-RPC arguments for the `dry_run` field. The handler
distinguishes "field omitted" from "field set to `false`":

- field absent OR `"dry_run": true` -> dry-run (no cluster mutation, no file written).
- `"dry_run": false` (explicit) -> mutation allowed.

A non-boolean `dry_run` value is rejected with a structured tool-result
error. This mirrors the CLI's load-bearing `--dry-run=true` default.

**Preflight gate.** `apply_plan` runs `agentmoat.Preflight` before its
first step, in dry-run too. On an error finding it returns a normal
`ApplyResult` (not a tool error) with `metadata.preflight.ready: false`,
every step `skipped`, and the findings under `spec.preflightFindings`;
nothing is mutated. An agent should call `preflight_cluster` first, show the
operator the remediation text, and only pass `"skip_preflight": true` when
the operator has confirmed the cluster can host the RuntimeClass anyway.
See [`preflight.md`](preflight.md) for the finding ids.

**The move through a repository.** `patch_manifests` is the tool for an agent
that works in a repository: call it with `manifest_paths` as a dry-run, show
the person the diff from `spec.files[].diff`, and call it again with
`"dry_run": false` when the person agrees. The change is one added line for
each workload, and a second call changes nothing. `spec.changes` lists the
workloads that the tool could not edit (JSON, flow-style YAML, a pod spec
behind a YAML anchor) with the reason; make those edits by hand. Then call
`scan_cluster` with the same `manifest_paths` to check the result. The tool
does not commit and does not open a pull request. See
[`ci-cd.md`](ci-cd.md#3-the-move-through-the-repository).

With Kustomize, when one overlay must move and not each overlay of the base,
pass `kustomize_dir` (for example `overlays/staging/gvisor`). The tool then
edits no source file. It writes one patch file for each selected workload
and a `kustomization.yaml` of kind `Component` into that directory; each
change has `patchFile`. Two things are left for the agent or the person: add
the directory to `components:` in the kustomization that must move, and
build that kustomization. `manifest_paths` must be what that kustomization
reads from its `resources:` and nothing more, because a patch for an object
that it does not read stops its build, and the tool does not run Kustomize.
The CLI reads standard input in this form; the tool does not (its standard
input is the protocol channel), so pass rendered output as a file. See
[Kustomize](ci-cd.md#kustomize).

**GPU workloads.** `gpu-passthrough` is refined from cluster facts (card,
driver, MIG). The driver list lives only in the `runsc` binary, so the flow
is: `probe_nvproxy` with `"dry_run": false` (one pod at a time, created and deleted),
save the returned JSON to a file, then pass its path as `facts_path` to
`scan_cluster` / `assess_workload` / `propose_plan`. The verdict is then
`compatible` on supported hardware and `incompatible` on an unsupported
card, a MIG slice, or an unlisted driver. See [`gpu-nvproxy.md`](gpu-nvproxy.md).

### Resources (2, read-only)

| URI                                | Body                                          | MIME              |
|------------------------------------|-----------------------------------------------|-------------------|
| `agentmoat://compatibility-rules`  | `internal/rules/gvisor.yaml` (verbatim)       | `application/yaml` |
| `agentmoat://known-gotchas`        | `docs/compatibility-checklist.md` (verbatim)  | `text/markdown`    |

Both resources are embedded at build time, so they work in containers
and sandboxes with no filesystem layout dependency.

### Prompts (1)

- `audit-cluster-for-agentic-workloads`: a curated heuristic for
  identifying likely-agentic workloads (LLM-calling services, MCP
  servers, agent frameworks) and proposing a migration plan for them.
  Takes two optional arguments: `namespace` (restrict the audit) and
  `include_review` (whether to include `review`-class workloads in the
  proposed plan). The prompt body lists the image-name and env-var
  substrings the heuristic matches against; an operator can edit them
  in `cmd/agentmoat-mcp/prompts.go` without changing any Go logic.

## 4. Wiring into Claude Code

The easiest wiring is the `claude mcp add` command:

```bash
claude mcp add agentmoat -- /absolute/path/to/agentmoat-mcp
```

Alternatively, declare the server in a checked-in `.mcp.json` at the
project root (shared with your team) or in your user-level
`~/.claude.json`. All three accept the same `mcpServers` shape. (Claude
Desktop, which is the chat app and not Claude Code, uses
`claude_desktop_config.json` with the same entry format.)

```json
{
  "mcpServers": {
    "agentmoat": {
      "command": "/absolute/path/to/agentmoat-mcp",
      "args": [],
      "env": {
        "KUBECONFIG": "/Users/you/.kube/config"
      }
    }
  }
}
```

The `env` block is optional. Two environment variables are worth
knowing about:

- `KUBECONFIG` (standard): path to the kubeconfig the tools should use.
  Tools also accept a per-call `kubeconfig_path` argument that overrides
  this for one call.
- `AGENTMOAT_AUDIT_PATH`: override the default `~/.agentmoat/audit.jsonl`
  destination. Useful when the MCP server runs in a container or
  sandbox that does not have a writable HOME. Set to `/tmp/audit.jsonl`
  (or similar) to keep the audit trail out of the user's home dir.

After updating the config, restart Claude Code. The `agentmoat` server
appears in the tool palette; calls to `scan_cluster`, `propose_plan`,
etc. are then available to the model.

## 5. Worked session example

A minimal session: initialize, then call `scan_cluster` and
`propose_plan`. The full exchange (request + response, JSON-RPC) is
captured in `examples/mcp-session/`. The wire shape is:

```jsonrpc
// initialize
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"client","version":"0"}}}
// notifications/initialized (no response expected)
{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}
// scan_cluster
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"scan_cluster","arguments":{"namespaces":["my-namespace"]}}}
// propose_plan
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"propose_plan","arguments":{"namespaces":["my-namespace"]}}}
```

Each `tools/call` returns a `CallToolResult` whose
`content[0].text` is the JSON-encoded schema struct. The body parses
into the corresponding `schema.<Kind>` (`schema.ScanReport`,
`schema.MigrationPlan`, etc.) without any glue: the same Go types are
re-used between CLI and MCP. See `examples/mcp-session/` for the
full request and response pair.

## 6. Local smoke test

The repo ships `scripts/mcp-smoke.sh`, runnable via `make mcp-smoke`,
which drives the JSON-RPC session above against an existing kind
cluster. Pre-requisites:

```bash
make kind-up        # one-time: stand up the local kind cluster
make build          # produces ./bin/agentmoat-mcp
make mcp-smoke      # asserts scan_cluster -> ScanReport, propose_plan -> MigrationPlan
```

The script is intentionally **not** chained into `make e2e` (the CLI's
end-to-end harness): keeping them separate avoids coupling CLI
regressions to MCP regressions.

## 7. Other MCP clients (Cursor, Continue, etc.)

Any MCP-aware client that supports stdio servers works the same way:
point the client at the `agentmoat-mcp` binary and let it launch the
subprocess. Each client has its own config file (Cursor uses
`~/.cursor/mcp.json`, Continue uses `~/.continue/config.json`), but the
binary path + args are identical across them.

## 8. Troubleshooting

- **`agentmoat-mcp` exits immediately**: ensure the binary is
  executable (`chmod +x`), and confirm the client is launching it with
  no TTY attached. Run `./bin/agentmoat-mcp --help` interactively to
  confirm the binary itself works.
- **JSON-RPC requests appear to hang**: the stdio transport expects
  one message per line. If the client buffers without newlines, the
  server waits. Use `--log-level=debug` to surface the receive-side
  framing on stderr.
- **Tool calls fail with "building kubernetes client"**: the `env`
  block in the MCP config does not inherit your shell's `KUBECONFIG`.
  Set it explicitly there.
- **Audit log writes fail**: set `AGENTMOAT_AUDIT_PATH` in the MCP
  config's `env` block, or pass `"audit_enabled": false` per call.
