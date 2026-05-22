// Unit tests for the two read-only resources. Asserts the URI, MIME type,
// and a content marker for each (so a future contributor who deletes the
// underlying file gets a clean signal).
package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestResource_CompatibilityRules(t *testing.T) {
	t.Parallel()
	srv := newServer(Deps{Stderr: io.Discard})
	entry, ok := srv.ListResources()["agentmoat://compatibility-rules"]
	if !ok {
		t.Fatalf("compatibility-rules resource missing")
	}
	contents, err := entry.Handler(context.Background(), mcp.ReadResourceRequest{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("contents: got %d, want 1", len(contents))
	}
	tc, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("expected TextResourceContents, got %T", contents[0])
	}
	if tc.MIMEType != "application/yaml" {
		t.Errorf("MIME: got %q, want application/yaml", tc.MIMEType)
	}
	if !strings.Contains(tc.Text, "raw-socket") {
		t.Errorf("body missing the 'raw-socket' rule ID; first 200 chars: %q", truncate(tc.Text, 200))
	}
}

func TestResource_KnownGotchas(t *testing.T) {
	t.Parallel()
	srv := newServer(Deps{Stderr: io.Discard})
	entry, ok := srv.ListResources()["agentmoat://known-gotchas"]
	if !ok {
		t.Fatalf("known-gotchas resource missing")
	}
	contents, err := entry.Handler(context.Background(), mcp.ReadResourceRequest{})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(contents) != 1 {
		t.Fatalf("contents: got %d, want 1", len(contents))
	}
	tc, ok := contents[0].(mcp.TextResourceContents)
	if !ok {
		t.Fatalf("expected TextResourceContents, got %T", contents[0])
	}
	if tc.MIMEType != "text/markdown" {
		t.Errorf("MIME: got %q, want text/markdown", tc.MIMEType)
	}
	if !strings.Contains(strings.ToLower(tc.Text), "gvisor") {
		t.Errorf("body should mention gVisor; got: %q", truncate(tc.Text, 200))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
