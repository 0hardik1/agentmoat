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
	"encoding/json"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/applier"

	"sigs.k8s.io/yaml"
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

	client, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
	if err != nil {
		return nil, fmt.Errorf("apply: building kubernetes client: %w", err)
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

	fmt.Fprintf(stderr, "apply: %d applied, %d already-applied, %d failed (dry-run=%v)\n",
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

	client, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
	if err != nil {
		return nil, fmt.Errorf("rollback: building kubernetes client: %w", err)
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

	fmt.Fprintf(stderr, "rollback: %d rolled back, %d already-applied, %d failed (dry-run=%v)\n",
		res.Spec.Summary.Applied,
		res.Spec.Summary.AlreadyApplied,
		res.Spec.Summary.Failed,
		opts.DryRun,
	)
	return res, nil
}

// loadPlan reads a MigrationPlan from disk. The file may be either YAML or
// JSON; sigs.k8s.io/yaml round-trips through JSON internally so the same
// parser handles both.
//
// Returns a non-nil error for: missing path, file read failure, malformed
// document, wrong Kind (e.g. someone hands us a ScanReport).
func loadPlan(path string) (*schema.MigrationPlan, error) {
	if path == "" {
		return nil, fmt.Errorf("--plan is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Quick header sniff so a stale ScanReport produces a friendly error
	// rather than a partially-populated MigrationPlan.
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if envelope.Kind != schema.KindMigrationPlan {
		return nil, fmt.Errorf("plan file %s has kind %q, want %q",
			path, envelope.Kind, schema.KindMigrationPlan)
	}

	var plan schema.MigrationPlan
	if err := yaml.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	// json.Valid acts as a final shape check; an explicit re-marshal makes
	// the test "the file is parseable as a real MigrationPlan" cheap.
	if _, err := json.Marshal(plan); err != nil {
		return nil, fmt.Errorf("validating plan shape: %w", err)
	}
	return &plan, nil
}
