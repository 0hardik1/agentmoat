// Package agentmoat: the Plan orchestrator.
//
// Plan is the public-facing wrapper around pkg/planner.Plan. Its job is to
// resolve the input ScanReport (either supplied or computed by running
// Scan() inline) and then hand it to the pure planner function. The
// orchestrator owns:
//
//   - resolving the source (in-memory ScanReport vs running a Scan).
//   - stamping the produced MigrationPlan with the agentmoat binary version.
//   - attaching Spec.Warnings derived from the scan's ClusterFacts (see
//     warnings.go) and from the deploy tool markers on the workloads (see
//     managed.go). The planner itself stays a pure function of workloads
//     and options; warnings never enter the plan hash.
//
// Both the CLI and the MCP server call into this so the resulting
// MigrationPlan is identical regardless of operator surface.
package agentmoat

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/planner"
)

// Plan produces a MigrationPlan for a cluster. The source is
// opts.ScanReport when it is non-nil, else the file at opts.ScanReportPath
// when that is set, else an inline Scan(opts.ScanOptions).
//
// The returned plan has its Metadata stamped with the current binary
// version and GeneratedAt timestamp. The planner's pure function does the
// actual partitioning/ordering/hashing.
func Plan(ctx context.Context, opts PlanOptions) (*schema.MigrationPlan, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	report, err := planReport(ctx, opts, stderr)
	if err != nil {
		return nil, err
	}

	plan, err := planner.Plan(report, opts.Planner)
	if err != nil {
		return nil, fmt.Errorf("plan: %w", err)
	}

	// Stamp envelope provenance the orchestrator (not the pure planner)
	// owns. AgentmoatVersion / Cluster were not set by the planner.
	plan.Metadata.GeneratedAt = time.Now().UTC().Format(time.RFC3339)
	if plan.Metadata.AgentmoatVersion == "" {
		plan.Metadata.AgentmoatVersion = Version
	}

	// Cluster warnings ride on the plan but not in its hash: the same
	// workloads hash the same whether or not the cluster was ready when
	// the scan ran. Apply re-checks the live cluster regardless.
	plan.Spec.Warnings = planWarnings(report.Metadata.ClusterFacts, plan.Spec.Options.RuntimeClassName)

	clusterWarnings := len(plan.Spec.Warnings)

	// A plan made from manifests is a preview. Mark it (loadPlan refuses a
	// marked plan for apply, rollback, and verify) and say so in a warning,
	// first in the list, because it is the one that changes what the
	// reader can do with the file. Like every warning, and like Source, it
	// is outside the plan hash.
	var sourceManaged *schema.PlanWarning
	if src := report.Metadata.Source; src != nil {
		plan.Metadata.Source = &schema.ReportSource{Type: src.Type}
		plan.Spec.Warnings = append([]schema.PlanWarning{{
			ID:      WarningManifestSource,
			Message: manifestPlanMessage,
		}}, plan.Spec.Warnings...)
	} else if sourceManaged = sourceManagedWarning(plan, report); sourceManaged != nil {
		// Workloads that a deploy tool owns (managed.go). Only for a plan
		// that `apply` accepts: a preview changes no live object, so
		// there is no live change that the source does not have.
		plan.Spec.Warnings = append(plan.Spec.Warnings, *sourceManaged)
	}

	// Steps that carry seccompProfile Unconfined into gVisor. From runsc
	// 20261123.1 the profile is enforced in the sandbox, so Unconfined is
	// the only way out of it: a choice the plan should show. Outside the
	// plan hash, like every warning.
	unconfined := seccompUnconfinedWarning(plan, report)
	if unconfined != nil {
		plan.Spec.Warnings = append(plan.Spec.Warnings, *unconfined)
	}

	_, _ = fmt.Fprintf(stderr, "planned %d steps (%d workloads excluded)\n",
		plan.Spec.Summary.Included,
		plan.Spec.Summary.Excluded,
	)
	switch {
	case plan.Metadata.Source != nil:
		_, _ = fmt.Fprintf(stderr, "plan: %s\n", manifestPlanMessage)
	case clusterWarnings > 0:
		_, _ = fmt.Fprintf(stderr, "plan: %d cluster warning(s); the cluster may not be able to host this plan yet (run 'agentmoat preflight')\n", clusterWarnings)
	}
	if sourceManaged != nil {
		_, _ = fmt.Fprintf(stderr, "plan: %s\n", sourceManaged.Message)
	}
	if unconfined != nil {
		_, _ = fmt.Fprintf(stderr, "plan: %s\n", unconfined.Message)
	}
	return plan, nil
}

// planReport resolves the ScanReport a plan is made from: opts.ScanReport,
// else the file at opts.ScanReportPath, else an inline Scan. When
// opts.ScanOptions.FactsPath is set and the report is a stored one, the
// facts in it are replaced (on a copy).
func planReport(ctx context.Context, opts PlanOptions, stderr io.Writer) (*schema.ScanReport, error) {
	report := opts.ScanReport
	if report == nil && opts.ScanReportPath != "" {
		r, err := loadScanReport(opts.ScanReportPath)
		if err != nil {
			return nil, fmt.Errorf("plan: loading scan report: %w", err)
		}
		report = r
	}
	if report != nil && opts.ScanOptions.FactsPath != "" {
		// A stored scan plus fresher facts (typically a probe report):
		// swap the facts block so the warnings Plan derives describe the
		// cluster as it is now. Classification inside the stored report
		// is left alone; re-scan with --facts to refine verdicts. Copy
		// the envelope so the caller's report is not mutated.
		facts, err := loadClusterFacts(opts.ScanOptions.FactsPath)
		if err != nil {
			return nil, fmt.Errorf("plan: loading --facts: %w", err)
		}
		r := *report
		r.Metadata.ClusterFacts = facts
		report = &r
	}
	if report == nil {
		// Inline scan. We pass the caller-supplied scan options through;
		// the caller can also pre-set opts.ScanOptions.Stderr to silence.
		scanOpts := opts.ScanOptions
		if scanOpts.Stderr == nil {
			scanOpts.Stderr = stderr
		}
		// Collect facts for the RuntimeClass the plan will target, so the
		// warnings Plan derives describe the right object.
		if scanOpts.RuntimeClassName == "" {
			scanOpts.RuntimeClassName = opts.Planner.RuntimeClassName
		}
		r, err := Scan(ctx, scanOpts)
		if err != nil {
			return nil, fmt.Errorf("plan: inline scan: %w", err)
		}
		report = r
	}
	return report, nil
}
