// Unit tests for the explain tool. Offline; no cluster client needed.
package main

import (
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func TestExplainHandler_ListMode(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset())
	result, err := callTool(t, srv, "explain", map[string]any{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var doc schema.ExplainDocument
	readTool(t, result, &doc)
	if len(doc.Spec.Topics) == 0 {
		t.Errorf("expected non-empty topic list; got %+v", doc)
	}
}

func TestExplainHandler_KnownTopic(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset())
	result, err := callTool(t, srv, "explain", map[string]any{"topic": "gvisor"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var doc schema.ExplainDocument
	readTool(t, result, &doc)
	if doc.Spec.Content == "" {
		t.Errorf("expected non-empty content for topic 'gvisor', got %+v", doc)
	}
}

func TestExplainHandler_UnknownTopic(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset())
	result, _ := callTool(t, srv, "explain", map[string]any{"topic": "nonsuch"})
	expectToolError(t, result, "nonsuch")
}
