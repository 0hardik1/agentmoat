// agentmoat preflight: can this cluster schedule pods that request the
// target RuntimeClass? Read-only.
//
// The per-workload verdicts (scan) say whether a workload can run under
// gVisor. Nothing there can notice that the cluster has no gVisor node,
// that the RuntimeClass steers pods nowhere, or that every node is an EKS
// Auto Mode instance on which runsc can never be installed. preflight is
// that check. `apply` runs the same check before every step and refuses to
// proceed on an error finding; this verb lets you run it on day zero.
//
// Exit codes (docs/exit-codes.md):
//   0  ready: no error-severity finding (warnings and info may exist)
//   1  generic error (kubeconfig, RBAC denied on nodes / runtimeclasses)
//   5  not ready: at least one error-severity finding

package main

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

// Preflight-specific flag values.
var flagPreflightRuntimeClass string

func newPreflightCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Check that the cluster can schedule pods onto gVisor nodes (read-only)",
		Long: `preflight reads one RuntimeClass and the node list and answers: can a
pod that requests this RuntimeClass actually start somewhere?

It checks that the RuntimeClass exists, that its scheduling.nodeSelector
matches at least one Ready node, that those nodes' taints are tolerated
by the RuntimeClass, and that the candidate nodes are not EKS Auto Mode
or Bottlerocket instances (AWS-managed, immutable, no runsc). Findings
have stable IDs and a remediation each; see docs/preflight.md.

'agentmoat apply' runs this same check first and refuses to patch anything
when it reports an error (exit 5). Run preflight on its own before you
plan, or in CI, to catch a cluster that is not ready yet.

preflight is strictly read-only. It needs get/list on nodes and
node.k8s.io/runtimeclasses (deploy/clusterrole-readonly.yaml).`,
		RunE: runPreflight,
	}
	cmd.Flags().StringVar(&flagPreflightRuntimeClass, "runtime-class", "gvisor",
		"RuntimeClass name to inspect")
	return cmd
}

func runPreflight(cmd *cobra.Command, _ []string) error {
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

	report, err := agentmoat.Preflight(context.Background(), agentmoat.PreflightOptions{
		KubeconfigPath:   flagKubeconfig,
		Context:          flagContext,
		RuntimeClassName: flagPreflightRuntimeClass,
		Stderr:           stderr,
	})
	if err != nil {
		return err
	}

	if err := output.Render(report, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// Exit-code shaping per docs/exit-codes.md: not ready -> 5.
	if !report.Spec.Summary.Ready {
		exitCode = 5
	}
	return nil
}
