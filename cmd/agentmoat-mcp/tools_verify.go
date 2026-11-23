// verify_migration tool: read-only "did the apply take?" check.
//
// Maps to agentmoat.Verify. The optional in_pod_probe field opts into the
// gVisor in-pod probe (uname / dmesg / /proc/cmdline inspection); without
// it the check is purely API-side (kubelet's pod runtimeClassName field).
//
// The optional wait field is the `--wait` flag of the CLI: a duration such
// as "3m". The tool call then does not return until every step is ok or the
// time ends, so the description tells the client to keep it short.
//
// With manifest_paths in place of plan_path the tool is `verify -f`: the
// workloads to check are the ones of the files that ask for the
// RuntimeClass. Standard input is not available here (it is the JSON-RPC
// channel), so a "-" path is an error.
//
// A report in which a workload could not be checked is not a pass. The CLI
// says that with exit code 1. A tool result has no exit code, so the report
// is the answer: summary.notChecked and spec.notChecked. The description
// tells the client to read them.
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
		mcp.WithDescription("Read-only check that workloads have actually landed on the gVisor RuntimeClass. Give ONE source: plan_path (the steps of a MigrationPlan, after apply_plan) or manifest_paths (after a change that came through a repository: the tool checks each workload of the files whose pod spec asks for the RuntimeClass). Returns a VerifyReport with per-step status (ok | mismatch | error). Set in_pod_probe=true to also exec a /proc-and-uname probe in one Running pod per step. Directly after apply_plan or a deploy, a rollout is in progress and one look reports a mismatch; set wait (for example \"2m\") to look again until every step is ok and its pods stay Ready. The report then has metadata.wait, and a step with pods that do not start says why in readiness. With manifest_paths: the files must be the objects as they were deployed (rendered; input with a kustomization file or an overlay patch is refused); namespace is the namespace of the objects that name none; summary.excluded counts the workloads that do not ask for the class and were not looked at; spec.notChecked lists the workloads that the tool could not find (a kind with no reader and no selector or with no pods for its selector, an object with generateName, a Job or a Pod that is not in the cluster, which a time to live or a hook delete policy can remove after a deploy that worked), and a report with summary.notChecked above 0 is NOT a pass even when no status is mismatch or error."),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name.")),
		mcp.WithString("plan_path",
			mcp.Description("Path to the MigrationPlan YAML/JSON file the apply was driven from. This or manifest_paths is required.")),
		mcp.WithArray("manifest_paths",
			mcp.Description("Manifest files or directories, in place of plan_path. The tool checks in the cluster each workload in them that asks for the RuntimeClass. Standard input is not available; pass rendered output as a file."),
			mcp.WithStringItems()),
		mcp.WithString("namespace",
			mcp.Description("With manifest_paths: the namespace of the objects that name none (the namespace that they were deployed to). Not a filter.")),
		mcp.WithString("label_selector",
			mcp.Description("With manifest_paths: check only the workloads whose labels match.")),
		mcp.WithString("runtime_class_name",
			mcp.Description("With manifest_paths: the RuntimeClass name to look for. Default 'gvisor'.")),
		mcp.WithBoolean("in_pod_probe",
			mcp.Description("If true, exec a small command in one Running pod per step to confirm the kernel reports gVisor markers. Slower; opt-in."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("wait",
			mcp.Description("Optional. A duration such as \"90s\" or \"3m\". Look again until every step is ok and its pods stay Ready, for at most this long. The call does not return before that, and a client can have its own time limit on a tool call, so keep the value below that limit. Empty or \"0\" looks one time.")),
		mcp.WithReadOnlyHintAnnotation(true),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		planPath := req.GetString("plan_path", "")
		paths := req.GetStringSlice("manifest_paths", nil)
		switch {
		case planPath == "" && len(paths) == 0:
			return toolErr(fmt.Errorf("verify_migration needs one source: plan_path, or manifest_paths"))
		case planPath != "" && len(paths) > 0:
			return toolErr(fmt.Errorf("verify_migration takes plan_path or manifest_paths, not the two"))
		}
		wait, err := parseWait(req.GetString("wait", ""))
		if err != nil {
			return toolErr(err)
		}
		opts := agentmoat.VerifyOptions{
			KubeconfigPath: req.GetString("kubeconfig_path", ""),
			Context:        req.GetString("context", ""),
			PlanPath:       planPath,
			// Stdin stays nil: a "-" path is then an error in the
			// library, and not a read of the protocol channel.
			ManifestPaths:    paths,
			Namespace:        req.GetString("namespace", ""),
			LabelSelector:    req.GetString("label_selector", ""),
			RuntimeClassName: req.GetString("runtime_class_name", ""),
			InPodProbe:       req.GetBool("in_pod_probe", false),
			Wait:             wait,
			Clock:            deps.Clock,
			Stderr:           deps.Stderr,
			KubeClient:       deps.KubeClient,
			RestConfig:       deps.RestConfig,
			ExecRunner:       deps.ExecRunner,
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
