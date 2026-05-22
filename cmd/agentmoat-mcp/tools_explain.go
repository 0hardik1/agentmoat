// explain tool: offline educational topic reader.
//
// Maps to agentmoat.Explain. No cluster access. With topic="" returns the
// list of available topics; with a known topic returns the markdown body.
// Unknown topics surface a tool-result error with the valid topic list.
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

func registerExplain(srv *server.MCPServer, deps Deps) {
	_ = deps // explain is offline; no client/version needed.

	tool := mcp.NewTool("explain",
		mcp.WithDescription("Return an educational topic from the bundled docs (gvisor, runtimeclass, threat model, etc.). Topic lookup is case-insensitive. Empty topic returns the list of available topics."),
		mcp.WithString("topic",
			mcp.Description("Topic name. Empty returns the topic index. Lookup is case-insensitive.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		_ = ctx // Explain is synchronous and does not honor ctx today.
		doc, err := agentmoat.Explain(agentmoat.ExplainOptions{
			Topic: req.GetString("topic", ""),
		})
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(doc)
	})
}
