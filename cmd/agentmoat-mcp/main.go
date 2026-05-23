// Binary entrypoint for agentmoat-mcp.
//
// The binary speaks JSON-RPC over stdio. Diagnostics (progress, warnings)
// go to stderr because mixing them into stdout would corrupt the
// JSON-RPC channel. A small set of CLI flags are accepted for
// inspectability: --help, --version, --log-level.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// Version and GitSHA are set at build time via -ldflags. Mirrors the
// CLI binary's pattern so both binaries report the same release string.
var (
	Version = "dev"
	GitSHA  = "unknown"
)

func main() {
	// Flags. We deliberately keep this surface tiny: MCP clients drive
	// the server over JSON-RPC, not via command-line arguments. The flags
	// here are operator-facing diagnostics.
	help := flag.Bool("help", false, "Show usage and exit.")
	version := flag.Bool("version", false, "Print version and exit.")
	logLevel := flag.String("log-level", "info", "Diagnostic log level on stderr: silent|info|debug. Stdout always carries JSON-RPC.")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, usage())
	}
	flag.Parse()

	if *help {
		_, _ = fmt.Fprintln(os.Stdout, usage())
		return
	}
	if *version {
		_, _ = fmt.Fprintf(os.Stdout, "agentmoat-mcp %s (git %s)\n", Version, GitSHA)
		return
	}

	stderr := io.Writer(os.Stderr)
	if *logLevel == "silent" {
		stderr = io.Discard
	}

	deps := Deps{
		Stderr:  stderr,
		Version: Version,
	}
	if err := Run(deps); err != nil {
		fmt.Fprintf(os.Stderr, "agentmoat-mcp: %v\n", err)
		os.Exit(1)
	}
}

// usage returns the long help text. Documented here because operators who
// run `agentmoat-mcp --help` should learn enough to wire it up to a
// Claude Code (or other MCP) client without reading the docs first.
func usage() string {
	return `agentmoat-mcp: Model Context Protocol server for the agentmoat toolkit (stdio mode).

Reads JSON-RPC requests on stdin and writes JSON-RPC responses on stdout
(MCP-over-stdio transport). This binary is meant to be launched by an MCP
client (such as Claude Code) as a subprocess; it is not interactive on a
terminal.

Quick start: add the following to your Claude Code MCP config
(typically ~/.claude/claude_desktop_config.json):

  {
    "mcpServers": {
      "agentmoat": {
        "command": "/path/to/agentmoat-mcp",
        "args": [],
        "env": {}
      }
    }
  }

See docs/mcp-integration.md for the full walkthrough.

Usage:
  agentmoat-mcp [flags]

Flags:
  --help              Show this help and exit.
  --version           Print the binary version and exit.
  --log-level LEVEL   Diagnostic log level (silent|info|debug). stdout always
                      carries JSON-RPC framing; only stderr is gated.

Surface:
  Tools     scan_cluster, assess_workload, propose_plan, apply_plan,
            rollback_plan, verify_migration, explain
  Resources agentmoat://compatibility-rules, agentmoat://known-gotchas
  Prompts   audit-cluster-for-agentic-workloads`
}
