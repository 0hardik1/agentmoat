// Package classifier: the Classify entry point.
//
// Classify is a pure, deterministic function: given a Workload and a
// Registry, it produces a single Verdict. No I/O, no time, no randomness,
// no shared state. The same workload classified twice with the same
// registry produces byte-identical output. This determinism is a hard
// requirement (plan.md section 12.6) because the same scan should round-
// trip cleanly through both the CLI and the MCP server, and because diffs
// between scans only mean anything if rule evaluation is repeatable.
//
// Aggregation logic (verdict resolution):
//   - any rule fires with SeverityError -> Incompatible
//   - otherwise any rule fires with SeverityWarn -> Review
//   - otherwise -> Compatible (info-only or no rules fired)
//
// Reasons are sorted by RuleID because rules are independent: there is no
// natural execution order, and consumers (humans and tools) want stable
// output to diff against.
package classifier

import (
	"fmt"
	"sort"

	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// Classify runs every Rule in the Registry against the Workload and
// produces a Verdict.
//
// Compatibility resolution:
//   - If any error-severity rule fires -> Incompatible.
//   - Else if any warn-severity rule fires -> Review.
//   - Else (only info or no rules fired) -> Compatible.
//
// Reasons are sorted by RuleID for deterministic output.
//
// Recommendation generation:
//   - Incompatible: "Do not migrate to gVisor: <count> blocking issues. See reasons."
//   - Review: "Review before migration: <count> potential issues."
//   - Compatible: "Safe to migrate: set spec.runtimeClassName: gvisor on this workload."
//
// Overhead is set from the highest-severity info-class rule that fires
// (network-throughput, syscall-heavy), else empty.
func Classify(w scanner.Workload, registry *Registry) Verdict {
	var (
		reasons      []Reason
		hasError     bool
		hasWarn      bool
		overheadHint string
	)

	// Iterate over Registry.Rules() (already sorted by ID) so the order
	// of evaluation is deterministic. We still re-sort Reasons below
	// because we may construct them out of order in future refactors.
	for _, rule := range registry.Rules() {
		if !rule.Match(w) {
			continue
		}
		reasons = append(reasons, Reason{
			RuleID:         rule.ID,
			Severity:       rule.Severity,
			Description:    rule.Description,
			RemediationURL: rule.RemediationURL,
		})

		// Track the worst severity seen so far.
		switch rule.Severity {
		case SeverityError:
			hasError = true
		case SeverityWarn:
			hasWarn = true
		case SeverityInfo:
			// Info-class rules don't affect Compatibility, but they
			// do set the Overhead hint. Map each well-known info rule
			// to its qualitative cost label from plan.md section 5.5.
			switch rule.ID {
			case "network-throughput":
				// Network-throughput is the dominant overhead category
				// when present; let it stick even if syscall-heavy also
				// fires (this happens with composite images).
				overheadHint = "Network throughput: 20-40%"
			case "syscall-heavy":
				// Only set syscall-heavy hint if we haven't already
				// landed on network-throughput (network is the bigger
				// concern when both apply).
				if overheadHint == "" {
					overheadHint = "Syscall-heavy: 5-10x latency in pathological cases"
				}
			}
		}
	}

	// Sort by RuleID for deterministic Reasons output. Stable sort isn't
	// necessary since rule IDs are unique within the registry.
	sort.Slice(reasons, func(i, j int) bool {
		return reasons[i].RuleID < reasons[j].RuleID
	})

	// Resolve the headline Compatibility value.
	var compat Compatibility
	switch {
	case hasError:
		compat = Incompatible
	case hasWarn:
		compat = Review
	default:
		compat = Compatible
	}

	return Verdict{
		Compatibility:  compat,
		Reasons:        reasons,
		Recommendation: recommendationFor(compat, reasons),
		Overhead:       overheadHint,
	}
}

// recommendationFor builds the single-sentence operator-facing summary.
// Kept separate from Classify so the wording can change without touching
// the aggregation logic, and so that tests can target it directly.
func recommendationFor(compat Compatibility, reasons []Reason) string {
	switch compat {
	case Incompatible:
		blocking := 0
		for _, r := range reasons {
			if r.Severity == SeverityError {
				blocking++
			}
		}
		return fmt.Sprintf("Do not migrate to gVisor: %d blocking issues. See reasons.", blocking)
	case Review:
		potential := 0
		for _, r := range reasons {
			if r.Severity == SeverityWarn {
				potential++
			}
		}
		return fmt.Sprintf("Review before migration: %d potential issues.", potential)
	default:
		// Compatible. The recommendation doubles as a hint for the next
		// agentmoat step (the planner will literally emit a patch that
		// sets spec.runtimeClassName: gvisor).
		return "Safe to migrate: set spec.runtimeClassName: gvisor on this workload."
	}
}
