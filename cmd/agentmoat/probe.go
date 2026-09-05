// agentmoat probe: one-shot diagnostic pods. Today there is one probe,
// `probe nvproxy`.
//
// nvproxy is how gVisor runs CUDA workloads: the Sentry forwards the NVIDIA
// driver ioctls to the host driver, and it only does so for host driver
// versions the installed runsc knows. That list is compiled into runsc, so
// the only way to read it is to run `runsc nvproxy list-supported-drivers`
// on a gVisor node. This command creates a small pod pinned to such a node,
// mounts the runsc binary read-only from the host, runs it as an
// unprivileged user, reads the pod log, and deletes the pod. The result is
// a PreflightReport (like `agentmoat preflight`) with the GPU facts and the
// driver list under spec.facts.gpu.nvproxy; save it with --output json and
// pass it to `scan --facts` so the classifier can settle gpu-passthrough.
//
// This is the third verb that creates something in the cluster, after
// apply and rollback, and it follows the same rule: --dry-run defaults to
// true. A dry run reports the pod that would be created and exits.
//
// Exit codes (docs/exit-codes.md):
//   0  probe ran (or dry-run described it); the preflight has no error
//   1  generic error (kubeconfig, RBAC denied on pods, leftover probe pod)
//   5  not ready: the preflight has an error finding, so the probe did not run

package main

import (
	"context"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
	"github.com/0hardik1/agentmoat/pkg/probe"
)

// Probe-specific flag values.
var (
	flagProbeDryRun       bool
	flagProbeRuntimeClass string
	flagProbeNamespace    string
	flagProbeImage        string
	flagProbeRunscPath    string
	flagProbeTimeout      time.Duration
)

func newProbeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Run a one-shot diagnostic pod against the cluster (dry-run by default)",
		Long: `probe runs a short-lived pod to read something the API server cannot
tell you. Each probe defaults to --dry-run=true and describes the pod it
would create; pass --dry-run=false to create it. The pod is deleted when
the probe finishes.

Probes:

    nvproxy    read the runsc version and the NVIDIA driver versions its
               nvproxy supports, from a gVisor node`,
	}
	cmd.AddCommand(newProbeNvproxyCmd())
	return cmd
}

func newProbeNvproxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "nvproxy",
		Short: "Read the nvproxy supported-driver list from runsc on a gVisor node",
		Long: `probe nvproxy runs the cluster preflight, then creates one pod on a Ready
node that matches the RuntimeClass nodeSelector (preferring a GPU node),
mounts the host's runsc binary read-only, and runs:

    runsc --version
    runsc nvproxy list-supported-drivers

The pod runs under runc (not the RuntimeClass) as an unprivileged user with
no capabilities, and is deleted afterwards. The output is a PreflightReport
whose spec.facts.gpu block lists the GPU nodes (from GPU Feature Discovery
labels) with, for each card and driver, whether gVisor nvproxy supports it.

Feed the saved report to the classifier:

    agentmoat probe nvproxy --dry-run=false --output json > probe.json
    agentmoat scan --facts probe.json

A GPU workload whose card and driver the installed runsc supports is then
classified compatible; one on an unsupported card, a MIG-sliced node, or an
unlisted driver is classified incompatible. See docs/gpu-nvproxy.md.

The pod mounts a hostPath, which Pod Security Admission "baseline" rejects.
On clusters that enforce PSA, apply deploy/nvproxy-probe.yaml and pass
--probe-namespace agentmoat-probe. RBAC: create/get/delete on pods and get
on pods/log in that namespace, plus the preflight's node reads.`,
		RunE: runProbeNvproxy,
	}
	cmd.Flags().BoolVar(&flagProbeDryRun, "dry-run", true,
		"describe the probe pod without creating it; pass --dry-run=false to run the probe")
	cmd.Flags().StringVar(&flagProbeRuntimeClass, "runtime-class", "gvisor",
		"RuntimeClass whose nodes host the probe pod")
	cmd.Flags().StringVar(&flagProbeNamespace, "probe-namespace", probe.DefaultNamespace,
		"namespace for the probe pod (must allow hostPath under Pod Security Admission)")
	cmd.Flags().StringVar(&flagProbeImage, "image", probe.DefaultImage,
		"image for the probe pod; only /bin/sh is needed")
	cmd.Flags().StringVar(&flagProbeRunscPath, "runsc-path", probe.DefaultRunscPath,
		"path of the runsc binary on the node")
	cmd.Flags().DurationVar(&flagProbeTimeout, "timeout", probe.DefaultTimeout,
		"how long to wait for the probe pod to complete")
	return cmd
}

func runProbeNvproxy(cmd *cobra.Command, _ []string) error {
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

	report, err := agentmoat.Probe(context.Background(), agentmoat.ProbeOptions{
		KubeconfigPath:   flagKubeconfig,
		Context:          flagContext,
		RuntimeClassName: flagProbeRuntimeClass,
		Namespace:        flagProbeNamespace,
		Image:            flagProbeImage,
		RunscPath:        flagProbeRunscPath,
		Timeout:          flagProbeTimeout,
		DryRun:           flagProbeDryRun,
		Stderr:           stderr,
	})
	if err != nil {
		return err
	}

	if err := output.Render(report, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// Exit-code shaping per docs/exit-codes.md: the probe cannot run on a
	// cluster the preflight rejects, and the report says so -> 5.
	if !report.Spec.Summary.Ready {
		exitCode = 5
	}
	return nil
}
