// Package agentmoat: plan-level cluster warnings.
//
// planWarnings turns the ClusterFacts stored in a ScanReport into
// MigrationPlan.Spec.Warnings. It reuses pkg/preflight.Evaluate (a pure
// function) so `plan` and `preflight` can never disagree about what a
// not-ready cluster looks like, while `plan` itself still never touches
// the API. Info-severity findings are dropped: a warning list is for
// things that can stop the plan from landing.
package agentmoat

import (
	"fmt"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
)

// WarningClusterFactsRuntimeClassMismatch is emitted (alone) when the
// scan inspected a different RuntimeClass than the plan targets, so the
// stored facts say nothing about this plan.
const WarningClusterFactsRuntimeClassMismatch = "cluster-facts-runtime-class-mismatch"

// planWarnings derives plan warnings from facts. Nil facts (scan ran with
// --no-cluster-facts, or RBAC denied the reads) yields nil: absence of
// evidence is not a warning, and apply re-checks the live cluster anyway.
func planWarnings(facts *schema.ClusterFacts, runtimeClassName string) []schema.PlanWarning {
	if facts == nil {
		return nil
	}
	if runtimeClassName == "" {
		runtimeClassName = schema.DefaultRuntimeClassName
	}
	if facts.RuntimeClass.Name != runtimeClassName {
		return []schema.PlanWarning{{
			ID: WarningClusterFactsRuntimeClassMismatch,
			Message: fmt.Sprintf("the scan collected cluster facts for RuntimeClass %q but this plan targets %q; "+
				"re-run 'agentmoat scan --runtime-class %s' for accurate warnings (apply re-checks the live cluster regardless)",
				facts.RuntimeClass.Name, runtimeClassName, runtimeClassName),
		}}
	}
	var out []schema.PlanWarning
	for _, f := range preflight.Evaluate(facts) {
		if f.Severity == schema.SeverityInfo {
			continue
		}
		out = append(out, schema.PlanWarning{ID: f.ID, Message: f.Message})
	}
	return out
}
