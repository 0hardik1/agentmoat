// Shared helpers for the MCP unit-test suite.
//
// Each tool handler is registered on the *server.MCPServer and stored in
// the server's tool map under its name. Tests retrieve the handler via
// srv.ListTools()[name].Handler and invoke it with a hand-constructed
// CallToolRequest. This pattern keeps each tool's wiring under one roof
// (newServer is the single registration site) and lets tests drive the
// handlers without the JSON-RPC transport.
package main

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"k8s.io/client-go/kubernetes"
)

// newTestServer constructs an MCPServer with the testing seam wired in:
// stderr is silenced so test output stays clean, and the caller's fake
// kubernetes client is plumbed through to every tool handler via Deps.
func newTestServer(client kubernetes.Interface) *server.MCPServer {
	return newServer(Deps{
		Stderr:     io.Discard,
		Version:    "test",
		KubeClient: client,
	})
}

// callTool retrieves the named tool's handler from a constructed server
// and invokes it with the given arguments. Returns the tool-result and
// any Go error from the handler.
func callTool(t *testing.T, srv *server.MCPServer, name string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	st, ok := srv.ListTools()[name]
	if !ok {
		t.Fatalf("tool %q not registered", name)
	}
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	return st.Handler(context.Background(), req)
}

// readTool extracts the JSON body from a successful tool-result and
// decodes it into the provided target. Fails the test if the result is
// an error or if decoding fails.
func readTool(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()
	if result == nil {
		t.Fatalf("nil tool result")
	}
	if result.IsError {
		t.Fatalf("expected success, got tool-result error: %s", toolText(t, result))
	}
	body := toolText(t, result)
	if err := json.Unmarshal([]byte(body), target); err != nil {
		t.Fatalf("decode tool result: %v\nbody: %s", err, body)
	}
}

// expectToolError returns true if the result is an error tool-result and
// its text contains the given substring. Fails the test if not.
func expectToolError(t *testing.T, result *mcp.CallToolResult, substr string) {
	t.Helper()
	if result == nil {
		t.Fatalf("nil tool result")
	}
	if !result.IsError {
		t.Fatalf("expected tool-result error containing %q, got success: %s", substr, toolText(t, result))
	}
	if got := toolText(t, result); !contains(got, substr) {
		t.Fatalf("error text missing %q: %s", substr, got)
	}
}

func toolText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatalf("tool result has no content")
	}
	switch c := result.Content[0].(type) {
	case mcp.TextContent:
		return c.Text
	default:
		t.Fatalf("expected TextContent, got %T", c)
		return ""
	}
}

// contains is a one-line substring check kept out of strings to avoid a
// trivial import in every test file.
func contains(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
