// apply_plan and rollback_plan tools: the two mutating verbs.
//
// Safety contract (the load-bearing piece of this file):
//
//	Both tools refuse to mutate the cluster unless the caller explicitly
//	sets `"dry_run": false` in the JSON-RPC arguments. Omitting the field,
//	or sending `"dry_run": true`, runs the orchestrator in dry-run mode
//	and returns the would-be patches without applying them.
//
// We distinguish "caller omitted dry_run" from "caller set dry_run=false"
// by inspecting the raw arguments map (req.GetArguments) rather than
// relying on a bool default. This mirrors the CLI's load-bearing
// --dry-run=true default at cmd/agentmoat/apply.go.
package main

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

// applyPlanDescription documents the safety gate next to the schema so
// inspectors (humans and LLMs) cannot miss it.
const applyPlanDescription = "Apply a MigrationPlan to the cluster. SAFETY: defaults to dry-run; you MUST pass \"dry_run\": false explicitly to mutate the cluster. Runs the cluster preflight first (see preflight_cluster) and refuses to proceed on an error finding: the result then has metadata.preflight.ready=false, every step skipped, and spec.preflightFindings explaining why; nothing is mutated. Returns an ApplyResult with per-step status (applied | already-applied | skipped | failed)."

const rollbackPlanDescription = "Rollback a previously-applied MigrationPlan. SAFETY: same dry-run default as apply_plan; you MUST pass \"dry_run\": false explicitly to mutate. Removes runtimeClassName and clears the namespace plan-hash annotation."

// commonApplyToolOptions are the schema fields shared by both apply and
// rollback tools. Centralized so they cannot diverge.
func commonApplyToolOptions() []mcp.ToolOption {
	return []mcp.ToolOption{
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name.")),
		mcp.WithString("plan_path",
			mcp.Required(),
			mcp.Description("Path to a MigrationPlan YAML/JSON file on disk.")),
		mcp.WithBoolean("dry_run",
			mcp.Description("If omitted or true, no cluster state is mutated. Set explicitly to false to apply patches."),
			mcp.DefaultBool(true),
		),
		mcp.WithBoolean("emit_events",
			mcp.Description("Emit one Kubernetes Event per mutation. Default true."),
			mcp.DefaultBool(true),
		),
		mcp.WithBoolean("audit_enabled",
			mcp.Description("Append each mutation to ~/.agentmoat/audit.jsonl (or AGENTMOAT_AUDIT_PATH). Default true."),
			mcp.DefaultBool(true),
		),
	}
}

// readDryRun pulls "dry_run" out of the raw arguments map and returns:
//   - the boolean value to pass through to the orchestrator,
//   - whether the caller explicitly set false (i.e. opted into mutation),
//   - an error if the value is present but not a boolean.
//
// Absent OR true -> dry-run (caller has not opted into mutation).
// Explicit false -> mutation allowed.
func readDryRun(req mcp.CallToolRequest) (dryRun bool, explicitMutate bool, err error) {
	args := req.GetArguments()
	raw, present := args["dry_run"]
	if !present {
		return true, false, nil
	}
	b, ok := raw.(bool)
	if !ok {
		return true, false, fmt.Errorf("dry_run must be a boolean, got %T", raw)
	}
	// b==true  -> dry run, no mutation requested.
	// b==false -> caller explicitly opted into mutation.
	return b, !b, nil
}

func registerApplyPlan(srv *server.MCPServer, deps Deps) {
	opts := append([]mcp.ToolOption{
		mcp.WithDescription(applyPlanDescription),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithBoolean("skip_preflight",
			mcp.Description("Bypass the cluster preflight gate. Default false. Only set true when an operator has confirmed the cluster can host the RuntimeClass despite the findings."),
			mcp.DefaultBool(false),
		),
	}, commonApplyToolOptions()...)
	tool := mcp.NewTool("apply_plan", opts...)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		planPath, err := req.RequireString("plan_path")
		if err != nil {
			return toolErr(err)
		}
		dryRun, _, err := readDryRun(req)
		if err != nil {
			return toolErr(err)
		}
		applyOpts := agentmoat.ApplyOptions{
			KubeconfigPath: req.GetString("kubeconfig_path", ""),
			Context:        req.GetString("context", ""),
			PlanPath:       planPath,
			DryRun:         dryRun,
			EmitEvents:     req.GetBool("emit_events", true),
			AuditEnabled:   req.GetBool("audit_enabled", true),
			SkipPreflight:  req.GetBool("skip_preflight", false),
			Stderr:         deps.Stderr,
			KubeClient:     deps.KubeClient,
		}
		result, err := agentmoat.Apply(ctx, applyOpts)
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(result)
	})
}

func registerRollbackPlan(srv *server.MCPServer, deps Deps) {
	opts := append([]mcp.ToolOption{
		mcp.WithDescription(rollbackPlanDescription),
		mcp.WithDestructiveHintAnnotation(true),
	}, commonApplyToolOptions()...)
	tool := mcp.NewTool("rollback_plan", opts...)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		planPath, err := req.RequireString("plan_path")
		if err != nil {
			return toolErr(err)
		}
		dryRun, _, err := readDryRun(req)
		if err != nil {
			return toolErr(err)
		}
		rollbackOpts := agentmoat.RollbackOptions{
			KubeconfigPath: req.GetString("kubeconfig_path", ""),
			Context:        req.GetString("context", ""),
			PlanPath:       planPath,
			DryRun:         dryRun,
			EmitEvents:     req.GetBool("emit_events", true),
			AuditEnabled:   req.GetBool("audit_enabled", true),
			Stderr:         deps.Stderr,
			KubeClient:     deps.KubeClient,
		}
		result, err := agentmoat.Rollback(ctx, rollbackOpts)
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(result)
	})
}
