// Tool-handler glue shared by every tool file in this package.
//
// marshalResult turns one of agentmoat's schema structs (ScanReport,
// MigrationPlan, ApplyResult, ...) into the JSON-RPC `result.content[0].text`
// body the MCP client receives. We call encoding/json directly (rather than
// pkg/output/json.go) for two reasons:
//
//   - pkg/output adds a trailing newline meant for shell redirection; MCP
//     clients don't want that.
//   - Using the stable json: tags on the schema types guarantees byte-for-
//     byte parity with the CLI's `--output json` shape.
//
// The mcp-go library also exposes NewToolResultJSON which marshals and
// attaches a `StructuredContent` alongside the text body, so newer clients
// that prefer typed content get the same object without re-parsing the
// text. We use that helper here for forward compatibility while still
// producing a TextContent body so older clients work.
package main

import (
	"github.com/mark3labs/mcp-go/mcp"
)

// marshalResult wraps mcp.NewToolResultJSON with a uniform error path: when
// JSON encoding fails we return a tool-result error rather than a Go error,
// so the MCP client sees a structured failure instead of a transport-level
// crash. The error path is theoretically unreachable for our schema types
// (every field is encoder-safe), but a generic guard rail is cheap.
func marshalResult[T any](v T) (*mcp.CallToolResult, error) {
	out, err := mcp.NewToolResultJSON(v)
	if err != nil {
		return mcp.NewToolResultErrorf("encoding result: %v", err), nil
	}
	return out, nil
}

// toolErr produces a tool-result error from a Go error. We surface the
// orchestrator's wrapped error verbatim ("apply: building kubernetes
// client: ...") because the stage prefix is what an operator needs to
// triage; re-wrapping it here would just add noise.
func toolErr(err error) (*mcp.CallToolResult, error) {
	if err == nil {
		return nil, nil
	}
	return mcp.NewToolResultError(err.Error()), nil
}
