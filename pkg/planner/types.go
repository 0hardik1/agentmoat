// Package planner turns a finished `schema.ScanReport` into an ordered
// `schema.MigrationPlan`.
//
// Why a dedicated package
//
//   - The planner is a *pure function* over a ScanReport plus PlannerOptions.
//     No I/O, no Kubernetes calls, no time-dependent state (the timestamp on
//     the resulting plan is supplied by the orchestrator, not stamped here).
//     This is what makes it testable with golden fixtures and reproducible
//     across runs (plan.md section 12.6).
//
//   - Decoupling planner from applier means the operator workflow can be
//     split: produce a plan, eyeball it (kubectl-style "review the diff"),
//     archive it, then apply it later, possibly from a different host. The
//     applier reads the same MigrationPlan envelope from disk.
//
//   - A separate package also gives a natural unit boundary for the
//     lowest-risk-first ordering heuristic. Tweaking the heuristic (e.g.
//     adding a "depriotise StatefulSets" knob) only touches planner code.
//
// This file declares types only; logic lives in planner.go and ordering.go.
package planner

import (
	"github.com/0hardik1/agentmoat/internal/schema"
)

// Options is a convenience alias so callers do not need to import the
// schema package just to construct a PlannerOptions value. Both names refer
// to the same struct.
type Options = schema.PlannerOptions

// defaultPlannerOptions returns the zero-value PlannerOptions tuned with
// the runtime-class default that makes the planner usable without the
// caller supplying explicit options.
func defaultPlannerOptions() Options {
	return Options{
		RuntimeClassName: schema.DefaultRuntimeClassName,
	}
}
