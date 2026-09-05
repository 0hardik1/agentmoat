// agentmoat explain namespace: deep, per-workload explanation for every
// workload in a single namespace.
//
// This is the online sibling of `agentmoat explain <topic>`: where the
// topic mode prints embedded prose with no cluster work, this mode
// performs the same read-only API calls that `agentmoat scan` makes,
// classifies every workload in the named namespace, and produces a
// document that shows (per workload):
//
//   - the verdict and recommendation
//   - the rules that fired, with structured evidence from the PodSpec
//     (volumes, capabilities, images, etc.) and operator-facing prose
//   - for compatible verdicts, the list of rules that were checked but
//     did not fire, so the audit trail is visible
//
// Exit codes (docs/exit-codes.md):
//   0  every workload compatible (or only review-level findings)
//   1  generic error (kubeconfig, network, etc.)
//   2  at least one workload is incompatible (matches `scan`)
//
// The command is strictly read-only: it never patches, applies, or
// mutates cluster state.

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

func newExplainNamespaceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "namespace <name>",
		Short: "Deeply explain compatibility for every workload in a namespace",
		Long: `Run scan + classify against the named namespace and render a deep,
per-workload explanation: which rules fired, what concrete evidence
(volumes, capabilities, images) triggered them, and operator-facing prose
on why each finding matters. Compatible workloads also show what was
checked, so the operator can see the rule sweep.

This command makes the same read-only API calls as 'agentmoat scan'.

Examples:

    agentmoat explain namespace payments
    agentmoat explain namespace payments --output json
    agentmoat explain namespace payments -l team=core

Exit code is 2 (same as scan) when the namespace contains at least one
incompatible workload, so CI scripts and AI agents can branch on it.`,
		Args: cobra.ExactArgs(1),
		RunE: runExplainNamespace,
	}
}

// runExplainNamespace dispatches the global flags into ExplainOptions
// (deep mode) and renders the resulting envelope. Exit-code shaping
// mirrors `agentmoat scan`: any incompatible workload trips exit 2.
func runExplainNamespace(cmd *cobra.Command, args []string) error {
	// Parse --output early so we fail before any K8s work.
	format, err := output.Parse(flagOutput)
	if err != nil {
		return err
	}

	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}

	// --quiet swaps stderr for io.Discard. Mirrors scan/plan/apply/etc.
	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	opts := agentmoat.ExplainOptions{
		Namespace: args[0],
		Scan: agentmoat.ScanOptions{
			KubeconfigPath: flagKubeconfig,
			Context:        flagContext,
			IncludeSystem:  flagIncludeSystem,
			LabelSelector:  flagLabelSelector,
			RulesYAMLPath:  flagRulesYAML,
			FactsPath:      flagExplainFactsPath,
			Stderr:         stderr,
		},
	}

	doc, err := agentmoat.Explain(context.Background(), opts)
	if err != nil {
		return err
	}

	if err := output.Render(doc, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// Exit-code shaping: if any workload in the namespace is
	// Incompatible, return code 2 (same as scan). This lets a CI job
	// `agentmoat explain namespace X` and branch on the result without
	// also running scan.
	if hasIncompatibleExplanation(doc) {
		exitCode = 2
	}
	return nil
}

// hasIncompatibleExplanation returns true when at least one workload in
// the deep document is Incompatible. Returns false for static-topic
// documents (Spec.Namespace nil), so a topic-mode invocation never
// shapes the exit code from this helper.
func hasIncompatibleExplanation(doc *schema.ExplainDocument) bool {
	if doc == nil || doc.Spec.Namespace == nil {
		return false
	}
	for _, wl := range doc.Spec.Namespace.Workloads {
		if wl.Compatibility == schema.CompatibilityIncompatible {
			return true
		}
	}
	return false
}
