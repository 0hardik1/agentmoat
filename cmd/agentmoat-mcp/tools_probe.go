// probe_nvproxy tool: the nvproxy driver probe.
//
// Maps to agentmoat.Probe. This is the third tool that can create anything
// in the cluster (after apply_plan and rollback_plan) and it uses the same
// safety gate: dry_run defaults to true and must be set to false explicitly.
// A dry run reports the pod that would be created; a real run creates one
// pod on a gVisor node, reads the runsc version and nvproxy driver list from
// its log, and deletes it.
//
// The result is a PreflightReport with metadata.probe and, after a real
// run, spec.facts.gpu.nvproxy. Save the JSON to a file and pass it as
// facts_path to scan_cluster / assess_workload / propose_plan so the
// gpu-passthrough verdict is settled from the real driver list.
package main

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/probe"
)

const probeNvproxyDescription = "Read which NVIDIA host driver versions the installed runsc's nvproxy supports, by running a one-shot pod on a gVisor node that mounts the host runsc binary read-only and runs 'runsc nvproxy list-supported-drivers'. SAFETY: defaults to dry-run (describes the pod, creates nothing); you MUST pass \"dry_run\": false to create the pod, which is deleted afterwards. Returns a PreflightReport: spec.facts.gpu lists the GPU nodes (from GPU Feature Discovery labels) with per-card and per-driver support verdicts, metadata.probe records the pod. Save the JSON and pass it as facts_path to scan_cluster to classify GPU workloads against the real driver list."

func registerProbeNvproxy(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("probe_nvproxy",
		mcp.WithDescription(probeNvproxyDescription),
		mcp.WithDestructiveHintAnnotation(true),
		mcp.WithString("kubeconfig_path",
			mcp.Description("Path to a kubeconfig file. Empty uses the standard search path, then in-cluster.")),
		mcp.WithString("context",
			mcp.Description("Kubeconfig context name. Empty uses the active context.")),
		mcp.WithString("runtime_class_name",
			mcp.Description("RuntimeClass whose nodes host the probe pod. Default 'gvisor'.")),
		mcp.WithString("namespace",
			mcp.Description("Namespace for the probe pod; must allow hostPath under Pod Security Admission (deploy/nvproxy-probe.yaml ships one). Default '"+probe.DefaultNamespace+"'.")),
		mcp.WithString("image",
			mcp.Description("Image for the probe pod; only /bin/sh is needed. Default '"+probe.DefaultImage+"'.")),
		mcp.WithString("runsc_path",
			mcp.Description("Path of the runsc binary on the node. Default '"+probe.DefaultRunscPath+"'.")),
		mcp.WithNumber("timeout_seconds",
			mcp.Description("How long to wait for the probe pod to complete. Default 120.")),
		mcp.WithBoolean("dry_run",
			mcp.Description("If omitted or true, no pod is created and the report describes the pod that would be. Set explicitly to false to run the probe."),
			mcp.DefaultBool(true),
		),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		dryRun, _, err := readDryRun(req)
		if err != nil {
			return toolErr(err)
		}
		var timeout time.Duration
		if secs := req.GetFloat("timeout_seconds", 0); secs > 0 {
			timeout = time.Duration(secs * float64(time.Second))
		}
		report, err := agentmoat.Probe(ctx, agentmoat.ProbeOptions{
			KubeconfigPath:   req.GetString("kubeconfig_path", ""),
			Context:          req.GetString("context", ""),
			RuntimeClassName: req.GetString("runtime_class_name", ""),
			Namespace:        req.GetString("namespace", ""),
			Image:            req.GetString("image", ""),
			RunscPath:        req.GetString("runsc_path", ""),
			Timeout:          timeout,
			DryRun:           dryRun,
			Stderr:           deps.Stderr,
			KubeClient:       deps.KubeClient,
		})
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(report)
	})
}
