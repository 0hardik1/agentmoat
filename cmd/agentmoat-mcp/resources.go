// Resource handlers for the MCP server.
//
// Two read-only resources are exposed:
//
//   - agentmoat://compatibility-rules
//     Returns internal/rules/gvisor.yaml. This is the classifier's
//     overridable severity-and-description table; an LLM that wants
//     to reason about why a rule fired (or to suggest a custom
//     override) can fetch it here.
//
//   - agentmoat://known-gotchas
//     Returns docs/compatibility-checklist.md. A curated, hand-written
//     summary of the gVisor compatibility caveats relevant to common
//     Kubernetes workloads. The MCP server reads it from the existing
//     docs.FS (the same embed that powers `agentmoat explain`).
//
// Both resources are byte-identical to the on-disk files; the embed
// machinery just lets us serve them without requiring a known filesystem
// layout at runtime (the MCP binary may run in a container, sandbox, etc.).
package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/docs"
	"github.com/0hardik1/agentmoat/internal/rules"
)

const (
	uriCompatRules  = "agentmoat://compatibility-rules"
	uriKnownGotchas = "agentmoat://known-gotchas"
)

func registerResources(srv *server.MCPServer) {
	srv.AddResource(
		mcp.NewResource(uriCompatRules, "compatibility-rules",
			mcp.WithResourceDescription("Classifier severity overrides (internal/rules/gvisor.yaml). Read this to learn each rule's ID, severity, description, and remediation URL."),
			mcp.WithMIMEType("application/yaml"),
		),
		func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			return []mcp.ResourceContents{mcp.TextResourceContents{
				URI:      uriCompatRules,
				MIMEType: "application/yaml",
				Text:     string(rules.GVisorYAML()),
			}}, nil
		},
	)

	srv.AddResource(
		mcp.NewResource(uriKnownGotchas, "known-gotchas",
			mcp.WithResourceDescription("Curated checklist of gVisor compatibility caveats (docs/compatibility-checklist.md)."),
			mcp.WithMIMEType("text/markdown"),
		),
		func(_ context.Context, _ mcp.ReadResourceRequest) ([]mcp.ResourceContents, error) {
			data, err := docs.FS.ReadFile("compatibility-checklist.md")
			if err != nil {
				return nil, fmt.Errorf("known-gotchas: %w", err)
			}
			return []mcp.ResourceContents{mcp.TextResourceContents{
				URI:      uriKnownGotchas,
				MIMEType: "text/markdown",
				Text:     string(data),
			}}, nil
		},
	)
}
