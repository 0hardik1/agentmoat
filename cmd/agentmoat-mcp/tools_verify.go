// verify_migration tool: read-only "did the apply take?" check.
//
// Maps to agentmoat.Verify. The optional in_pod_probe field opts into the
// gVisor in-pod probe (uname / dmesg / /proc/cmdline inspection); without
// it the check is purely API-side (kubelet's pod runtimeClassName field).
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

func registerVerifyMigration(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("verify_migration",
		mcp.WithDescription("Read-only check that the workloads in a MigrationPlan have actually landed on the gVisor RuntimeClass. Returns a VerifyReport with per-step status (ok | mismatch | error). Set in_pod_probe=true to also exec a /proc-and-uname probe in one Running pod per step."),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name.")),
		mcp.WithString("plan_path",
			mcp.Required(),
			mcp.Description("Path to the MigrationPlan YAML/JSON file the apply was driven from.")),
		mcp.WithBoolean("in_pod_probe",
			mcp.Description("If true, exec a small command in one Running pod per step to confirm the kernel reports gVisor markers. Slower; opt-in."),
			mcp.DefaultBool(false),
		),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		planPath, err := req.RequireString("plan_path")
		if err != nil {
			return toolErr(err)
		}
		opts := agentmoat.VerifyOptions{
			KubeconfigPath: req.GetString("kubeconfig_path", ""),
			Context:        req.GetString("context", ""),
			PlanPath:       planPath,
			InPodProbe:     req.GetBool("in_pod_probe", false),
			Stderr:         deps.Stderr,
			KubeClient:     deps.KubeClient,
			RestConfig:     deps.RestConfig,
			ExecRunner:     deps.ExecRunner,
		}
		report, err := agentmoat.Verify(ctx, opts)
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(report)
	})
}
