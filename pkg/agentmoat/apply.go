// Package agentmoat: Apply / Rollback orchestrators.
//
// Both functions are thin shells over pkg/applier. The orchestrator's job
// is to:
//
//   - build the Kubernetes client (same three-tier loader Scan uses).
//   - read the MigrationPlan from disk if PlanPath is set.
//   - run the cluster preflight (pkg/preflight) and refuse to apply when
//     it finds an error: no RuntimeClass, a RuntimeClass that steers pods
//     nowhere, no Ready runsc node, or only EKS Auto Mode / Bottlerocket
//     nodes. A blocked apply is an ApplyResult with every step skipped and
//     Metadata.Preflight.Ready=false, not a Go error, so the JSON output
//     carries the findings and the CLI can exit 5.
//   - stamp the apply result with the binary version.
//
// DryRun defaults to true at the orchestrator boundary as well: dry-run is
// the safe default, so callers (CLI, MCP server) must set ApplyOptions.DryRun
// to false explicitly to mutate the cluster.
package agentmoat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/applier"
	"github.com/0hardik1/agentmoat/pkg/preflight"
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
	cluster := kube.CurrentContext(opts.KubeconfigPath, opts.Context)

	// Preflight gate. Runs in dry-run too: a preview that says "8 applied"
	// against a cluster with no gVisor node would be a lie. The gate is a
	// read of one RuntimeClass and the node list, so it is cheap.
	var pf *schema.PreflightReport
	if !opts.SkipPreflight {
		pf, err = preflight.Run(ctx, preflight.Options{
			Client:           client,
			RuntimeClassName: plan.Spec.Options.RuntimeClassName,
			Cluster:          cluster,
			AgentmoatVersion: Version,
		})
		if err != nil {
			return nil, fmt.Errorf("apply: preflight: %w (pass --skip-preflight to bypass)", err)
		}
		if !pf.Spec.Summary.Ready {
			_, _ = fmt.Fprintf(stderr, "apply: blocked by preflight (%d error finding(s)); nothing was mutated. "+
				"Run 'agentmoat preflight' for details, or pass --skip-preflight to override.\n",
				pf.Spec.Summary.Error)
			return blockedApplyResult(plan, pf, opts.DryRun, cluster), nil
		}
	}

	res, err := applier.Apply(ctx, applier.Options{
		Client:           client,
		Plan:             plan,
		DryRun:           opts.DryRun,
		EmitEvents:       opts.EmitEvents,
		AuditEnabled:     opts.AuditEnabled,
		Stderr:           stderr,
		Cluster:          cluster,
		AgentmoatVersion: Version,
	})
	if err != nil {
		return nil, fmt.Errorf("apply: %w", err)
	}
	attachPreflight(res, pf)

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
		Cluster:          kube.CurrentContext(opts.KubeconfigPath, opts.Context),
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

// attachPreflight copies the preflight summary and findings onto a
// successful ApplyResult so the output explains the warnings (for example
// "some matching nodes are tainted") next to the steps. No-op when the
// preflight was skipped.
func attachPreflight(res *schema.ApplyResult, pf *schema.PreflightReport) {
	if res == nil || pf == nil {
		return
	}
	summary := pf.Spec.Summary
	res.Metadata.Preflight = &summary
	res.Spec.PreflightFindings = pf.Spec.Findings
}

// blockedApplyResult shapes the "preflight said no" outcome as a regular
// ApplyResult: every step is skipped with the same reason, the summary
// counts them as skipped, and the preflight findings ride along. Keeping
// it a result (not an error) means `--output json` consumers and the MCP
// client get the findings in the same document they already parse.
func blockedApplyResult(plan *schema.MigrationPlan, pf *schema.PreflightReport, dryRun bool, cluster string) *schema.ApplyResult {
	summary := pf.Spec.Summary
	ids := make([]string, 0, summary.Error)
	for _, f := range pf.Spec.Findings {
		if f.Severity == schema.SeverityError {
			ids = append(ids, f.ID)
		}
	}
	reason := "blocked by preflight: " + strings.Join(ids, ", ")

	steps := make([]schema.StepResult, 0, len(plan.Spec.Steps))
	for _, st := range plan.Spec.Steps {
		steps = append(steps, schema.StepResult{
			Order:  st.Order,
			Target: st.Target,
			Status: schema.StepStatusSkipped,
			Error:  reason,
		})
	}

	res := schema.NewApplyResult()
	res.Metadata = schema.ApplyMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          cluster,
		AgentmoatVersion: Version,
		PlanHash:         plan.Metadata.PlanHash,
		DryRun:           dryRun,
		Preflight:        &summary,
	}
	res.Spec = schema.ApplySpec{
		Summary:           schema.ApplySummary{Total: len(steps), Skipped: len(steps)},
		Steps:             steps,
		PreflightFindings: pf.Spec.Findings,
	}
	return res
}

// loadPlan moved to loader.go (shared with Verify).
