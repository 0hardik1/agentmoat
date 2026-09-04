// Server construction and stdio serve loop.
//
// newServer assembles an MCPServer with every tool, resource, and prompt
// registered. The serve loop in Run() blocks on stdio until the client
// disconnects (or EOF on stdin).
//
// Splitting Run() (which actually serves) from newServer() (which only
// constructs) is what makes the unit tests possible: they reuse newServer
// to get a fully-wired *server.MCPServer without dialing stdio.
package main

import (
	"fmt"

	"github.com/mark3labs/mcp-go/server"
)

// serverInstructions is surfaced in the MCP `initialize` response so the
// client (and the LLM it drives) gets an at-a-glance summary of what this
// server is for. Keep it short; the per-tool descriptions carry the
// detail.
const serverInstructions = `agentmoat-mcp moves Kubernetes workloads from runc to gVisor (runsc).
Read-only tools: scan_cluster, preflight_cluster, assess_workload, propose_plan, verify_migration, explain.
Mutating tools: apply_plan, rollback_plan (default dry_run=true; must set "dry_run": false to mutate).
Call preflight_cluster before apply_plan: apply refuses to run (every step skipped) when the cluster has no Ready node that can host the RuntimeClass.
Resources: agentmoat://compatibility-rules, agentmoat://known-gotchas.
Prompt: audit-cluster-for-agentic-workloads (audit for LLM/MCP/agent workloads).`

// newServer builds the *server.MCPServer with every tool/resource/prompt
// registered. The registration order does not matter functionally (the
// server stores them in maps) but we list tools first, resources second,
// prompts last for readability when scanning the file.
func newServer(deps Deps) *server.MCPServer {
	version := deps.Version
	if version == "" {
		version = "dev"
	}
	srv := server.NewMCPServer(
		"agentmoat-mcp",
		version,
		server.WithToolCapabilities(false), // we never push tools/list_changed
		server.WithResourceCapabilities(false, false),
		server.WithPromptCapabilities(false),
		server.WithInstructions(serverInstructions),
		server.WithRecovery(),
	)

	// Tools (8). Each registerXxx call adds one tool definition + handler.
	registerScanCluster(srv, deps)
	registerPreflightCluster(srv, deps)
	registerAssessWorkload(srv, deps)
	registerProposePlan(srv, deps)
	registerApplyPlan(srv, deps)
	registerRollbackPlan(srv, deps)
	registerVerifyMigration(srv, deps)
	registerExplain(srv, deps)

	// Resources (2).
	registerResources(srv)

	// Prompts (1).
	registerAuditPrompt(srv)

	return srv
}

// Run is the binary's serve loop. It builds the server, hands it to
// server.ServeStdio, and returns when stdin is closed or the server
// errors out. Errors are returned to the caller (main.go), which
// surfaces them on stderr with a non-zero exit code.
func Run(deps Deps) error {
	srv := newServer(deps)
	if err := server.ServeStdio(srv); err != nil {
		return fmt.Errorf("serve stdio: %w", err)
	}
	return nil
}
