// Package classifier turns a scanned Workload into a gVisor-compatibility
// Verdict by running a registered set of Rules against the Pod spec.
//
// Design properties (determinism)
//
//   - Pure functions. Classify takes a Workload + Rule set, returns a
//     Verdict. No I/O, no time, no randomness. Same input -> same output.
//
//   - Rule registry. Rules live in builtin_rules.go and can be augmented
//     or overridden by loading internal/rules/gvisor.yaml at startup.
//
//   - Stable IDs. Each Rule has a stable ID string referenced from output
//     JSON and from operator-authored overrides.
//
// This file declares the data shapes. Implementation logic is in
// classifier.go and rules.go.
package classifier

import "github.com/0hardik1/agentmoat/internal/schema"

// Compatibility re-exports the schema-level compatibility enum so callers
// inside this package read naturally ("classifier.Compatible") without
// importing internal/schema directly.
type Compatibility = schema.Compatibility

// Re-exported compatibility values, identical to the schema constants.
const (
	Compatible   = schema.CompatibilityCompatible
	Review       = schema.CompatibilityReview
	Incompatible = schema.CompatibilityIncompatible
)

// Severity re-exports the schema severity enum (info | warn | error).
type Severity = schema.Severity

const (
	SeverityInfo  = schema.SeverityInfo
	SeverityWarn  = schema.SeverityWarn
	SeverityError = schema.SeverityError
)

// Reason re-exports the schema.Reason type. Rules return Reasons; the
// classifier collects them into a Verdict.
type Reason = schema.Reason

// Verdict is the classifier's output for a single Workload.
//
// The Compatibility field is the headline: any rule that fires with
// SeverityError makes the verdict Incompatible; any SeverityWarn (without
// errors) makes it Review; no findings makes it Compatible.
type Verdict struct {
	Compatibility Compatibility

	// Reasons is the deduplicated, deterministic list of rule findings,
	// sorted by RuleID for stable output.
	Reasons []Reason

	// Recommendation is the single-sentence operator-facing summary. It is
	// generated from the highest-severity reason; for "compatible"
	// workloads it suggests setting `spec.runtimeClassName: gvisor`.
	Recommendation string

	// Overhead is the qualitative cost estimate ("CPU-bound: <5%", etc.).
	// Populated when the classifier can detect a workload class from
	// container images or annotations. Empty when unknown.
	Overhead string
}

// IsBlocking returns true if at least one Reason has SeverityError. Used by
// the orchestrator (pkg/agentmoat) to decide the process exit code.
func (v Verdict) IsBlocking() bool {
	for _, r := range v.Reasons {
		if r.Severity == SeverityError {
			return true
		}
	}
	return false
}
