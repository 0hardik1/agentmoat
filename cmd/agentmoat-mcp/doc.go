// Command agentmoat-mcp is the MCP (Model Context Protocol) server front-end
// for the agentmoat toolkit.
//
// The MCP server speaks JSON-RPC over stdio per the Model Context Protocol
// spec (https://modelcontextprotocol.io/). Every tool, resource, and prompt
// here is a thin shell over the same pkg/agentmoat library that powers the
// CLI (`agentmoat`); the two surfaces share orchestrator functions
// (Scan, AssessWorkload, Plan, Apply, Rollback, Verify, Explain) so they
// cannot drift.
//
// The seven tools and two resources form the contract documented in
// plan.md section 8. The one prompt (audit-cluster-for-agentic-workloads)
// is a curated heuristic for identifying workloads that benefit most from
// gVisor's sandbox boundary (LLM-calling services, autonomous agents, MCP
// servers). See docs/mcp-integration.md for the wire-level walkthrough.
//
// Safety
//
//	The mutating tools (apply_plan, rollback_plan) refuse to mutate the
//	cluster unless the caller explicitly sets `"dry_run": false`. Omitting
//	the field, or sending `"dry_run": true`, runs the orchestrator in
//	dry-run mode and returns the would-be patches without applying them.
//	This mirrors the CLI's load-bearing `--dry-run=true` default.
package main
