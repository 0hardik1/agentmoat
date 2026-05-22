// agentmoat explain workload: deep explanation for one specific
// workload, addressed by `<namespace>/<name>` or
// `<namespace>/<kind>/<name>`.
//
// Output shape and exit codes are identical to `explain namespace`; the
// only difference is that the document is narrowed to a single workload
// before rendering. See explain_namespace.go for the rationale.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

func newExplainWorkloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "workload <namespace>/<name> | <namespace>/<kind>/<name>",
		Short: "Deeply explain compatibility for a single workload",
		Long: `Drill into one workload and render its compatibility verdict with
structured evidence and prose. The argument is "<namespace>/<name>" or,
when two controllers in the same namespace share a name,
"<namespace>/<kind>/<name>" to disambiguate.

Kind values are the controller kinds the scanner reports on:
Deployment, StatefulSet, DaemonSet, Job, CronJob, Pod. Kind matching is
case-sensitive (matches what 'agentmoat scan' shows in its first column).

Examples:

    agentmoat explain workload payments/api-gateway
    agentmoat explain workload payments/Deployment/api-gateway --output json

Exit code is 2 (same as scan) when the workload is incompatible.`,
		Args: cobra.ExactArgs(1),
		RunE: runExplainWorkload,
	}
}

// runExplainWorkload parses the positional reference, calls into the
// orchestrator's deep mode, renders, and shapes the exit code the same
// way `explain namespace` does.
func runExplainWorkload(cmd *cobra.Command, args []string) error {
	ns, filter, err := parseWorkloadRef(args[0])
	if err != nil {
		return err
	}

	format, err := output.Parse(flagOutput)
	if err != nil {
		return err
	}

	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}

	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	opts := agentmoat.ExplainOptions{
		Namespace: ns,
		Workload:  filter,
		Scan: agentmoat.ScanOptions{
			KubeconfigPath: flagKubeconfig,
			Context:        flagContext,
			IncludeSystem:  flagIncludeSystem,
			LabelSelector:  flagLabelSelector,
			RulesYAMLPath:  flagRulesYAML,
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

	if hasIncompatibleExplanation(doc) {
		exitCode = 2
	}
	return nil
}

// parseWorkloadRef accepts `<namespace>/<name>` or
// `<namespace>/<kind>/<name>` and returns the namespace plus a populated
// WorkloadFilter. The error message lists both valid forms so the
// operator can self-correct without reading the help text.
//
// We use strings.Split (not SplitN) so an input like "a/b/c/d" trips the
// default branch and produces a clear error, rather than silently
// merging the trailing segments into the name.
func parseWorkloadRef(s string) (string, *agentmoat.WorkloadFilter, error) {
	if s == "" {
		return "", nil, fmt.Errorf(
			"empty workload reference: expected <namespace>/<name> or <namespace>/<kind>/<name>",
		)
	}
	parts := strings.Split(s, "/")
	switch len(parts) {
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", nil, fmt.Errorf(
				"invalid workload reference %q: namespace and name must both be non-empty (expected <namespace>/<name> or <namespace>/<kind>/<name>)",
				s,
			)
		}
		return parts[0], &agentmoat.WorkloadFilter{Name: parts[1]}, nil
	case 3:
		if parts[0] == "" || parts[1] == "" || parts[2] == "" {
			return "", nil, fmt.Errorf(
				"invalid workload reference %q: namespace, kind, and name must all be non-empty (expected <namespace>/<name> or <namespace>/<kind>/<name>)",
				s,
			)
		}
		return parts[0], &agentmoat.WorkloadFilter{Kind: parts[1], Name: parts[2]}, nil
	default:
		return "", nil, fmt.Errorf(
			"invalid workload reference %q: expected <namespace>/<name> or <namespace>/<kind>/<name>",
			s,
		)
	}
}
