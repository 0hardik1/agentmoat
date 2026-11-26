package agentmoat

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func TestSeccompUnconfinedWarning(t *testing.T) {
	t.Parallel()
	report := &schema.ScanReport{Spec: schema.ReportSpec{Workloads: []schema.WorkloadResult{
		{Kind: "Deployment", Namespace: "shop", Name: "web", Reasons: []schema.Reason{{RuleID: "seccomp-unconfined", Severity: schema.SeverityInfo}}},
		{Kind: "Deployment", Namespace: "shop", Name: "api"},
		{Kind: "Deployment", Namespace: "shop", Name: "left-out", Reasons: []schema.Reason{{RuleID: "seccomp-unconfined", Severity: schema.SeverityInfo}}},
	}}}
	plan := &schema.MigrationPlan{Spec: schema.PlanSpec{Steps: []schema.PlanStep{
		{Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "api"}},
		{Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "web"}},
	}}}
	w := seccompUnconfinedWarning(plan, report)
	if w == nil || w.ID != WarningSeccompUnconfined {
		t.Fatalf("warning = %+v", w)
	}
	if !strings.HasPrefix(w.Message, "1 of 2 workload(s) in this plan set seccompProfile Unconfined (Deployment shop/web)") {
		t.Fatalf("message = %s", w.Message)
	}
	plan.Spec.Steps = plan.Spec.Steps[:1]
	if seccompUnconfinedWarning(plan, report) != nil {
		t.Fatal("want no warning when no step is flagged")
	}
}
