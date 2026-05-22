// Package agentmoat: Apply / Rollback orchestrators.
//
// Both functions are thin shells over pkg/applier. The orchestrator's job
// is to:
//
//   - build the Kubernetes client (same three-tier loader Scan uses).
//   - read the MigrationPlan from disk if PlanPath is set.
//   - stamp the apply result with the binary version.
//
// Per plan.md section 12.2, DryRun defaults to true at the orchestrator
// boundary as well. Callers (CLI, MCP server) must set ApplyOptions.DryRun
// to false explicitly to mutate the cluster.
package agentmoat

import (
	"context"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/applier"
)

// Apply executes a MigrationPlan against the live cluster (or, in dry-run,
// computes the patches without sending them).
//
// Source resolution: opts.PlanPath is read from disk and parsed via
// sigs.k8s.io/yaml (which accepts both JSON and YAML). If the file is
// missing or malformed, an error is returned before any K8s call.
func Apply(ctx context.Context, opts ApplyOptions) (*schema.ApplyResult, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	plan, err := loadPlan(opts.PlanPath)
	if err != nil {
		return nil, fmt.Errorf("apply: loading plan: %w", err)
	}

	client := opts.KubeClient
	if client == nil {
		cs, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
		if err != nil {
			return nil, fmt.Errorf("apply: building kubernetes client: %w", err)
		}
		client = cs
	}

	res, err := applier.Apply(ctx, applier.Options{
		Client:           client,
		Plan:             plan,
		DryRun:           opts.DryRun,
		EmitEvents:       opts.EmitEvents,
		AuditEnabled:     opts.AuditEnabled,
		Stderr:           stderr,
		Cluster:          kube.CurrentContext(opts.KubeconfigPath),
		AgentmoatVersion: Version,
	})
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}

	_, _ = fmt.Fprintf(stderr, "apply: %d applied, %d already-applied, %d failed (dry-run=%v)\n",
		res.Spec.Summary.Applied,
		res.Spec.Summary.AlreadyApplied,
		res.Spec.Summary.Failed,
		opts.DryRun,
	)
	return res, nil
}

// Rollback runs Apply's inverse: removes runtimeClassName and clears the
// namespace plan-hash annotation. See pkg/applier.Rollback for details.
func Rollback(ctx context.Context, opts RollbackOptions) (*schema.RollbackResult, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	plan, err := loadPlan(opts.PlanPath)
	if err != nil {
		return nil, fmt.Errorf("rollback: loading plan: %w", err)
	}

	client := opts.KubeClient
	if client == nil {
		cs, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
		if err != nil {
			return nil, fmt.Errorf("rollback: building kubernetes client: %w", err)
		}
		client = cs
	}

	res, err := applier.Rollback(ctx, applier.Options{
		Client:           client,
		Plan:             plan,
		DryRun:           opts.DryRun,
		EmitEvents:       opts.EmitEvents,
		AuditEnabled:     opts.AuditEnabled,
		Stderr:           stderr,
		Cluster:          kube.CurrentContext(opts.KubeconfigPath),
		AgentmoatVersion: Version,
	})
	if err != nil {
		return nil, fmt.Errorf("rollback: %w", err)
	}

	_, _ = fmt.Fprintf(stderr, "rollback: %d rolled back, %d already-applied, %d failed (dry-run=%v)\n",
		res.Spec.Summary.Applied,
		res.Spec.Summary.AlreadyApplied,
		res.Spec.Summary.Failed,
		opts.DryRun,
	)
	return res, nil
}

// loadPlan moved to loader.go (shared with Verify).
