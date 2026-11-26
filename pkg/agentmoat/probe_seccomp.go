// Package agentmoat: the seccomp probe orchestrator.
//
// ProbeSeccomp runs pkg/seccompprobe: a trial copy of each migrated
// workload in a gVisor sandbox with its own seccomp profile, then a read of
// the Sentry log for the syscalls that profile refused. It matters from
// gVisor release 20261123.1, which loads the container's OCI seccomp filter
// inside the sandbox by default. Like apply, rollback and the nvproxy
// probe, it creates something in the cluster, so the CLI and the MCP tool
// default DryRun to true.
//
// The result is a PreflightReport with Metadata.SeccompProbe and
// Spec.Facts.Seccomp. Saved with --output json and passed to `plan --facts`
// (or `scan --facts`), its warn findings become plan warnings.
package agentmoat

import (
	"context"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/seccompprobe"
)

// ProbeSeccomp runs the seccomp probe. A trial that could not complete is a
// finding in the report, not an error; only precondition failures
// (kubeconfig, the plan file, RBAC on the workload list) return one.
func ProbeSeccomp(ctx context.Context, opts SeccompProbeOptions) (*schema.PreflightReport, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	var targets []schema.WorkloadRef
	runtimeClass := opts.RuntimeClassName
	if opts.PlanPath != "" {
		plan, err := loadPlan(opts.PlanPath)
		if err != nil {
			return nil, fmt.Errorf("probe seccomp: %w", err)
		}
		for _, step := range plan.Spec.Steps {
			targets = append(targets, step.Target)
		}
		if runtimeClass == "" {
			runtimeClass = plan.Spec.Options.RuntimeClassName
		}
		if len(targets) == 0 {
			_, _ = fmt.Fprintf(stderr, "probe seccomp: plan %s has no steps\n", opts.PlanPath)
		}
	}

	client := opts.KubeClient
	if client == nil {
		cs, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
		if err != nil {
			return nil, fmt.Errorf("probe seccomp: building kubernetes client: %w", err)
		}
		client = cs
	}

	if opts.DryRun {
		_, _ = fmt.Fprintln(stderr, "probe seccomp: dry run, no pod will be created (pass --dry-run=false to run the trials)")
	}
	report, err := seccompprobe.Run(ctx, seccompprobe.Options{
		Client:                client,
		RuntimeClassName:      runtimeClass,
		AuditRuntimeClassName: opts.AuditRuntimeClassName,
		Targets:               targets,
		Namespaces:            opts.Namespaces,
		RunFor:                opts.RunFor,
		Timeout:               opts.Timeout,
		LogDir:                opts.LogDir,
		ReaderNamespace:       opts.ReaderNamespace,
		ReaderImage:           opts.ReaderImage,
		DryRun:                opts.DryRun,
		Cluster:               kube.CurrentContext(opts.KubeconfigPath, opts.Context),
		AgentmoatVersion:      Version,
	})
	if err != nil {
		return nil, err
	}

	counts := map[string]int{}
	blocked := 0
	if sf := report.Spec.Facts.Seccomp; sf != nil {
		for _, w := range sf.Workloads {
			counts[w.Status]++
			if len(w.Blocked) > 0 {
				blocked++
			}
		}
	}
	_, _ = fmt.Fprintf(stderr, "probe seccomp: %d ran (%d with blocked syscalls), %d failed, %d skipped, %d dry-run\n",
		counts[schema.SeccompTrialRan], blocked, counts[schema.SeccompTrialFailed], counts[schema.SeccompTrialSkipped], counts[schema.SeccompTrialDryRun])
	sum := report.Spec.Summary
	_, _ = fmt.Fprintf(stderr, "preflight: RuntimeClass %q ready=%v (%d error, %d warn, %d info)\n",
		report.Metadata.RuntimeClassName, sum.Ready, sum.Error, sum.Warn, sum.Info)
	return report, nil
}
