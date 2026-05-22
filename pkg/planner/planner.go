// Package planner: the Plan entry point.
//
// Plan() is the only public function here. It takes a ScanReport plus Options
// and returns a MigrationPlan. The function is pure: no I/O, no network, no
// time-dependent state. The orchestrator (pkg/agentmoat.Plan) is what stamps
// the plan with GeneratedAt and AgentmoatVersion: those are environment
// facts, not properties of the algorithm.
//
// Step-by-step (parallels the doc comment on Plan itself):
//
//   1. Walk every WorkloadResult in the input report. Decide
//      include-vs-exclude:
//        - Compatibility == Incompatible -> Excluded with the join of rule
//          IDs as the reason.
//        - Compatibility == Review       -> Excluded unless opts.IncludeReview
//                                            is true.
//        - Compatibility == Compatible   -> Included.
//
//   2. Score the included workloads via ordering.Order(). The orderer also
//      tie-breaks deterministically on (Namespace, Kind, Name).
//
//   3. Materialise PlanStep entries in the ordered slice. Each step is a
//      declarative description: action=set-runtime-class, the target's
//      identity, the runtime class name, an AddToleration flag, a WaitFor
//      hint, and a Notes string. The applier turns these into patches.
//
//   4. Compute the PlanHash by hashing the step list (deterministic, sorted
//      input, see hashSteps).
//
//   5. Return a fully-formed *schema.MigrationPlan with envelope filled and
//      Spec.Options echoing what the planner used.
//
// The planner deliberately does NOT generate the JSON patch bytes here. That
// keeps the planner package free of K8s API types, and means a Phase 2 reader
// can move from MigrationPlan -> patch without re-parsing the planner's
// implementation choices.
package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// Plan converts a ScanReport into a deterministic MigrationPlan.
//
// Determinism contract:
//   - The same (ScanReport, Options) pair always returns the same MigrationPlan
//     (modulo Metadata.GeneratedAt, which the orchestrator stamps and which
//     callers can override via the returned plan).
//   - The PlanHash is a content hash over Spec.Steps + Spec.Options that
//     does not change when only Metadata changes.
//
// Errors are returned only for invalid inputs (e.g. nil report). All other
// edge cases produce a valid (possibly empty) plan.
func Plan(report *schema.ScanReport, opts Options) (*schema.MigrationPlan, error) {
	if report == nil {
		return nil, fmt.Errorf("planner: nil ScanReport")
	}

	// Fill in defaults for any zero-valued options. Mirrors the kubectl
	// pattern of "explicit + defaulted = effective".
	effective := opts
	if effective.RuntimeClassName == "" {
		effective.RuntimeClassName = schema.DefaultRuntimeClassName
	}

	// 1. Partition workloads.
	var included []schema.WorkloadResult
	var excluded []schema.ExcludedWorkload
	for _, w := range report.Spec.Workloads {
		switch w.Compatibility {
		case schema.CompatibilityIncompatible:
			excluded = append(excluded, schema.ExcludedWorkload{
				Target:        toWorkloadRef(w),
				Compatibility: w.Compatibility,
				Reason: fmt.Sprintf(
					"workload is incompatible: %s",
					commaJoinErrorReasons(w.Reasons),
				),
			})
		case schema.CompatibilityReview:
			if effective.IncludeReview {
				included = append(included, w)
				continue
			}
			excluded = append(excluded, schema.ExcludedWorkload{
				Target:        toWorkloadRef(w),
				Compatibility: w.Compatibility,
				Reason: fmt.Sprintf(
					"workload needs review (use --include-review to plan anyway): %s",
					commaJoinWarnReasons(w.Reasons),
				),
			})
		default:
			// Compatible (default).
			included = append(included, w)
		}
	}

	// 2 + 3. Score, sort, build steps.
	scored := Order(included)
	steps := make([]schema.PlanStep, 0, len(scored))
	for i, s := range scored {
		steps = append(steps, schema.PlanStep{
			Order:            i + 1,
			Target:           toWorkloadRef(s.Workload),
			Action:           "set-runtime-class",
			RuntimeClassName: effective.RuntimeClassName,
			AddToleration:    true,
			WaitFor:          waitForKind(s.Workload.Kind),
			RiskScore:        s.Score,
			Notes:            buildNotes(s.Workload),
		})
	}

	// Ensure Excluded is non-nil (consumers should not have to nil-check it).
	if excluded == nil {
		excluded = []schema.ExcludedWorkload{}
	}

	// 4. PlanHash. We hash the steps + effective options so it changes when
	// either changes. The Metadata block (timestamps, cluster) is excluded
	// from the hash on purpose: re-planning the same scan moments later
	// must produce the same PlanHash so namespace-annotation idempotency
	// works across operator runs.
	hashIn := struct {
		Steps   []schema.PlanStep      `json:"steps"`
		Options schema.PlannerOptions  `json:"options"`
	}{
		Steps:   steps,
		Options: effective,
	}
	planHash, err := hashSteps(hashIn)
	if err != nil {
		return nil, fmt.Errorf("planner: computing plan hash: %w", err)
	}

	plan := schema.NewMigrationPlan()
	plan.Metadata = schema.PlanMetadata{
		Cluster:               report.Metadata.Cluster,
		AgentmoatVersion:      report.Metadata.AgentmoatVersion,
		SourceScanGeneratedAt: report.Metadata.GeneratedAt,
		PlanHash:              planHash,
	}
	plan.Spec = schema.PlanSpec{
		Summary: schema.PlanSummary{
			Total:    len(steps) + len(excluded),
			Included: len(steps),
			Excluded: len(excluded),
		},
		Options:  effective,
		Steps:    steps,
		Excluded: excluded,
	}
	return plan, nil
}

// toWorkloadRef projects a WorkloadResult down to the WorkloadRef shape
// carried by PlanStep and ExcludedWorkload.
func toWorkloadRef(w schema.WorkloadResult) schema.WorkloadRef {
	return schema.WorkloadRef{
		Kind:      w.Kind,
		Namespace: w.Namespace,
		Name:      w.Name,
	}
}

// buildNotes returns a short, single-sentence rationale used in Plan.Notes.
// Empty when nothing interesting fired (the table view drops the column).
func buildNotes(w schema.WorkloadResult) string {
	if len(w.Reasons) == 0 {
		return ""
	}
	return fmt.Sprintf("flagged: %s", summariseReasons(w.Reasons))
}

// commaJoinErrorReasons returns a comma-joined list of rule IDs for the
// SeverityError findings in r, in their (already-sorted) input order.
func commaJoinErrorReasons(r []schema.Reason) string {
	out := ""
	first := true
	for _, x := range r {
		if x.Severity != schema.SeverityError {
			continue
		}
		if !first {
			out += ", "
		}
		out += x.RuleID
		first = false
	}
	return out
}

// commaJoinWarnReasons is the warn-class counterpart of
// commaJoinErrorReasons.
func commaJoinWarnReasons(r []schema.Reason) string {
	out := ""
	first := true
	for _, x := range r {
		if x.Severity != schema.SeverityWarn {
			continue
		}
		if !first {
			out += ", "
		}
		out += x.RuleID
		first = false
	}
	return out
}

// hashSteps computes a SHA-256 hex digest of v. The marshaling step uses
// encoding/json with the schema struct tags, so the hash is stable across
// runs as long as the struct shapes do not change.
func hashSteps(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
