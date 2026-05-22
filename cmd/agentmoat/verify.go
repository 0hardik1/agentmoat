// agentmoat verify: audit a previously-applied MigrationPlan against the
// live cluster. Read-only.
//
// Exit codes (docs/exit-codes.md):
//   0  every step's live state matches the plan
//   1  generic error (kubeconfig, plan file missing, etc.)
//   4  at least one step reports mismatch or error
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

verify is strictly read-only and never mutates the cluster.`,
		RunE: runVerify,
	}
	cmd.Flags().StringVar(&flagVerifyPlanPath, "plan", "",
		"path to the MigrationPlan file (JSON or YAML). Required.")
	cmd.Flags().BoolVar(&flagVerifyInPodProbe, "in-pod-probe", false,
		"exec a gVisor-detection probe inside one Running pod per step (requires pods/exec)")
	_ = cmd.MarkFlagRequired("plan")
	return cmd
}

func runVerify(cmd *cobra.Command, _ []string) error {
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

	opts := agentmoat.VerifyOptions{
		KubeconfigPath: flagKubeconfig,
		Context:        flagContext,
		PlanPath:       flagVerifyPlanPath,
		InPodProbe:     flagVerifyInPodProbe,
		Stderr:         stderr,
	}

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
