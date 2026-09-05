// preflight_cluster tool: read-only "can this cluster host gVisor pods?".
//
// Maps to agentmoat.Preflight. The result is a PreflightReport: facts
// (RuntimeClass, nodes, platform), findings with stable IDs and
// remediations, and a summary whose `ready` field is the one an agent
// should branch on before calling apply_plan. apply_plan runs this same
// check itself and refuses to proceed on an error finding, so calling
// this first is how an agent avoids a blocked apply and explains the fix
// to the operator.
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

func registerPreflightCluster(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("preflight_cluster",
		mcp.WithDescription("Read-only check that pods requesting the gVisor RuntimeClass can actually schedule: the RuntimeClass exists, its scheduling.nodeSelector matches at least one Ready node whose taints it tolerates, and the candidate nodes are not EKS Auto Mode or Bottlerocket instances (AWS-managed, immutable, runsc cannot be installed). Returns a PreflightReport; spec.summary.ready=false means apply_plan will refuse to run. Findings carry stable ids and a remediation each."),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file. Empty uses the standard search path, then in-cluster.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name. Empty uses the active context.")),
		mcp.WithString("runtime_class_name",
			mcp.Description("RuntimeClass to inspect. Default 'gvisor'.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		report, err := agentmoat.Preflight(ctx, agentmoat.PreflightOptions{
			KubeconfigPath:   req.GetString("kubeconfig_path", ""),
			Context:          req.GetString("context", ""),
			RuntimeClassName: req.GetString("runtime_class_name", ""),
			Stderr:           deps.Stderr,
			KubeClient:       deps.KubeClient,
		})
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(report)
	})
}
