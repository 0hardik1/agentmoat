// agentmoat probe seccomp: run each migrated workload in a gVisor sandbox
// with its own seccomp profile and report the syscalls the profile refused.
//
// From gVisor release 20261123.1, runsc loads the container's OCI seccomp
// filter inside the sandbox by default (--oci-seccomp defaults to true).
// Before it, a RuntimeDefault profile under runsc filtered nothing, so a
// workload that runs fine on gVisor today can get EPERM after its nodes
// move to 20261123.1. See pkg/seccompprobe for how the trial works.
//
// Exit codes (docs/exit-codes.md):
//   0  probe ran (or dry-run described it); the preflight has no error
//   1  generic error (kubeconfig, plan file, RBAC)
//   5  not ready: the preflight has an error finding, so no trial ran

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
	"github.com/0hardik1/agentmoat/pkg/seccompprobe"
)

var (
	flagSeccompDryRun          bool
	flagSeccompPlan            string
	flagSeccompRuntimeClass    string
	flagSeccompAuditClass      string
	flagSeccompRunFor          time.Duration
	flagSeccompTimeout         time.Duration
	flagSeccompLogDir          string
	flagSeccompReaderNamespace string
	flagSeccompReaderImage     string
)

func newProbeSeccompCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seccomp",
		Short: "Run each migrated workload under its own seccomp profile in gVisor and report blocked syscalls",
		Long: `probe seccomp runs a trial copy of each migrated workload in a gVisor
sandbox, with the workload's own seccomp profile, and reports the syscalls
the profile refused inside the sandbox.

Why: from gVisor release 20261123.1, runsc loads the container's OCI
seccomp filter inside the sandbox by default (runsc --oci-seccomp defaults
to true). Before it, a RuntimeDefault or Localhost profile filtered nothing
under runsc. A workload that runs on gVisor today can start failing with
EPERM the day its nodes move to 20261123.1.

Which workloads: the steps of --plan, or, without --plan, the workloads in
--namespace (all namespaces when omitted) that already request
--runtime-class.

How: each trial is the workload's pod template with RuntimeClass
gvisor-seccomp-audit, whose node handler runs runsc with oci-seccomp on and
the Sentry debug log written under --log-dir. The trial keeps the
containers, images, service account, capabilities and every seccomp
profile, but drops the workload's labels (so it gets no Service traffic and
no controller adopts it), runs with restartPolicy Never, and replaces
persistent volume claims with emptyDir. After --run-for the trials are
deleted, and a reader pod on each node that ran one (under runc, the log
directory mounted read-only) counts the denials for this run.

Setup: apply deploy/runtimeclass-seccomp-audit.yaml, and give the gVisor
nodes its handler (the kind node image and the agentmoat AMI ship it; see
docs/preflight.md#seccomp-probe). The reader mounts a hostPath, so
--reader-namespace must allow privileged pods under Pod Security Admission.

The trial runs the workload's real code: whatever it does on start, the
trial does too. That is why --dry-run defaults to true; a dry run lists
every pod it would create and every change it would make to the template.

    agentmoat plan --output json > plan.json
    agentmoat probe seccomp --plan plan.json                  # dry run
    agentmoat probe seccomp --plan plan.json --dry-run=false --output json > seccomp.json
    agentmoat plan --facts seccomp.json                       # warnings include blocked syscalls`,
		RunE: runProbeSeccomp,
	}
	f := cmd.Flags()
	f.BoolVar(&flagSeccompDryRun, "dry-run", true,
		"describe the trial and reader pods without creating them; pass --dry-run=false to run the trials")
	f.StringVar(&flagSeccompPlan, "plan", "",
		"MigrationPlan whose steps are the workloads to run (default: workloads that already request --runtime-class)")
	f.StringVar(&flagSeccompRuntimeClass, "runtime-class", "",
		"RuntimeClass the workloads migrate to (default: the plan's, else gvisor)")
	f.StringVar(&flagSeccompAuditClass, "audit-runtime-class", seccompprobe.DefaultAuditRuntimeClass,
		"RuntimeClass the trials request; its handler must run runsc with oci-seccomp and the Sentry debug log on")
	f.DurationVar(&flagSeccompRunFor, "run-for", seccompprobe.DefaultRunFor,
		"how long each trial runs once its containers started")
	f.DurationVar(&flagSeccompTimeout, "timeout", seccompprobe.DefaultTimeout,
		"how long to wait for the trials to start, and for each reader pod to finish")
	f.StringVar(&flagSeccompLogDir, "log-dir", seccompprobe.DefaultLogDir,
		"node directory of the Sentry debug logs (debug-log in runsc-seccomp-audit.toml)")
	f.StringVar(&flagSeccompReaderNamespace, "reader-namespace", seccompprobe.DefaultReaderNamespace,
		"namespace for the reader pods (must allow hostPath under Pod Security Admission)")
	f.StringVar(&flagSeccompReaderImage, "reader-image", seccompprobe.DefaultReaderImage,
		"image for the reader pods; needs /bin/sh, grep and awk")
	return cmd
}

func runProbeSeccomp(cmd *cobra.Command, _ []string) error {
	format, err := output.ParseFor(flagOutput, schema.KindPreflightReport)
	if err != nil {
		return err
	}
	renderOpts := output.RenderOptions{NoColor: flagNoColor || os.Getenv("NO_COLOR") != ""}
	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	report, err := agentmoat.ProbeSeccomp(context.Background(), agentmoat.SeccompProbeOptions{
		KubeconfigPath:        flagKubeconfig,
		Context:               flagContext,
		PlanPath:              flagSeccompPlan,
		Namespaces:            flagNamespaces,
		RuntimeClassName:      flagSeccompRuntimeClass,
		AuditRuntimeClassName: flagSeccompAuditClass,
		RunFor:                flagSeccompRunFor,
		Timeout:               flagSeccompTimeout,
		LogDir:                flagSeccompLogDir,
		ReaderNamespace:       flagSeccompReaderNamespace,
		ReaderImage:           flagSeccompReaderImage,
		DryRun:                flagSeccompDryRun,
		Stderr:                stderr,
	})
	if err != nil {
		return err
	}
	if err := output.Render(report, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}
	if !report.Spec.Summary.Ready {
		exitCode = 5
	}
	return nil
}
