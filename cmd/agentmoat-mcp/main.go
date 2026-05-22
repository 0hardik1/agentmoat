// Command agentmoat-mcp is the MCP (Model Context Protocol) server front-end
// for the agentmoat toolkit.
//
// Phase status: PLACEHOLDER. The full MCP server is targeted for Phase 4
// of the roadmap (plan.md section 14). This binary currently prints a
// stub message and exits non-zero so anyone who runs it sees a clear
// "not yet implemented" signal rather than a silent no-op.
//
// When implemented, agentmoat-mcp will speak MCP over stdio, modeled on
// github.com/containers/kubernetes-mcp-server. It will share the same
// pkg/agentmoat library that powers the CLI, so the two surfaces cannot
// drift. See plan.md section 8 for the full tool/resource surface.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "agentmoat-mcp: not implemented yet (Phase 4 target).")
	fmt.Fprintln(os.Stderr, "Track progress at https://github.com/0hardik1/agentmoat.")
	os.Exit(1)
}
