// Package preflight: the Run entry point.
package preflight

import (
	"context"
	"fmt"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// Run collects the facts, evaluates them, and wraps both in a
// PreflightReport. Returns a non-nil error only when the cluster could not
// be read (nil client, RBAC denied, network): a not-ready cluster is a
// report with Summary.Ready=false, not an error, so callers shape exit
// codes from the summary exactly as they do for verify.
func Run(ctx context.Context, opts Options) (*schema.PreflightReport, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("preflight: opts.Client is nil")
	}
	name := opts.RuntimeClassName
	if name == "" {
		name = schema.DefaultRuntimeClassName
	}

	facts, err := Collect(ctx, opts.Client, name)
	if err != nil {
		return nil, err
	}
	findings := Evaluate(facts)

	report := schema.NewPreflightReport()
	report.Metadata = schema.PreflightMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          opts.Cluster,
		AgentmoatVersion: opts.AgentmoatVersion,
		RuntimeClassName: name,
	}
	report.Spec = schema.PreflightSpec{
		Summary:  Summarize(findings),
		Facts:    *facts,
		Findings: findings,
	}
	return report, nil
}
