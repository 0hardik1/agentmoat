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

	"github.com/0hardik1/agentmoat/pkg/output"
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

	// flagNoColor disables ANSI escapes in the table renderer even when
	// stdout is a TTY. The CLI honors this AND the NO_COLOR env var (per
	// no-color.org). See pkg/output/style.go: ColorEnabled.
	flagNoColor bool

	// flagQuiet suppresses orchestrator progress lines on stderr. Each
	// runX swaps cmd.ErrOrStderr() for io.Discard when this is set, which
	// the orchestrators document as the silencing path (pkg/agentmoat/
	// options.go: ScanOptions.Stderr et al.).
	flagQuiet bool
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

Subcommands ship: 'scan', 'preflight', 'plan', 'apply', 'rollback',
'verify', 'explain'. Every mutating command (apply, rollback) defaults to
--dry-run=true, and apply refuses to run when 'preflight' finds that no
node can host the migration (exit 5).

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
	// --no-color and --quiet do not have a short form: there is no widely-
	// accepted convention for them, and the existing short flags (-A, -l,
	// -n, -o) already cover the kubectl muscle memory.
	pf.BoolVar(&flagNoColor, "no-color", false,
		"disable ANSI color in the table output (also honors NO_COLOR env)")
	pf.BoolVarP(&flagQuiet, "quiet", "q", false,
		"suppress progress messages on stderr (good for scripts)")

	// Subcommands.
	root.AddCommand(newScanCmd())
	root.AddCommand(newPreflightCmd())
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
		// Style the error prefix when stderr is a colored TTY. We
		// deliberately keep the lowercase "error: " in the plain path to
		// match prior behavior (and the convention many Unix tools use);
		// the colored variant capitalizes "Error" because the visual
		// distinction is what carries the meaning when a human reads it.
		useColor := output.ColorEnabled(os.Stderr, flagNoColor)
		if useColor {
			s := output.NewStyles(true)
			// Bold red prefix, then the error message.
			prefix := s.Bold.Render(s.Danger.Render("Error:"))
			fmt.Fprintf(os.Stderr, "%s %s\n", prefix, err.Error())
		} else {
			fmt.Fprintf(os.Stderr, "error: %s\n", err.Error())
		}
		return 1
	}
	return exitCode
}

// exitCode is set by subcommands when they need a non-zero exit (e.g. the
// scan command sets it to 2 when blocking compatibility issues are found,
// preflight and apply set 5 when the cluster cannot host the migration).
var exitCode int
