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
	"os"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"sigs.k8s.io/yaml"

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
		mcp.WithNumber("batch_size",
			mcp.Description("How many workloads to apply per batch before waiting on readiness. 0 uses the planner default.")),
		mcp.WithNumber("max_parallel",
			mcp.Description("Maximum patches in flight inside a single batch. 0 uses the planner default.")),
		mcp.WithString("runtime_class_name",
			mcp.Description("RuntimeClass name to write into the migrated pod templates. Default 'gvisor' matches the bundled RuntimeClass.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		nsList := req.GetStringSlice("namespaces", nil)
		opts := agentmoat.PlanOptions{
			ScanOptions: agentmoat.ScanOptions{
				KubeconfigPath: req.GetString("kubeconfig_path", ""),
				Context:        req.GetString("context", ""),
				Namespaces:     nsList,
				AllNamespaces:  len(nsList) == 0,
				IncludeSystem:  req.GetBool("include_system", false),
				LabelSelector:  req.GetString("label_selector", ""),
				RulesYAMLPath:  req.GetString("rules_yaml_path", ""),
				Stderr:         deps.Stderr,
				KubeClient:     deps.KubeClient,
			},
			Planner: schema.PlannerOptions{
				BatchSize:        intArg(req, "batch_size"),
				MaxParallel:      intArg(req, "max_parallel"),
				IncludeReview:    req.GetBool("include_review", false),
				RuntimeClassName: req.GetString("runtime_class_name", ""),
			},
			Stderr: deps.Stderr,
		}

		// If the caller supplied a scan_report_path, load and use it as the
		// source. Mirrors how `cmd/agentmoat/plan.go --scan <file>` works.
		if path := req.GetString("scan_report_path", ""); path != "" {
			report, err := loadScanReport(path)
			if err != nil {
				return toolErr(err)
			}
			opts.ScanReport = report
		}

		plan, err := agentmoat.Plan(ctx, opts)
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(plan)
	})
}

// intArg pulls a number argument and casts it to int. Returns 0 when the
// caller omitted the field; the planner treats 0 as "use the default".
func intArg(req mcp.CallToolRequest, key string) int {
	args := req.GetArguments()
	v, ok := args[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case int64:
		return int(x)
	default:
		return 0
	}
}

// loadScanReport reads a ScanReport from disk (JSON or YAML; sigs.k8s.io/yaml
// accepts both). The schema sanity-check is light: we trust the caller's
// path, but a wrong-Kind file should surface a clean error rather than a
// silently-empty plan.
func loadScanReport(path string) (*schema.ScanReport, error) {
	// #nosec G304 -- path is operator-supplied and intentionally
	// configurable; this mirrors the CLI's --scan flag behavior.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	report := &schema.ScanReport{}
	if err := yaml.Unmarshal(data, report); err != nil {
		return nil, err
	}
	if report.Kind != schema.KindScanReport {
		return nil, &kindMismatchError{got: report.Kind, want: schema.KindScanReport}
	}
	return report, nil
}

// kindMismatchError is the typed error returned when a loaded document's
// Kind does not match what the caller asked for. Kept as a private type
// so callers don't depend on it (string matching against Error() is fine).
type kindMismatchError struct {
	got, want string
}

func (e *kindMismatchError) Error() string {
	return "scan_report_path: expected Kind=" + e.want + ", got Kind=" + e.got
}
