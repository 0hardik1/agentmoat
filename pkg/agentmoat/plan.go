// Package agentmoat: the Plan orchestrator.
//
// Plan is the public-facing wrapper around pkg/planner.Plan. Its job is to
// resolve the input ScanReport (either supplied or computed by running
// Scan() inline) and then hand it to the pure planner function. The
// orchestrator owns:
//
//   - resolving the source (in-memory ScanReport vs running a Scan).
//   - stamping the produced MigrationPlan with the agentmoat binary version.
//
// Both the CLI and the future MCP server call into this so the resulting
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
	if report == nil {
		// Inline scan. We pass the caller-supplied scan options through;
		// the caller can also pre-set opts.ScanOptions.Stderr to silence.
		scanOpts := opts.ScanOptions
		if scanOpts.Stderr == nil {
			scanOpts.Stderr = stderr
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

	fmt.Fprintf(stderr, "planned %d steps (%d included, %d excluded)\n",
		plan.Spec.Summary.Total,
		plan.Spec.Summary.Included,
		plan.Spec.Summary.Excluded,
	)
	return plan, nil
}

// _ keeps the io import live for callers that pass opts.Stderr.
var _ io.Writer = (io.Writer)(nil)
