// scan_cluster tool: read-only enumeration + classification of cluster workloads.
//
// One-to-one mapping with agentmoat.Scan. The input schema mirrors
// ScanOptions field-by-field, with snake_case names so they read naturally
// in JSON-RPC. The result is the *schema.ScanReport serialized as JSON in
// the tool result body.
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

func registerScanCluster(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("scan_cluster",
		mcp.WithDescription("Enumerate Kubernetes workloads and classify each for gVisor compatibility. Returns a versioned ScanReport (apiVersion: agentmoat.io/v1alpha1, kind: ScanReport)."),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file. Empty uses the standard search path (KUBECONFIG env, then ~/.kube/config) and falls back to in-cluster.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name. Empty uses the active context.")),
		mcp.WithArray("namespaces",
			mcp.Description("Namespaces to scan. Empty means cluster-wide."),
			mcp.WithStringItems(),
		),
		mcp.WithBoolean("include_system",
			mcp.Description("Include kube-system and other kube-* namespaces. Default false."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("label_selector",
			mcp.Description("Kubernetes-style label selector applied to every list call. Empty means no filter.")),
		mcp.WithString("rules_yaml_path",
			mcp.Description("Optional path to a classifier rules YAML override (layered on top of the built-in rule set).")),
		mcp.WithString("runtime_class_name",
			mcp.Description("RuntimeClass to inspect for metadata.clusterFacts (node and RuntimeClass inventory). Default 'gvisor'.")),
		mcp.WithBoolean("no_cluster_facts",
			mcp.Description("Skip the node and RuntimeClass reads; the report omits metadata.clusterFacts. Default false."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("facts_path",
			mcp.Description("Path to a saved PreflightReport (from probe_nvproxy or preflight_cluster) or ScanReport whose cluster facts replace the live reads. This is how the nvproxy driver list reaches the classifier; GPU workloads are then classified against the real card and driver support.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsList := req.GetStringSlice("namespaces", nil)
		opts := agentmoat.ScanOptions{
			KubeconfigPath:   req.GetString("kubeconfig_path", ""),
			Context:          req.GetString("context", ""),
			Namespaces:       nsList,
			AllNamespaces:    len(nsList) == 0,
			IncludeSystem:    req.GetBool("include_system", false),
			LabelSelector:    req.GetString("label_selector", ""),
			RulesYAMLPath:    req.GetString("rules_yaml_path", ""),
			RuntimeClassName: req.GetString("runtime_class_name", ""),
			SkipClusterFacts: req.GetBool("no_cluster_facts", false),
			FactsPath:        req.GetString("facts_path", ""),
			Stderr:           deps.Stderr,
			KubeClient:       deps.KubeClient,
		}
		report, err := agentmoat.Scan(ctx, opts)
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(report)
	})
}
