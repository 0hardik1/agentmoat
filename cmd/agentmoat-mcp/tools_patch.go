// patch_manifests tool: add runtimeClassName to manifest files.
//
// One-to-one mapping with agentmoat.Patch, the function behind `agentmoat
// patch`. A coding agent that works in a repository is a natural user: it
// calls the tool as a dry-run, shows the diff, and writes the files when the
// person agrees.
//
// The tool reads and writes local files only. It contacts no cluster, and
// the handler does not pass the Kubernetes client of the server on:
// PatchOptions has no field for one.
//
// Safety contract: the same as apply_plan. The tool writes no file unless
// the caller sets "dry_run": false. Omitting the field, or sending true,
// returns the result with the diff and changes nothing. readDryRun (in
// tools_apply.go) reads the raw arguments, so a value that is not a boolean
// is an error and not a write.
package main

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

// patchManifestsDescription says what the tool changes, and the safety gate,
// next to the schema.
const patchManifestsDescription = "Add runtimeClassName to the manifest files of the workloads that can run in gVisor, so that the move goes through a pull request and the deploy pipeline of the repository ('git revert' is the rollback). Contacts no cluster. SAFETY: defaults to dry-run; you MUST pass \"dry_run\": false explicitly to write the files. It reads the manifests as scan_cluster does with manifest_paths, selects the workloads that are compatible and have no RuntimeClass (with include_review, also the ones that need review), and adds one line to the pod spec of each. Nothing else in a file changes (comments, key order, and quotes stay), and a second call changes nothing. Returns a PatchResult (apiVersion: agentmoat.io/v1alpha1, kind: PatchResult): spec.changes has the status of each selected workload (patched | not-edited | failed) with file and line, spec.excluded has the workloads that were not selected with the reason, and spec.files has the unified diff of each file. Not edited, and listed with the reason: JSON, flow-style YAML, and a pod spec behind a YAML anchor. Helm templates cannot be edited (set the chart value instead). With Kustomize it edits the files it is given and applies no overlay (metadata.source.unrendered lists the signs); afterwards, scan the rendered output with scan_cluster."

func registerPatchManifests(srv *server.MCPServer, deps Deps) {
	tool := mcp.NewTool("patch_manifests",
		mcp.WithDescription(patchManifestsDescription),
		mcp.WithArray("manifest_paths",
			mcp.Required(),
			mcp.Description("Manifest files or directories to edit (YAML; a directory is searched for *.yaml, *.yml, *.json). Standard input is not available: the tool edits files."),
			mcp.WithStringItems(),
		),
		mcp.WithBoolean("dry_run",
			mcp.Description("If omitted or true, no file is written and the result has the diff. Set explicitly to false to write the files."),
			mcp.DefaultBool(true),
		),
		mcp.WithBoolean("include_review",
			mcp.Description("Also patch the workloads that need review. Default false."),
			mcp.DefaultBool(false),
		),
		mcp.WithString("runtime_class_name",
			mcp.Description("The RuntimeClass name to write. Default 'gvisor'.")),
		mcp.WithString("namespace",
			mcp.Description("The namespace for objects that name none. It is not a filter, and it is not written to a file.")),
		mcp.WithString("label_selector",
			mcp.Description("Kubernetes-style label selector: only the workloads whose labels match are patched. Empty means no filter.")),
		mcp.WithString("rules_yaml_path",
			mcp.Description("Optional path to a classifier rules YAML override (layered on top of the built-in rule set).")),
		mcp.WithString("facts_path",
			mcp.Description("Path to a saved PreflightReport or ScanReport whose cluster facts the classifier uses, as for scan_cluster. GPU workloads are then classified against the real driver list.")),
		// The tool writes files, so it is not read-only. Its change is one
		// added line for each workload (nothing is removed or replaced), and
		// a second call with the same input changes nothing.
		mcp.WithReadOnlyHintAnnotation(false),
		mcp.WithDestructiveHintAnnotation(false),
		mcp.WithIdempotentHintAnnotation(true),
		mcp.WithOpenWorldHintAnnotation(false),
	)

	srv.AddTool(tool, func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		paths, err := req.RequireStringSlice("manifest_paths")
		if err != nil {
			return toolErr(err)
		}
		dryRun, _, err := readDryRun(req)
		if err != nil {
			return toolErr(err)
		}
		result, err := agentmoat.Patch(ctx, agentmoat.PatchOptions{
			ManifestPaths:    paths,
			Namespace:        req.GetString("namespace", ""),
			LabelSelector:    req.GetString("label_selector", ""),
			RulesYAMLPath:    req.GetString("rules_yaml_path", ""),
			FactsPath:        req.GetString("facts_path", ""),
			RuntimeClassName: req.GetString("runtime_class_name", ""),
			IncludeReview:    req.GetBool("include_review", false),
			DryRun:           dryRun,
			Stderr:           deps.Stderr,
		})
		if err != nil {
			return toolErr(err)
		}
		return marshalResult(result)
	})
}
