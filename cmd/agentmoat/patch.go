// `agentmoat patch`: add runtimeClassName to manifest files.
//
// A thin shell over agentmoat.Patch. The command reads flags, calls the
// library, renders the result, and sets the exit code from
// agentmoat.PatchExitCode. It builds no Kubernetes client.
//
// Exit codes (docs/exit-codes.md):
//
//	0  every selected workload is patched, or none was selected
//	1  an error, or no selected workload could be patched
//	3  some selected workloads are patched and some are not
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

var (
	flagPatchFiles         []string
	flagPatchDryRun        bool
	flagPatchIncludeReview bool
	flagPatchRuntimeClass  string
	flagPatchFactsPath     string
)

func newPatchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "patch -f <file or directory>",
		Short: "Add runtimeClassName to the manifest files of workloads that can run in gVisor",
		Long: `patch makes the move to gVisor as a change to your manifest files, so that
a pull request can review it, your pipeline can deploy it, and 'git revert'
can undo it. It reads no cluster.

It reads the manifests as 'scan -f' does, and selects the workloads that are
compatible and have no RuntimeClass (with --include-review, also the ones
that need review). For each one it adds one line to the pod spec:

    spec:
      runtimeClassName: gvisor      <- the added line
      containers:

Nothing else in a file changes: comments, the order of keys, and quotes stay
as they are. The command checks each edit before it uses it: it reads the
new text again, and requires that the selected workloads have the class and
that nothing else is different.

By default this is a dry-run: it prints the diff and writes no file. Pass
--dry-run=false to write the files. A second run changes nothing, because
the workloads then have the class.

    agentmoat patch -f ./k8s                       # show the diff
    agentmoat patch -f ./k8s --dry-run=false       # write the files
    agentmoat scan -f ./k8s --fail-on sandboxed    # check the result

What is not edited, and is listed with the reason (exit code 3):

  - JSON, and flow-style YAML ({...}).
  - A pod spec behind a YAML anchor or alias.
  - A pod spec that already has a runtimeClassName key with no name as its
    value.

What patch does not do:

  - It does not edit Helm templates. A template is not YAML before it is
    rendered. Set the chart value for runtimeClassName.
  - It does not replace another RuntimeClass.
  - With Kustomize, it edits the files that it is given (the bases). The
    verdicts describe those files, not what an overlay makes of them. Scan
    the rendered result afterwards:
    kustomize build <dir> | agentmoat scan -f - --fail-on sandboxed

A namespace (-n) is the default for objects that name none, as for 'scan
-f'. It is not a filter and it is not written to a file. -l limits the
patch to the workloads whose labels match.`,
		RunE: runPatch,
	}
	cmd.Flags().StringArrayVarP(&flagPatchFiles, "filename", "f", nil,
		"the manifests to edit: a file or a directory (repeatable). Required.")
	cmd.Flags().BoolVar(&flagPatchDryRun, "dry-run", true,
		"if true (default), show the diff and write no file")
	cmd.Flags().BoolVar(&flagPatchIncludeReview, "include-review", false,
		"also patch the workloads that need review")
	cmd.Flags().StringVar(&flagPatchRuntimeClass, "runtime-class", "gvisor",
		"the RuntimeClass name to write")
	cmd.Flags().StringVar(&flagPatchFactsPath, "facts", "",
		"load cluster facts from a saved PreflightReport or ScanReport, as for 'scan -f --facts'")
	return cmd
}

func runPatch(cmd *cobra.Command, _ []string) error {
	format, err := output.ParseFor(flagOutput, schema.KindPatchResult)
	if err != nil {
		return err
	}
	if len(flagPatchFiles) == 0 {
		return fmt.Errorf("patch needs the manifests to edit: -f <file or directory> (repeatable)")
	}
	if len(flagNamespaces) > 1 {
		return fmt.Errorf("patch takes one namespace (the default for objects that name none), not %d: %s",
			len(flagNamespaces), strings.Join(flagNamespaces, ", "))
	}

	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}
	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	opts := agentmoat.PatchOptions{
		ManifestPaths:    flagPatchFiles,
		LabelSelector:    flagLabelSelector,
		RulesYAMLPath:    flagRulesYAML,
		FactsPath:        flagPatchFactsPath,
		RuntimeClassName: flagPatchRuntimeClass,
		IncludeReview:    flagPatchIncludeReview,
		DryRun:           flagPatchDryRun,
		Stderr:           stderr,
	}
	if len(flagNamespaces) == 1 {
		opts.Namespace = flagNamespaces[0]
	}

	// Flags that only mean something for a cluster. Say that they were not
	// used, as 'scan -f' does; an error would be unkind to a wrapper script
	// that always passes --context.
	var unused []string
	for _, name := range []string{"kubeconfig", "context", "all-namespaces", "include-system"} {
		if cmd.Flags().Changed(name) {
			unused = append(unused, "--"+name)
		}
	}
	if len(unused) > 0 {
		_, _ = fmt.Fprintf(stderr, "note: patch edits manifests and reads no cluster; not used: %s\n", strings.Join(unused, ", "))
	}

	res, err := agentmoat.Patch(cmd.Context(), opts)
	if err != nil {
		return err
	}
	if err := output.Render(res, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}
	exitCode = agentmoat.PatchExitCode(res)
	return nil
}
