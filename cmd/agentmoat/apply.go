// agentmoat apply: execute a MigrationPlan against the cluster.
//
// `apply` defaults to `--dry-run=true` per plan.md section 12.2. Mutating
// the cluster requires the operator to set `--dry-run=false` explicitly.
//
// Exit codes (docs/exit-codes.md):
//   0  fully successful apply (or fully dry-run preview)
//   1  generic error (kubeconfig, malformed plan, etc.)
//   3  partial apply (some steps applied, others failed)

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

// Apply-specific flag values.
var (
	flagApplyPlanPath string
	flagApplyDryRun   bool
	flagApplyNoEvents bool
	flagApplyNoAudit  bool
)

func newApplyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Apply a MigrationPlan to the cluster (mutating; default dry-run)",
		Long: `apply walks a MigrationPlan and patches each workload's pod template
with the target RuntimeClass and the matching toleration. Idempotent:
re-running an applied plan reports every step as already-applied and exits 0.

By default --dry-run=true: the patches are computed and reported but no
mutation is sent to the API server. Pass --dry-run=false to mutate.

The apply writes the plan hash to each affected namespace as the
'agentmoat.io/plan-hash' annotation, emits one Kubernetes Event per
mutation, and appends one line per mutation to ~/.agentmoat/audit.jsonl.`,
		RunE: runApply,
	}
	cmd.Flags().StringVar(&flagApplyPlanPath, "plan", "",
		"path to a MigrationPlan file (JSON or YAML). Required.")
	cmd.Flags().BoolVar(&flagApplyDryRun, "dry-run", true,
		"if true (default), compute the patches but do not mutate the cluster")
	cmd.Flags().BoolVar(&flagApplyNoEvents, "no-events", false,
		"do not emit Kubernetes Events per mutation")
	cmd.Flags().BoolVar(&flagApplyNoAudit, "no-audit", false,
		"do not append to ~/.agentmoat/audit.jsonl")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

func runApply(cmd *cobra.Command, _ []string) error {
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

	opts := agentmoat.ApplyOptions{
		KubeconfigPath: flagKubeconfig,
		Context:        flagContext,
		PlanPath:       flagApplyPlanPath,
		DryRun:         flagApplyDryRun,
		EmitEvents:     !flagApplyNoEvents,
		AuditEnabled:   !flagApplyNoAudit,
		Stderr:         stderr,
	}

	res, err := agentmoat.Apply(context.Background(), opts)
	if err != nil {
		return err
	}

	if err := output.Render(res, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// Exit-code shaping per docs/exit-codes.md:
	//   - Any failure with at least one applied step -> 3 (partial).
	//   - All failed -> 1 (treated as generic apply error). The renderer
	//     already showed the per-step errors; the caller sees the exit code.
	//   - Otherwise 0.
	if hasApplyFailures(res) {
		if res.Spec.Summary.Applied > 0 || res.Spec.Summary.AlreadyApplied > 0 {
			exitCode = 3
		} else {
			exitCode = 1
		}
	}
	return nil
}

func hasApplyFailures(res *schema.ApplyResult) bool {
	return res.Spec.Summary.Failed > 0
}
