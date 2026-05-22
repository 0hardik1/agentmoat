// agentmoat root command and global flags.
//
// All subcommands inherit these flags. The CLI follows kubectl conventions
// (--kubeconfig, --context, --namespace) so K8s operators do not need to
// learn new ergonomics.

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// Global flag values. Each subcommand reads from these; we keep them at
// package scope rather than threading through context for brevity.
var (
	flagOutput        string
	flagKubeconfig    string
	flagContext       string
	flagNamespaces    []string
	flagAllNamespaces bool
	flagLabelSelector string
	flagIncludeSystem bool
	flagRulesYAML     string
	flagExplain       bool
)

// newRootCmd builds the root cobra.Command and attaches subcommands.
// We construct it in a function (not as a global) so unit tests can build
// fresh instances without leaked state.
func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "agentmoat",
		Short: "Read-only assessment and one-shot migration toolkit: runc -> gVisor",
		Long: `agentmoat scans a Kubernetes cluster, classifies workloads by
gVisor compatibility, plans and applies a migration to RuntimeClass gvisor,
verifies the result, and rolls back on demand. It also embeds the gVisor
educational docs so operators can read them without leaving the terminal.

Subcommands ship: 'scan', 'plan', 'apply', 'rollback', 'verify', 'explain'.
Every mutating command (apply, rollback) defaults to --dry-run=true.

Documentation: see docs/ in the repo or run 'agentmoat explain <topic>'.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	// Global flags shared by all subcommands.
	pf := root.PersistentFlags()
	pf.StringVarP(&flagOutput, "output", "o", "table",
		"output format: table | json | yaml")
	pf.StringVar(&flagKubeconfig, "kubeconfig", "",
		"path to kubeconfig file (default: $KUBECONFIG or ~/.kube/config)")
	pf.StringVar(&flagContext, "context", "",
		"kubeconfig context to use (default: current-context)")
	pf.StringSliceVarP(&flagNamespaces, "namespace", "n", nil,
		"namespace(s) to scan (repeatable). Default: all namespaces")
	pf.BoolVarP(&flagAllNamespaces, "all-namespaces", "A", false,
		"scan all namespaces (default true when --namespace is not set)")
	pf.StringVarP(&flagLabelSelector, "selector", "l", "",
		"Kubernetes label selector applied to every list call")
	pf.BoolVar(&flagIncludeSystem, "include-system", false,
		"include kube-system and other kube-* namespaces (default false)")
	pf.StringVar(&flagRulesYAML, "rules", "",
		"path to classifier rules YAML override (default: built-in only)")
	pf.BoolVar(&flagExplain, "explain", false,
		"add inline educational notes to output where supported")

	// Subcommands.
	root.AddCommand(newScanCmd())
	root.AddCommand(newPlanCmd())
	root.AddCommand(newApplyCmd())
	root.AddCommand(newRollbackCmd())
	root.AddCommand(newVerifyCmd())
	root.AddCommand(newExplainCmd())
	root.AddCommand(newVersionCmd())

	return root
}

// Execute runs the root command and returns the desired process exit code.
// Documented in docs/exit-codes.md.
func Execute() int {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		return 1
	}
	return exitCode
}

// exitCode is set by subcommands when they need a non-zero exit (e.g. the
// scan command sets it to 2 when blocking compatibility issues are found).
var exitCode int
