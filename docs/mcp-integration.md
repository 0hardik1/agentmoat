# MCP integration

> Status: STUB. Filled in by Phase 4 (MCP server).

## Planned table of contents

1. What MCP is (Model Context Protocol, stdio-based JSON-RPC)
2. Why `agentmoat-mcp` is a sibling binary to `agentmoat` (shared library, no drift)
3. Tools exposed by `agentmoat-mcp`:
   - `scan_cluster`
   - `assess_workload`
   - `propose_plan`
   - `apply_plan` (refuses to mutate unless `dry_run=false` is explicit)
   - `verify_migration`
   - `explain`
4. Resources: `agentmoat://compatibility-rules`, `agentmoat://known-gotchas`
5. Prompts: `audit-cluster-for-agentic-workloads`
6. Wiring `agentmoat-mcp` into Claude Code (`~/.claude.json` snippet)
7. Wiring `agentmoat-mcp` into other MCP clients (Cursor, Continue, etc.)

## Today

Run `./bin/agentmoat-mcp` and you will get a "not implemented yet"
message. The full design lives in plan.md section 8 (gitignored locally).

The CLI's `--output json` already emits the same versioned schema
(`agentmoat.io/v1alpha1`) that the MCP server will use, so you can
pipe `agentmoat scan --output json` into any MCP-aware tool today and
get reasonable results.
