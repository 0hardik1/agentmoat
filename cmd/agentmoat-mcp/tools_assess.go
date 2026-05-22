// assess_workload tool: classify a single named workload.
//
// Maps one-to-one to agentmoat.AssessWorkload. The result body is a
// schema.WorkloadResult: the same per-row shape that scan_cluster
// returns inside its WorkloadResults slice.
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

func registerAssessWorkload(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("assess_workload",
		mcp.WithDescription("Classify one named Kubernetes workload for gVisor compatibility. Returns a WorkloadResult with the per-rule reasons, recommendation, and qualitative overhead estimate. Use when you already know the target workload; for cluster-wide assessment use scan_cluster."),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file. See scan_cluster for fallback semantics.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name.")),
		mcp.WithString("kind",
			mcp.Required(),
			mcp.Description("Kubernetes kind. One of: Pod, Deployment, StatefulSet, DaemonSet, Job, CronJob."),
			mcp.Enum("Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"),
		),
		mcp.WithString("namespace",
			mcp.Required(),
			mcp.Description("Namespace of the target workload.")),
		mcp.WithString("name",
			mcp.Required(),
			mcp.Description("Name of the target workload (must be unique within Kind+Namespace).")),
		mcp.WithString("rules_yaml_path",
			mcp.Description("Optional path to a classifier rules YAML override.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		kind, err := req.RequireString("kind")
		if err != nil {
			return toolErr(err)
		}
		namespace, err := req.RequireString("namespace")
		if err != nil {
			return toolErr(err)
		}
		name, err := req.RequireString("name")
		if err != nil {
			return toolErr(err)
		}
		opts := agentmoat.AssessWorkloadOptions{
			KubeconfigPath: req.GetString("kubeconfig_path", ""),
			Context:        req.GetString("context", ""),
			Kind:           kind,
			Namespace:      namespace,
			Name:           name,
			RulesYAMLPath:  req.GetString("rules_yaml_path", ""),
			Stderr:         deps.Stderr,
			KubeClient:     deps.KubeClient,
		}
		result, err := agentmoat.AssessWorkload(ctx, opts)
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(result)
	})
}
