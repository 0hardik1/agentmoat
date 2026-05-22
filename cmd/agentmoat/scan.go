// agentmoat scan: enumerate cluster workloads and report gVisor
// compatibility.
//
// Exit codes (docs/exit-codes.md):
//   0  no blocking compatibility issues
//   1  generic error (kubeconfig, network, etc.)
//   2  scan succeeded but at least one workload classified as Incompatible

package main

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

func newScanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Enumerate workloads and classify gVisor compatibility",
		Long: `scan walks the cluster via client-go, classifies each workload
against the built-in compatibility rules, and prints a report.

The default output is a human-readable table. Use --output json for a
versioned, stable schema suitable for piping into other tools or AI agents:

    agentmoat scan --output json | jq '.spec.workloads[] | select(.compatibility=="incompatible")'

scan is strictly read-only and never mutates cluster state.`,
		RunE: runScan,
	}
}

func runScan(cmd *cobra.Command, _ []string) error {
	// Parse the --output flag early so we fail before doing any K8s work.
	format, err := output.Parse(flagOutput)
	if err != nil {
		return err
	}

	// Build the render options once. NoColor is the OR of the explicit
	// flag and the NO_COLOR env (the renderer also checks the env, but
	// we OR here so the env decision is visible at the CLI level too).
	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}

	// --quiet swaps stderr for io.Discard so the orchestrator's progress
	// lines vanish without any orchestrator-side branching. The
	// orchestrator option docs (pkg/agentmoat/options.go) explicitly
	// name io.Discard as the silencing path.
	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	// Default behavior: if no namespace is specified, scan all namespaces.
	allNs := flagAllNamespaces || len(flagNamespaces) == 0

	opts := agentmoat.ScanOptions{
		KubeconfigPath: flagKubeconfig,
		Context:        flagContext,
		Namespaces:     flagNamespaces,
		AllNamespaces:  allNs,
		IncludeSystem:  flagIncludeSystem,
		LabelSelector:  flagLabelSelector,
		RulesYAMLPath:  flagRulesYAML,
		Stderr:         stderr,
	}

	report, err := agentmoat.Scan(context.Background(), opts)
	if err != nil {
		return err
	}

	// Render to stdout. Stderr already received progress messages.
	if err := output.Render(report, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// Exit-code shaping: if any workload is Incompatible, return code 2
	// so CI scripts and AI agents can branch on it.
	if hasIncompatible(report) {
		exitCode = 2
	}
	return nil
}

func hasIncompatible(report *schema.ScanReport) bool {
	for _, w := range report.Spec.Workloads {
		if w.Compatibility == schema.CompatibilityIncompatible {
			return true
		}
	}
	return false
}
