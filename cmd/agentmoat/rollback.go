// agentmoat rollback: undo a previously applied MigrationPlan.
//
// rollback removes runtimeClassName from each workload listed in the plan
// and clears the annotations of the plan (agentmoat.io/plan-hash and
// agentmoat.io/apply-started) on each affected namespace. It also undoes an
// apply that did not finish. Idempotent: a second rollback reports every
// step as already-applied. Defaults to --dry-run=true.
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
from each workload's pod template. The annotations of the plan
(agentmoat.io/plan-hash and agentmoat.io/apply-started) are cleared on
every affected namespace.

rollback acts on a namespace only when one of the two annotations has the
hash of the plan:

    plan-hash      the apply was complete: every step is undone
    apply-started  the apply did not finish (a step failed, or the process
                   was stopped): a step is undone when its workload has the
                   RuntimeClass of the plan now
    none           the namespace has no record of the plan: nothing is
                   changed, every step is reported as already-applied, and
                   stderr has a note

So a pipeline can run "apply, and on a failure rollback" with the same plan
file, also when the apply stopped in the middle.

The two annotations hold one plan. A later apply of another plan replaces
them, and a rollback with the older plan then changes nothing (its steps
are skipped, or already-applied when no annotation is left). To roll back
that older plan, apply it again first.

rollback has no record for each workload: it removes the RuntimeClass of
the plan from the workloads of the plan, also from one that had that class
before the apply. See docs/exit-codes.md.

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
	format, err := output.ParseFor(flagOutput, schema.KindRollbackResult)
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
