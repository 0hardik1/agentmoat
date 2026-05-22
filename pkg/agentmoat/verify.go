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
func Verify(ctx context.Context, opts VerifyOptions) (*schema.VerifyReport, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	plan, err := loadPlan(opts.PlanPath)
	if err != nil {
		return nil, fmt.Errorf("verify: loading plan: %w", err)
	}

	client, cfg, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
	if err != nil {
		return nil, fmt.Errorf("verify: building kubernetes client: %w", err)
	}

	res, err := verifier.Verify(ctx, verifier.Options{
		Client:           client,
		Config:           cfg,
		Plan:             plan,
		InPodProbe:       opts.InPodProbe,
		Stderr:           stderr,
		Cluster:          kube.CurrentContext(opts.KubeconfigPath),
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

	_, _ = fmt.Fprintf(stderr, "verify: %d ok, %d mismatch, %d error (in-pod-probe=%v)\n",
		res.Spec.Summary.OK,
		res.Spec.Summary.Mismatch,
		res.Spec.Summary.Error,
		opts.InPodProbe,
	)
	return res, nil
}
