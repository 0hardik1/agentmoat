// probe_seccomp tool: run each migrated workload in a gVisor sandbox with
// its own seccomp profile and report the syscalls the profile refused.
//
// Maps to agentmoat.ProbeSeccomp. It creates pods (a trial per workload and
// a reader per node), so it uses the same gate as apply_plan, rollback_plan
// and probe_nvproxy: dry_run defaults to true and must be set to false
// explicitly.
package main

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/seccompprobe"
)

const probeSeccompDescription = "Run a trial copy of each migrated workload in a gVisor sandbox with the workload's own seccomp profile and report which syscalls the profile refused. Needed from gVisor 20261123.1, which loads the container's OCI seccomp filter inside the sandbox by default (before it, a RuntimeDefault profile filtered nothing under runsc). Workloads: the steps of plan_path, else the workloads in namespaces that already request runtime_class_name. Each trial uses RuntimeClass '" + seccompprobe.DefaultAuditRuntimeClass + "' (deploy/runtimeclass-seccomp-audit.yaml; its node handler logs seccomp denials), keeps containers, service account, capabilities and seccomp profiles, drops the workload's labels, replaces volume claims with emptyDir, runs for run_for_seconds, and is deleted; then a reader pod per node counts the denials. SAFETY: the trial runs the workload's real code. Defaults to dry-run (lists every pod and change, creates nothing); you MUST pass \"dry_run\": false to run the trials. Returns a PreflightReport: spec.facts.seccomp.workloads has per-workload status, profiles, runsc release and blocked syscalls; findings include seccomp-blocked-syscalls. Pass the saved JSON as facts_path to propose_plan to turn the warnings into plan warnings."

func registerProbeSeccomp(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("probe_seccomp",
		mcp.WithDescription(probeSeccompDescription),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file. Empty uses the standard search path, then in-cluster.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name. Empty uses the active context.")),
		mcp.WithString("plan_path",
			mcp.Description("Path to a MigrationPlan whose steps are the workloads to run. Empty: the workloads that already request runtime_class_name.")),
		mcp.WithArray("namespaces",
			mcp.Description("Namespaces to look for migrated workloads in, without plan_path. Empty means all."),
			mcp.WithStringItems()),
		mcp.WithString("runtime_class_name",
			mcp.Description("RuntimeClass the workloads migrate to. Default: the plan's, else 'gvisor'.")),
		mcp.WithString("audit_runtime_class_name",
			mcp.Description("RuntimeClass the trials request. Default '"+seccompprobe.DefaultAuditRuntimeClass+"'.")),
		mcp.WithNumber("run_for_seconds",
			mcp.Description("How long each trial runs once started. Default 60.")),
		mcp.WithNumber("timeout_seconds",
			mcp.Description("How long to wait for the trials to start, and for each reader pod. Default 180.")),
		mcp.WithString("reader_namespace",
			mcp.Description("Namespace for the reader pods; must allow hostPath under Pod Security Admission. Default '"+seccompprobe.DefaultReaderNamespace+"'.")),
		mcp.WithBoolean("dry_run",
			mcp.Description("If omitted or true, no pod is created and the report lists the pods that would be. Set explicitly to false to run the trials."),
			mcp.DefaultBool(true),
		),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dryRun, _, err := readDryRun(req)
		if err != nil {
			return toolErr(err)
		}
		seconds := func(name string) time.Duration {
			if s := req.GetFloat(name, 0); s > 0 {
				return time.Duration(s * float64(time.Second))
			}
			return 0
		}
		report, err := agentmoat.ProbeSeccomp(ctx, agentmoat.SeccompProbeOptions{
			KubeconfigPath:        req.GetString("kubeconfig_path", ""),
			Context:               req.GetString("context", ""),
			PlanPath:              req.GetString("plan_path", ""),
			Namespaces:            req.GetStringSlice("namespaces", nil),
			RuntimeClassName:      req.GetString("runtime_class_name", ""),
			AuditRuntimeClassName: req.GetString("audit_runtime_class_name", ""),
			RunFor:                seconds("run_for_seconds"),
			Timeout:               seconds("timeout_seconds"),
			ReaderNamespace:       req.GetString("reader_namespace", ""),
			DryRun:                dryRun,
			Stderr:                deps.Stderr,
			KubeClient:            deps.KubeClient,
		})
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(report)
	})
}
