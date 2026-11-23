// verify_migration tool: read-only "did the apply take?" check.
//
// Maps to agentmoat.Verify. The optional in_pod_probe field opts into the
// gVisor in-pod probe (uname / dmesg / /proc/cmdline inspection); without
// it the check is purely API-side (kubelet's pod runtimeClassName field).
//
// The optional wait field is the `--wait` flag of the CLI: a duration such
// as "3m". The tool call then does not return until every step is ok or the
// time ends, so the description tells the client to keep it short.
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

func registerVerifyMigration(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("verify_migration",
		mcp.WithDescription("Read-only check that the workloads in a MigrationPlan have actually landed on the gVisor RuntimeClass. Returns a VerifyReport with per-step status (ok | mismatch | error). Set in_pod_probe=true to also exec a /proc-and-uname probe in one Running pod per step. Directly after apply_plan a rollout is in progress and one look reports a mismatch; set wait (for example \"2m\") to look again until every step is ok and its pods stay Ready. The report then has metadata.wait, and a step with pods that do not start says why in readiness."),
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
		mcp.WithString("wait",
			mcp.Description("Optional. A duration such as \"90s\" or \"3m\". Look again until every step is ok and its pods stay Ready, for at most this long. The call does not return before that, and a client can have its own time limit on a tool call, so keep the value below that limit. Empty or \"0\" looks one time.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		planPath, err := req.RequireString("plan_path")
		if err != nil {
			return toolErr(err)
		}
		wait, err := parseWait(req.GetString("wait", ""))
		if err != nil {
			return toolErr(err)
		}
		opts := agentmoat.VerifyOptions{
			KubeconfigPath: req.GetString("kubeconfig_path", ""),
			Context:        req.GetString("context", ""),
			PlanPath:       planPath,
			InPodProbe:     req.GetBool("in_pod_probe", false),
			Wait:           wait,
			Clock:          deps.Clock,
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

// parseWait reads the wait input. Empty means no wait. The text is a Go
// duration ("90s", "3m"), the same form that the --wait flag of the CLI
// takes, so a value moves between the two with no change.
func parseWait(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("wait: %q is not a duration; use a form such as \"90s\" or \"3m\"", s)
	}
	return d, nil
}
