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
//     warnings.go). The planner itself stays a pure function of workloads
//     and options; warnings never enter the plan hash.
//
// Both the CLI and the MCP server call into this so the resulting
// MigrationPlan is identical regardless of operator surface.
package agentmoat

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/planner"
)

// Plan produces a MigrationPlan for a cluster. If opts.ScanReport is
// non-nil, it is used as the source. Otherwise, Plan runs Scan(opts.ScanOptions)
// to obtain one.
//
// The returned plan has its Metadata stamped with the current binary
// version and GeneratedAt timestamp. The planner's pure function does the
// actual partitioning/ordering/hashing.
func Plan(ctx context.Context, opts PlanOptions) (*schema.MigrationPlan, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	report := opts.ScanReport
	if report != nil && opts.ScanOptions.FactsPath != "" {
		// A stored scan plus fresher facts (typically a probe report):
		// swap the facts block so the warnings below describe the
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
		// warnings below describe the right object.
		if scanOpts.RuntimeClassName == "" {
			scanOpts.RuntimeClassName = opts.Planner.RuntimeClassName
		}
		r, err := Scan(ctx, scanOpts)
		if err != nil {
			return nil, fmt.Errorf("plan: inline scan: %w", err)
		}
		report = r
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

	_, _ = fmt.Fprintf(stderr, "planned %d steps (%d workloads excluded)\n",
		plan.Spec.Summary.Included,
		plan.Spec.Summary.Excluded,
	)
	if n := len(plan.Spec.Warnings); n > 0 {
		_, _ = fmt.Fprintf(stderr, "plan: %d cluster warning(s); the cluster may not be able to host this plan yet (run 'agentmoat preflight')\n", n)
	}
	return plan, nil
}
