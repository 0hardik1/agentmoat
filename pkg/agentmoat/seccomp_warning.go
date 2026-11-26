// Package agentmoat: the plan warning for steps that set seccompProfile
// Unconfined.
package agentmoat

import (
	"fmt"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
)

// WarningSeccompUnconfined is the plan warning for steps whose workload
// has a container with seccompProfile Unconfined (classifier rule
// seccomp-unconfined).
const WarningSeccompUnconfined = classifier.RuleSeccompUnconfined

// maxNamedSteps bounds how many workloads the warning names.
const maxNamedSteps = 10

// seccompUnconfinedWarning returns the warning, or nil when no step's
// workload has the seccomp-unconfined reason in the report. It names the
// workloads in step order, because unlike the managed-by markers the
// report row is the only place a reader would otherwise find them.
func seccompUnconfinedWarning(plan *schema.MigrationPlan, report *schema.ScanReport) *schema.PlanWarning {
	flagged := map[schema.WorkloadRef]bool{}
	for _, w := range report.Spec.Workloads {
		for _, r := range w.Reasons {
			if r.RuleID == classifier.RuleSeccompUnconfined {
				flagged[schema.WorkloadRef{Kind: w.Kind, Namespace: w.Namespace, Name: w.Name}] = true
			}
		}
	}
	var names []string
	for _, step := range plan.Spec.Steps {
		if flagged[step.Target] {
			names = append(names, fmt.Sprintf("%s %s/%s", step.Target.Kind, step.Target.Namespace, step.Target.Name))
		}
	}
	if len(names) == 0 {
		return nil
	}
	total := len(names)
	if total > maxNamedSteps {
		names = append(names[:maxNamedSteps], fmt.Sprintf("and %d more", total-maxNamedSteps))
	}
	return &schema.PlanWarning{
		ID: WarningSeccompUnconfined,
		Message: fmt.Sprintf("%d of %d workload(s) in this plan set seccompProfile Unconfined (%s). "+
			"From gVisor %s runsc enforces a container's seccomp profile inside the sandbox, and Unconfined opts these workloads out of it. "+
			"Keep it only if that is a choice; 'agentmoat probe seccomp' shows what RuntimeDefault would block (docs/explanations/seccomp-unconfined.md)",
			total, len(plan.Spec.Steps), strings.Join(names, ", "), schema.OCISeccompDefaultRelease),
	}
}
