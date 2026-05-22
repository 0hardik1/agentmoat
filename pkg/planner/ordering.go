// Package planner: workload-risk scoring and ordering.
//
// Why this file exists
//
//   The single most important property of a migration plan is that *low-risk
//   workloads roll first*. If the operator notices something is wrong, the
//   plan should have already moved the easy stuff (stateless web frontends,
//   cooperative agents) before touching the scary stuff (StatefulSet
//   databases, DaemonSets that run on every node). The orderer assigns each
//   workload a deterministic integer "risk score" derived from its kind and
//   the classifier reasons that fired against it, then sorts ascending.
//
// Scoring rubric
//
//   The score is a sum of contributions. Each contribution is small
//   (single-digit) so the score stays comprehensible when printed in the
//   plan envelope. The exact magnitudes are tuned for the v1 fixture set
//   in test/testdata/fixtures/; they are not load-bearing across the rest of
//   the codebase. Three categories:
//
//     1. Controller kind:
//          Deployment, Job, CronJob  -> 0  (cooperative scale; restart-safe).
//          Pod                       -> 1  (standalone; rolling means
//                                          delete+recreate by the operator).
//          DaemonSet                 -> 3  (every node feels the change).
//          StatefulSet               -> 5  (ordered, identity-stable; rolls
//                                          one pod at a time and may need
//                                          PVC/PV churn).
//
//     2. Severity hints carried by Reasons:
//          info-only reasons         -> +0 (no operational risk).
//          one warn reason           -> +2
//          multiple warn reasons     -> +3
//
//     3. Overhead category (info-class reason already capped):
//          network-throughput        -> +2 (performance-sensitive; cap-aware
//                                          operators want to validate first).
//          syscall-heavy             -> +1
//
//   Workloads with at least one error-class reason should never reach this
//   scorer: the orderer's caller (planner.Plan) filters Incompatible workloads
//   into the Excluded list before calling Order(). Defensive nil-handling is
//   still cheap here.
//
// Tie-breaker
//
//   Two workloads with the same score fall back to (Namespace, Kind, Name)
//   lexicographic order so output stays deterministic.
package planner

import (
	"sort"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// Scored is a (WorkloadResult, score) pair the orderer produces internally.
// Exposed via Order() so tests can assert on the per-workload score, not
// just the final order.
type Scored struct {
	Workload schema.WorkloadResult
	Score    int
}

// Score returns the deterministic risk score for one workload. The function
// is pure: same input -> same output, no globals, no time.
func Score(w schema.WorkloadResult) int {
	score := kindRiskWeight(w.Kind)

	warnCount := 0
	for _, r := range w.Reasons {
		switch r.Severity {
		case schema.SeverityWarn:
			warnCount++
		case schema.SeverityInfo:
			switch r.RuleID {
			case "network-throughput":
				score += 2
			case "syscall-heavy":
				score += 1
			}
		}
	}
	switch {
	case warnCount >= 2:
		score += 3
	case warnCount == 1:
		score += 2
	}
	return score
}

// kindRiskWeight assigns the per-controller weight described in the package
// doc. Unknown kinds default to 1 (Pod-equivalent, since that's the lowest
// inflate we are comfortable with for an unseen controller).
func kindRiskWeight(kind string) int {
	switch kind {
	case "Deployment", "Job", "CronJob":
		return 0
	case "Pod":
		return 1
	case "DaemonSet":
		return 3
	case "StatefulSet":
		return 5
	default:
		return 1
	}
}

// Order takes the workloads the planner has decided are "in" the plan and
// returns them sorted by risk-score ascending, with a deterministic
// (Namespace, Kind, Name) tie-break.
//
// The returned slice is a new allocation; the input is not mutated.
func Order(in []schema.WorkloadResult) []Scored {
	out := make([]Scored, 0, len(in))
	for _, w := range in {
		out = append(out, Scored{Workload: w, Score: Score(w)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score < out[j].Score
		}
		// Deterministic tiebreaker for stable plans across runs.
		if out[i].Workload.Namespace != out[j].Workload.Namespace {
			return out[i].Workload.Namespace < out[j].Workload.Namespace
		}
		if out[i].Workload.Kind != out[j].Workload.Kind {
			return out[i].Workload.Kind < out[j].Workload.Kind
		}
		return out[i].Workload.Name < out[j].Workload.Name
	})
	return out
}

// waitForKind returns the appropriate `waitFor` hint for a given controller
// kind. Lifted out of Plan() so the policy is visible in one place.
//
//   - Deployment, StatefulSet, DaemonSet: wait on "Ready" because these are
//     long-running serving controllers; the rollout should converge before
//     the applier moves on.
//   - Job, CronJob, Pod: "Running" is enough; some of these intentionally
//     exit shortly after starting and would never reach Ready=true.
//   - Anything else: empty string ("do not wait"), the conservative default.
func waitForKind(kind string) string {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		return "Ready"
	case "Job", "CronJob", "Pod":
		return "Running"
	default:
		return ""
	}
}

// summariseReasons returns a short, comma-joined string of rule IDs for a
// Notes field. Empty when reasons is empty.
func summariseReasons(reasons []schema.Reason) string {
	if len(reasons) == 0 {
		return ""
	}
	out := ""
	for i, r := range reasons {
		if i > 0 {
			out += ", "
		}
		out += r.RuleID
	}
	return out
}
