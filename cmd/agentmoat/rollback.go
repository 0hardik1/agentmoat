// agentmoat rollback: undo a previously applied MigrationPlan.
//
// rollback removes runtimeClassName from each workload listed in the plan
// and clears the agentmoat.io/plan-hash annotation on each affected
// namespace. Idempotent: a second rollback reports every step as
// already-applied. Defaults to --dry-run=true.
//
// Exit codes (docs/exit-codes.md):
//   0  rollback complete (or dry-run preview)
//   1  generic error (kubeconfig, malformed plan, all-failed)
//   3  partial rollback (some steps rolled back, others failed)

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

// Rollback flag values. Re-uses the same set as apply for symmetry.
var (
	flagRollbackPlanPath string
	flagRollbackDryRun   bool
	flagRollbackNoEvents bool
	flagRollbackNoAudit  bool
)

func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Rollback a previously applied MigrationPlan (mutating; default dry-run)",
		Long: `rollback walks the plan in reverse order and removes runtimeClassName
from each workload's pod template. The agentmoat.io/plan-hash annotation
is cleared on every affected namespace.

By default --dry-run=true: the patches are computed and reported but no
mutation is sent to the API server. Pass --dry-run=false to mutate.

NOTE: rollback does NOT remove the runtime=gvisor:NoSchedule toleration
that apply injects. That is intentional: with the matching taint absent
from any node, the toleration is harmless, and removing the *specific*
toleration via JSON Patch indices would be fragile if other admission
controllers re-ordered the list since apply ran.`,
		RunE: runRollback,
	}
	cmd.Flags().StringVar(&flagRollbackPlanPath, "plan", "",
		"path to the MigrationPlan that was originally applied. Required.")
	cmd.Flags().BoolVar(&flagRollbackDryRun, "dry-run", true,
		"if true (default), compute the rollback patches but do not mutate the cluster")
	cmd.Flags().BoolVar(&flagRollbackNoEvents, "no-events", false,
		"do not emit Kubernetes Events per mutation")
	cmd.Flags().BoolVar(&flagRollbackNoAudit, "no-audit", false,
		"do not append to ~/.agentmoat/audit.jsonl")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

func runRollback(cmd *cobra.Command, _ []string) error {
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

	opts := agentmoat.RollbackOptions{
		KubeconfigPath: flagKubeconfig,
		Context:        flagContext,
		PlanPath:       flagRollbackPlanPath,
		DryRun:         flagRollbackDryRun,
		EmitEvents:     !flagRollbackNoEvents,
		AuditEnabled:   !flagRollbackNoAudit,
		Stderr:         stderr,
	}

	res, err := agentmoat.Rollback(context.Background(), opts)
	if err != nil {
		return err
	}

	if err := output.Render(res, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	if hasRollbackFailures(res) {
		if res.Spec.Summary.Applied > 0 || res.Spec.Summary.AlreadyApplied > 0 {
			exitCode = 3
		} else {
			exitCode = 1
		}
	}
	return nil
}

func hasRollbackFailures(res *schema.RollbackResult) bool {
	return res.Spec.Summary.Failed > 0
}
