// Package agentmoat: Verify orchestrator.
//
// Verify is the read-only "did the apply actually take?" companion to Apply.
// It loads a MigrationPlan from disk (same loader as Apply / Rollback),
// builds the Kubernetes client (same three-tier loader as Scan), then hands
// the work to pkg/verifier. The verifier is what knows how to walk the plan,
// inspect pods, and optionally exec the in-pod gVisor probe.
//
// The orchestrator does not interpret Verify's per-step results. Exit-code
// shaping (exit 4 on any non-ok result, per docs/exit-codes.md) lives in the
// CLI; the MCP server may shape its own equivalent.
package agentmoat

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/verifier"
)

// Verify reads a MigrationPlan from disk and audits its live state on the
// cluster pointed at by the kubeconfig. The returned VerifyReport carries
// one VerifyResult per plan step plus a bucketed summary.
//
// Verify only returns a non-nil error for precondition failures (plan file
// missing or malformed, kubeconfig unloadable). Per-step problems show up in
// VerifyReport.Spec.Results so the renderer can present the full picture
// even when one workload is missing.
//
// With opts.Wait, Verify looks again until every step is ok or the time
// ends, and a step is ok only when its pods stay Ready (pkg/verifier,
// wait.go). The report then has metadata.wait, which says how the wait
// ended. A wait that timed out is not an error of Verify: the steps that
// are not ok say why, and the CLI makes that exit code 4.
func Verify(ctx context.Context, opts VerifyOptions) (*schema.VerifyReport, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Refuse a bad wait time before the plan file or the cluster is read.
	if opts.Wait < 0 {
		return nil, fmt.Errorf("verify: the wait time is %s; it must be zero or more", opts.Wait)
	}

	plan, err := loadPlan(opts.PlanPath)
	if err != nil {
		return nil, fmt.Errorf("verify: loading plan: %w", err)
	}

	// Honor the testing seam first; if no client was injected, build the
	// production one. The verifier also needs a *rest.Config when the
	// caller asks for the in-pod probe and did not supply a fake Exec
	// runner; we plumb both through.
	client := opts.KubeClient
	cfg := opts.RestConfig
	if client == nil {
		cs, restCfg, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
		if err != nil {
			return nil, fmt.Errorf("verify: building kubernetes client: %w", err)
		}
		client = cs
		cfg = restCfg
	}

	res, err := verifier.Verify(ctx, verifier.Options{
		Client:           client,
		Config:           cfg,
		Plan:             plan,
		InPodProbe:       opts.InPodProbe,
		Wait:             opts.Wait,
		Clock:            opts.Clock,
		Exec:             opts.ExecRunner,
		Stderr:           stderr,
		Cluster:          kube.CurrentContext(opts.KubeconfigPath, opts.Context),
		AgentmoatVersion: Version,
	})
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}

	// Stamp envelope fields that only the orchestrator can know. The
	// verifier could set these itself, but keeping them here mirrors the
	// pattern in apply.go and keeps the verifier framework-agnostic.
	if res.Metadata.GeneratedAt == "" {
		res.Metadata.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if res.Metadata.AgentmoatVersion == "" {
		res.Metadata.AgentmoatVersion = Version
	}

	_, _ = fmt.Fprintf(stderr, "verify: %d ok, %d mismatch, %d error (in-pod-probe=%v%s)\n",
		res.Spec.Summary.OK,
		res.Spec.Summary.Mismatch,
		res.Spec.Summary.Error,
		opts.InPodProbe,
		waitNote(res.Metadata.Wait),
	)
	// A plan with no steps gives "0 ok, 0 mismatch" and exit 0. That is
	// correct (the second run of a pipeline with no new workload gets
	// here), but it must not read as "everything is in the sandbox": the
	// verify looked at no workload. The usual cause is a plan from a scan
	// after the workloads got the class: the planner makes no step for a
	// workload that has a RuntimeClass.
	if len(plan.Spec.Steps) == 0 {
		_, _ = fmt.Fprintln(stderr, "verify: nothing was verified: the plan has no steps. "+
			"A plan has a step only for a workload that had no RuntimeClass at the time of the scan. "+
			"To check workloads after a change, use the plan from before the change.")
	}
	return res, nil
}

// waitNote is the part of the stderr summary that says how the wait ended.
// It is empty for a verify with no wait time.
func waitNote(w *schema.VerifyWait) string {
	if w == nil {
		return ""
	}
	requested := time.Duration(w.RequestedSeconds) * time.Second
	waited := time.Duration(w.WaitedSeconds) * time.Second
	switch {
	case w.Interrupted:
		return fmt.Sprintf(", wait of %s interrupted after %s", requested, waited)
	case w.TimedOut:
		return fmt.Sprintf(", wait of %s timed out", requested)
	default:
		return fmt.Sprintf(", waited %s of %s", waited, requested)
	}
}
