// agentmoat verify: audit a previously-applied MigrationPlan against the
// live cluster. Read-only.
//
// Exit codes (docs/exit-codes.md):
//   0  every step's live state matches the plan
//   1  generic error (kubeconfig, plan file missing, etc.)
//   4  at least one step reports mismatch or error
//
// With --wait, verify looks again until every step is ok or the time ends.
// This is the form for a pipeline, directly after `apply --dry-run=false`:
// one look at that moment finds a rollout in progress and reports a
// mismatch for a migration that is correct. With a wait, a step is ok only
// when its pods are also Ready and stay Ready, so exit 4 after a wait means
// "roll back", not "look again later".
//
// The in-pod probe (--in-pod-probe) opts into a per-step `kubectl exec`
// that greps for distinctive gVisor markers in dmesg, /proc/cmdline, and
// uname output. It needs the `pods/exec` permission; skip it in CI loops
// where the field-only check is enough.

package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

// flag* identifiers stay in root.go for the global flags; subcommand-local
// flags live in the subcommand's own file (mirrors plan.go / apply.go).
var (
	flagVerifyPlanPath   string
	flagVerifyInPodProbe bool
	flagVerifyWait       time.Duration
)

func newVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Audit a previously-applied MigrationPlan against the live cluster",
		Long: `verify reads a MigrationPlan from disk and inspects the live
cluster to confirm every step actually landed:

  - For each PlanStep, the verifier resolves the target workload and
    reads each backing pod's .spec.runtimeClassName.
  - The result for each step is ok / mismatch / error. The bucketed
    summary drives the exit code (4 on any non-ok, per docs/exit-codes.md).

With --in-pod-probe, the verifier also execs a small script inside one
Running pod per step and asserts the stdout contains gVisor markers.
This requires the 'pods/exec' permission; leave it off in CI where the
field-only check is enough.

With --wait <duration> (for example --wait 3m), verify looks again every
few seconds until every step is ok or the time ends. Use it in a pipeline
directly after 'apply --dry-run=false', when the rollout is in progress:

  agentmoat apply --plan plan.json --dry-run=false
  agentmoat verify --plan plan.json --wait 3m || agentmoat rollback --plan plan.json --dry-run=false

With a wait, a step is ok only when its pods are also Ready and stay Ready
for 10 seconds with no restart. A pod that has the RuntimeClass and stops at
start (the program needs something that gVisor does not have) is a mismatch,
with the reason in the message. A workload that has no pods now (replicas: 0,
a CronJob between two runs) is checked on its pod template and does not use
the wait time. Progress goes to stderr; -q removes it.

verify is strictly read-only and never mutates the cluster.`,
		RunE: runVerify,
	}
	cmd.Flags().StringVar(&flagVerifyPlanPath, "plan", "",
		"path to the MigrationPlan file (JSON or YAML). Required.")
	cmd.Flags().BoolVar(&flagVerifyInPodProbe, "in-pod-probe", false,
		"exec a gVisor-detection probe inside one Running pod per step (requires pods/exec)")
	cmd.Flags().DurationVar(&flagVerifyWait, "wait", 0,
		"look again until every step is ok and its pods stay Ready, for at most this long (for example 3m). 0 looks one time")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

func runVerify(cmd *cobra.Command, _ []string) error {
	format, err := output.ParseFor(flagOutput, schema.KindVerifyReport)
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

	opts := agentmoat.VerifyOptions{
		KubeconfigPath: flagKubeconfig,
		Context:        flagContext,
		PlanPath:       flagVerifyPlanPath,
		InPodProbe:     flagVerifyInPodProbe,
		Wait:           flagVerifyWait,
		Stderr:         stderr,
	}

	// No signal handler here, on purpose. An interrupt (Ctrl-C, or the
	// SIGTERM of a job that the CI system cancels) must end the process by
	// the signal. A shell goes on with the right side of
	//
	//	agentmoat verify --wait 3m || agentmoat rollback --dry-run=false
	//
	// when the left side handled the signal and then returned an exit
	// code, and a wait that was cut short always has a step that is not
	// ok (exit 4). Ctrl-C on a healthy rollout would then start a live
	// rollback. A process that dies by the signal makes the shell stop
	// the full list. The library does return the last complete look when
	// its context is canceled (metadata.wait.interrupted); that is for
	// callers that own their context, such as the MCP server.
	res, err := agentmoat.Verify(context.Background(), opts)
	if err != nil {
		return err
	}

	if err := output.Render(res, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// Exit-code shaping per docs/exit-codes.md: any non-ok result triggers
	// exit 4 so CI scripts and AI agents can branch on it.
	if hasVerifyFailures(res) {
		exitCode = 4
	}
	return nil
}

// hasVerifyFailures returns true when at least one VerifyResult is not OK.
// We use the summary counts (cheap, already populated) rather than
// re-walking the slice.
func hasVerifyFailures(r *schema.VerifyReport) bool {
	return r.Spec.Summary.Mismatch > 0 || r.Spec.Summary.Error > 0
}
