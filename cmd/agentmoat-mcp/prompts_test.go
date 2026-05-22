// Unit tests for the audit-cluster-for-agentic-workloads prompt. The
// prompt body carries the operator-editable heuristic (image-name and
// env-var matchers); this test pins the keywords so a future contributor
// who accidentally deletes the heuristic gets a clear failure.
package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
)

func getPrompt(t *testing.T, args map[string]string) string {
	t.Helper()
	srv := newServer(Deps{Stderr: io.Discard})
	entry, ok := srv.ListPrompts()["audit-cluster-for-agentic-workloads"]
	if !ok {
		t.Fatalf("audit prompt not registered")
	}
	req := mcp.GetPromptRequest{}
	req.Params.Name = "audit-cluster-for-agentic-workloads"
	req.Params.Arguments = args
	result, err := entry.Handler(context.Background(), req)
	if err != nil {
		t.Fatalf("prompt handler: %v", err)
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1", len(result.Messages))
	}
	tc, ok := result.Messages[0].Content.(mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Messages[0].Content)
	}
	return tc.Text
}

func TestAuditPrompt_KeywordsPresent(t *testing.T) {
	t.Parallel()
	body := getPrompt(t, nil)
	// Pin a subset of the heuristic so future edits cannot silently drop it.
	for _, want := range []string{
		"scan_cluster",
		"assess_workload",
		"propose_plan",
		"ANTHROPIC_API_KEY",
		"OPENAI_API_KEY",
		"claude",
		"openai",
		"langchain",
		"Do not call apply_plan",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prompt body missing %q", want)
		}
	}
}

func TestAuditPrompt_TemplatesNamespace(t *testing.T) {
	t.Parallel()
	body := getPrompt(t, map[string]string{"namespace": "team-foo"})
	if !strings.Contains(body, `namespaces=["team-foo"]`) {
		t.Errorf("prompt body should template the namespace; got: %s", body)
	}
}

func TestAuditPrompt_TemplatesIncludeReview(t *testing.T) {
	t.Parallel()
	body := getPrompt(t, map[string]string{"include_review": "true"})
	if !strings.Contains(body, "include_review=true") {
		t.Errorf("prompt body should template include_review; got: %s", body)
	}
}
