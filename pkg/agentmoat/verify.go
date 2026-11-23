// Package agentmoat: Verify orchestrator.
//
// Verify is the read-only "did the apply actually take?" companion to Apply.
// It loads a MigrationPlan from disk (same loader as Apply / Rollback),
// builds the Kubernetes client (same three-tier loader as Scan), then hands
// the work to pkg/verifier. The verifier is what knows how to walk the plan,
// inspect pods, and optionally exec the in-pod gVisor probe.
//
// With manifest paths in place of a plan (`verify -f`), the workloads to
// check come from the files: see verify_manifests.go.
//
// VerifyExitCode is the exit code of a report (docs/exit-codes.md). It is
// here and not in cmd/, so that the CLI and the MCP server agree.
package agentmoat

import (
	"context"
	"fmt"
	"io"
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

	// The list of what to check comes from a plan or from manifest files.
	// It is read, and refused when it is not usable, before the client is
	// built: an error in the input must not show as a kubeconfig error.
	plan, input, err := verifySource(opts, stderr)
	if err != nil {
		return nil, err
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

	var targets []verifier.Target
	if input != nil {
		targets = input.targets
	}
	res, err := verifier.Verify(ctx, verifier.Options{
		Client:           client,
		Config:           cfg,
		Plan:             plan,
		Targets:          targets,
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

	if input != nil {
		input.stamp(res)
	}

	_, _ = fmt.Fprintf(stderr, "verify: %d ok, %d mismatch, %d error (in-pod-probe=%v%s)\n",
		res.Spec.Summary.OK,
		res.Spec.Summary.Mismatch,
		res.Spec.Summary.Error,
		opts.InPodProbe,
		waitNote(res.Metadata.Wait),
	)
	if input != nil {
		describeManifestVerify(stderr, res)
		return res, nil
	}
	describeEmptyPlan(stderr, plan)
	return res, nil
}

// verifySource reads the list of what to check: a plan, or (with manifest
// paths) the workloads of the files. Exactly one of the two results is not
// nil. It reads no cluster.
func verifySource(opts VerifyOptions, stderr io.Writer) (*schema.MigrationPlan, *verifyInput, error) {
	switch {
	case opts.PlanPath != "" && len(opts.ManifestPaths) > 0:
		return nil, nil, fmt.Errorf("verify: a plan and manifest paths are given; a verify has one source")
	case len(opts.ManifestPaths) > 0:
		input, err := readVerifyTargets(opts, stderr)
		if err != nil {
			return nil, nil, err
		}
		return nil, input, nil
	default:
		plan, err := loadPlan(opts.PlanPath)
		if err != nil {
			return nil, nil, fmt.Errorf("verify: loading plan: %w", err)
		}
		return plan, nil, nil
	}
}

// describeEmptyPlan says on stderr that a verify of a plan with no steps
// looked at no workload. Such a run gives "0 ok, 0 mismatch" and exit 0.
// That is correct (the second run of a pipeline with no new workload gets
// here), but it must not read as "everything is in the sandbox". The usual
// cause is a plan from a scan after the workloads got the class: the
// planner makes no step for a workload that has a RuntimeClass.
func describeEmptyPlan(stderr io.Writer, plan *schema.MigrationPlan) {
	if len(plan.Spec.Steps) > 0 {
		return
	}
	_, _ = fmt.Fprintln(stderr, "verify: nothing was verified: the plan has no steps. "+
		"A plan has a step only for a workload that had no RuntimeClass at the time of the scan. "+
		"To check workloads after a change that came through manifest files, give the files: verify -f <files>.")
}

// describeManifestVerify says on stderr what a verify from manifests looked
// at, and what it did not. A workload that asks for the class and has no
// verdict makes the run "not a pass"; say so in words, because the exit code
// for it (1) is the code of an error and has no message of its own.
//
// The lines have counts only. The names and the reasons are text from a
// manifest; the report on stdout has them, and its renderers make such text
// safe for a terminal and for Markdown. stderr has no renderer.
func describeManifestVerify(stderr io.Writer, res *schema.VerifyReport) {
	s := res.Spec.Summary
	_, _ = fmt.Fprintf(stderr, "verify: %d workload(s) of the input ask for RuntimeClass %q; %d do not and were not looked at\n",
		s.Total+s.NotChecked, res.Metadata.RuntimeClassName, s.Excluded)
	if s.NotChecked == 0 {
		return
	}
	_, _ = fmt.Fprintf(stderr, "verify: %d workload(s) that ask for the class could not be checked. "+
		"This is not a pass. It is also not evidence that the deploy failed: verify has no verdict for them. "+
		"The report lists each one with the reason (spec.notChecked in JSON and YAML)\n", s.NotChecked)
}

// Exit codes of a verify (docs/exit-codes.md).
const (
	// VerifyExitOK: each workload that was asked about is ok.
	VerifyExitOK = 0
	// VerifyExitNotChecked: no result is a failure, and one workload or
	// more has no verdict. Not a pass, and not a rollback signal.
	VerifyExitNotChecked = 1
	// VerifyExitFailed: one result or more is a mismatch or an error. With
	// a wait time this is the rollback signal.
	VerifyExitFailed = 4
)

// VerifyExitCode returns the exit code for a report. A failure comes first:
// evidence in the cluster that a workload does not run in the sandbox is the
// rollback signal, also when another workload could not be checked.
func VerifyExitCode(r *schema.VerifyReport) int {
	s := r.Spec.Summary
	switch {
	case s.Mismatch > 0 || s.Error > 0:
		return VerifyExitFailed
	case s.NotChecked > 0:
		return VerifyExitNotChecked
	default:
		return VerifyExitOK
	}
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
