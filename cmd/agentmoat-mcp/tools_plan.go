// propose_plan tool: deterministic migration plan from a scan.
//
// Maps to agentmoat.Plan. The tool accepts either (a) a scan_report_path
// pointing at an existing ScanReport on disk (the same shape the CLI's
// `--scan` flag accepts) or (b) the scan-flag set, in which case the
// orchestrator runs Scan inline. Both yield a deterministic MigrationPlan
// (same scan in -> same planHash out).
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

func registerProposePlan(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("propose_plan",
		mcp.WithDescription("Produce a deterministic MigrationPlan from a scan. Same scan in produces the same planHash. The plan is read-only: this tool never mutates the cluster (use apply_plan for that)."),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file (used when scan_report_path is empty and an inline scan must run).")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name.")),
		mcp.WithArray("namespaces",
			mcp.Description("Namespaces to scan inline. Empty means cluster-wide."),
			mcp.WithStringItems(),
		),
		mcp.WithBoolean("include_system",
			mcp.Description("Include kube-system namespaces in the inline scan. Default false."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("label_selector",
			mcp.Description("Kubernetes label selector for the inline scan.")),
		mcp.WithString("rules_yaml_path",
			mcp.Description("Optional path to a classifier rules YAML override.")),
		mcp.WithString("scan_report_path",
			mcp.Description("Optional path to a previously-saved ScanReport. When set, the planner uses it instead of running a fresh scan.")),
		mcp.WithBoolean("include_review",
			mcp.Description("Include 'review'-class workloads in the plan. Default false (only 'compatible' workloads are included)."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("runtime_class_name",
			mcp.Description("RuntimeClass name to write into the migrated pod templates. Default 'gvisor' matches the bundled RuntimeClass.")),
		mcp.WithBoolean("add_toleration",
			mcp.Description("Also patch the runtime=gvisor:NoSchedule toleration into each pod template. Default false: placement comes from the RuntimeClass's scheduling block. Changes the planHash."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("facts_path",
			mcp.Description("Path to a saved PreflightReport (probe_nvproxy output) or ScanReport. The inline scan classifies against its cluster facts (GPU workloads settle from the probed driver list) and the plan warnings come from it; with scan_report_path it replaces the stored facts for the warnings only.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsList := req.GetStringSlice("namespaces", nil)
		opts := agentmoat.PlanOptions{
			ScanOptions: agentmoat.ScanOptions{
				KubeconfigPath:   req.GetString("kubeconfig_path", ""),
				Context:          req.GetString("context", ""),
				Namespaces:       nsList,
				AllNamespaces:    len(nsList) == 0,
				IncludeSystem:    req.GetBool("include_system", false),
				LabelSelector:    req.GetString("label_selector", ""),
				RulesYAMLPath:    req.GetString("rules_yaml_path", ""),
				RuntimeClassName: req.GetString("runtime_class_name", ""),
				FactsPath:        req.GetString("facts_path", ""),
				Stderr:           deps.Stderr,
				KubeClient:       deps.KubeClient,
			},
			// BatchSize / MaxParallel are deliberately NOT exposed as tool
			// arguments: the v1 applier walks steps serially, so those
			// fields would only perturb the planHash without changing any
			// behavior. They stay in the schema (reserved for a future
			// batched applier) but default to zero here.
			Planner: schema.PlannerOptions{
				IncludeReview:    req.GetBool("include_review", false),
				RuntimeClassName: req.GetString("runtime_class_name", ""),
				AddToleration:    req.GetBool("add_toleration", false),
			},
			Stderr: deps.Stderr,
		}

		// scan_report_path names a saved ScanReport. The library loads it,
		// exactly as it does for `agentmoat plan --scan <file>`.
		opts.ScanReportPath = req.GetString("scan_report_path", "")

		plan, err := agentmoat.Plan(ctx, opts)
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(plan)
	})
}
