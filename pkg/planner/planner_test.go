// Tests for the planner package.
//
// The tests are deliberately fixture-light: a ScanReport is just a struct
// literal here. We are testing pure logic (partitioning, ordering, hashing),
// not Kubernetes integration. The applier package's tests cover the K8s
// interaction surface.
package planner

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// fixtureReport mirrors the 7 yaml fixtures under test/testdata/fixtures/.
// The compatibility verdicts and rule IDs match what the classifier would
// produce against those fixtures (also tested independently in
// pkg/classifier/classifier_test.go), so this stays in sync with the
// classifier without re-running it.
func fixtureReport() *schema.ScanReport {
	return &schema.ScanReport{
		APIVersion: schema.APIVersion,
		Kind:       schema.KindScanReport,
		Metadata: schema.ReportMetadata{
			GeneratedAt:      "2026-05-22T11:00:00Z",
			Cluster:          "kind-agentmoat-verify",
			AgentmoatVersion: "test",
		},
		Spec: schema.ReportSpec{
			Workloads: []schema.WorkloadResult{
				// Compatible: a vanilla nginx Deployment. Only info-level
				// hint fired (network-throughput).
				{
					Kind:          "Deployment",
					Namespace:     "default",
					Name:          "web",
					Compatibility: schema.CompatibilityCompatible,
					Reasons: []schema.Reason{
						{RuleID: "network-throughput", Severity: schema.SeverityInfo},
					},
				},
				// Compatible: a busybox Pod (no rules fired).
				{
					Kind:          "Pod",
					Namespace:     "default",
					Name:          "sleeper",
					Compatibility: schema.CompatibilityCompatible,
				},
				// Compatible: a StatefulSet running redis (syscall-heavy info).
				{
					Kind:          "StatefulSet",
					Namespace:     "data",
					Name:          "cache",
					Compatibility: schema.CompatibilityCompatible,
					Reasons: []schema.Reason{
						{RuleID: "syscall-heavy", Severity: schema.SeverityInfo},
					},
				},
				// Review: GPU passthrough.
				{
					Kind:          "Pod",
					Namespace:     "default",
					Name:          "gpu-worker",
					Compatibility: schema.CompatibilityReview,
					Reasons: []schema.Reason{
						{RuleID: "gpu-passthrough", Severity: schema.SeverityWarn},
					},
				},
				// Review: hostPath mount.
				{
					Kind:          "Pod",
					Namespace:     "default",
					Name:          "log-reader",
					Compatibility: schema.CompatibilityReview,
					Reasons: []schema.Reason{
						{RuleID: "host-path-mount", Severity: schema.SeverityWarn},
					},
				},
				// Incompatible: host-network.
				{
					Kind:          "Pod",
					Namespace:     "default",
					Name:          "host-net-app",
					Compatibility: schema.CompatibilityIncompatible,
					Reasons: []schema.Reason{
						{RuleID: "host-network", Severity: schema.SeverityError},
					},
				},
				// Incompatible: privileged + raw-socket together.
				{
					Kind:          "Pod",
					Namespace:     "default",
					Name:          "privileged-app",
					Compatibility: schema.CompatibilityIncompatible,
					Reasons: []schema.Reason{
						{RuleID: "privileged", Severity: schema.SeverityError},
						{RuleID: "raw-socket", Severity: schema.SeverityError},
					},
				},
				// Incompatible: eBPF (Cilium-like).
				{
					Kind:          "DaemonSet",
					Namespace:     "kube-system",
					Name:          "cilium",
					Compatibility: schema.CompatibilityIncompatible,
					Reasons: []schema.Reason{
						{RuleID: "ebpf", Severity: schema.SeverityError},
					},
				},
			},
		},
	}
}

func TestPlan_PartitionsByCompatibility(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// 3 Compatible -> 3 steps. 2 Review + 3 Incompatible -> 5 excluded.
	if got := plan.Spec.Summary.Total; got != 8 {
		t.Errorf("Summary.Total: got %d want 8", got)
	}
	if got := plan.Spec.Summary.Included; got != 3 {
		t.Errorf("Summary.Included: got %d want 3", got)
	}
	if got := plan.Spec.Summary.Excluded; got != 5 {
		t.Errorf("Summary.Excluded: got %d want 5", got)
	}

	// Spot-check that the excluded list carries the right kind of reason.
	foundIncompat := 0
	foundReview := 0
	for _, e := range plan.Spec.Excluded {
		switch e.Compatibility {
		case schema.CompatibilityIncompatible:
			foundIncompat++
			if !strings.Contains(e.Reason, "incompatible") {
				t.Errorf("Excluded reason for %s/%s should mention 'incompatible', got %q",
					e.Target.Namespace, e.Target.Name, e.Reason)
			}
		case schema.CompatibilityReview:
			foundReview++
			if !strings.Contains(e.Reason, "review") {
				t.Errorf("Excluded reason for %s/%s should mention 'review', got %q",
					e.Target.Namespace, e.Target.Name, e.Reason)
			}
		}
	}
	if foundIncompat != 3 {
		t.Errorf("incompatible-excluded count: got %d want 3", foundIncompat)
	}
	if foundReview != 2 {
		t.Errorf("review-excluded count: got %d want 2", foundReview)
	}
}

func TestPlan_IncludeReviewAddsThemToSteps(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{IncludeReview: true})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// 3 Compatible + 2 Review -> 5 steps. 3 Incompatible -> 3 excluded.
	if got := plan.Spec.Summary.Included; got != 5 {
		t.Errorf("Summary.Included with IncludeReview: got %d want 5", got)
	}
	if got := plan.Spec.Summary.Excluded; got != 3 {
		t.Errorf("Summary.Excluded with IncludeReview: got %d want 3", got)
	}
}

func TestPlan_OrdersLowestRiskFirst(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Spec.Steps) != 3 {
		t.Fatalf("step count: got %d want 3", len(plan.Spec.Steps))
	}

	// Expected order, derived by hand from ordering.Score():
	//   - Pod "default/sleeper": Pod weight 1, no reasons -> 1.
	//   - Deployment "default/web": Deployment 0 + network-throughput 2 -> 2.
	//   - StatefulSet "data/cache": StatefulSet 5 + syscall-heavy 1 -> 6.
	wantOrder := []string{"sleeper", "web", "cache"}
	for i, w := range wantOrder {
		got := plan.Spec.Steps[i].Target.Name
		if got != w {
			t.Errorf("step %d: got name %q want %q (full order: %v)",
				i, got, w, namesOf(plan.Spec.Steps))
		}
	}

	// Risk scores should be ascending (or equal, then tiebreak).
	for i := 1; i < len(plan.Spec.Steps); i++ {
		if plan.Spec.Steps[i].RiskScore < plan.Spec.Steps[i-1].RiskScore {
			t.Errorf("steps[%d] risk %d < steps[%d] risk %d (must be ascending)",
				i, plan.Spec.Steps[i].RiskScore, i-1, plan.Spec.Steps[i-1].RiskScore)
		}
	}
}

func TestPlan_PerStepFields(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	deploymentStep := findStep(plan, "Deployment", "default", "web")
	if deploymentStep == nil {
		t.Fatalf("Deployment 'default/web' missing from plan: %v", namesOf(plan.Spec.Steps))
	}
	if deploymentStep.Action != "set-runtime-class" {
		t.Errorf("Action: got %q want %q", deploymentStep.Action, "set-runtime-class")
	}
	if deploymentStep.RuntimeClassName != "gvisor" {
		t.Errorf("RuntimeClassName default: got %q want %q", deploymentStep.RuntimeClassName, "gvisor")
	}
	if !deploymentStep.AddToleration {
		t.Errorf("AddToleration: got false, want true (every step adds the toleration)")
	}
	if deploymentStep.WaitFor != "Ready" {
		t.Errorf("Deployment WaitFor: got %q want %q", deploymentStep.WaitFor, "Ready")
	}

	podStep := findStep(plan, "Pod", "default", "sleeper")
	if podStep == nil {
		t.Fatalf("Pod 'default/sleeper' missing from plan")
	}
	if podStep.WaitFor != "Running" {
		t.Errorf("Pod WaitFor: got %q want %q", podStep.WaitFor, "Running")
	}
}

func TestPlan_PlanHashIsDeterministic(t *testing.T) {
	plan1, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan #1: %v", err)
	}
	plan2, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan #2: %v", err)
	}
	if plan1.Metadata.PlanHash == "" {
		t.Fatalf("PlanHash is empty")
	}
	if plan1.Metadata.PlanHash != plan2.Metadata.PlanHash {
		t.Errorf("PlanHash drifted across runs: %q vs %q",
			plan1.Metadata.PlanHash, plan2.Metadata.PlanHash)
	}

	// Changing an option must change the hash so the namespace annotation
	// distinguishes the two plans.
	plan3, err := Plan(fixtureReport(), Options{IncludeReview: true})
	if err != nil {
		t.Fatalf("Plan #3: %v", err)
	}
	if plan3.Metadata.PlanHash == plan1.Metadata.PlanHash {
		t.Errorf("PlanHash unchanged when IncludeReview toggled: %q", plan3.Metadata.PlanHash)
	}
}

func TestPlan_EmptyReportReturnsEmptyPlan(t *testing.T) {
	report := &schema.ScanReport{
		APIVersion: schema.APIVersion,
		Kind:       schema.KindScanReport,
	}
	plan, err := Plan(report, Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if got := plan.Spec.Summary.Total; got != 0 {
		t.Errorf("Total: got %d want 0", got)
	}
	if len(plan.Spec.Steps) != 0 {
		t.Errorf("Steps: got %d want 0", len(plan.Spec.Steps))
	}
	if plan.Spec.Excluded == nil {
		t.Errorf("Excluded must be non-nil (was nil), to spare consumers a nil-check")
	}
}

func TestPlan_NilReportErrors(t *testing.T) {
	if _, err := Plan(nil, Options{}); err == nil {
		t.Errorf("expected error for nil report")
	}
}

func TestPlan_HonorsCustomRuntimeClassName(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{RuntimeClassName: "kata"})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	for _, s := range plan.Spec.Steps {
		if s.RuntimeClassName != "kata" {
			t.Errorf("step %s/%s RuntimeClassName: got %q want %q",
				s.Target.Namespace, s.Target.Name, s.RuntimeClassName, "kata")
		}
	}
	if plan.Spec.Options.RuntimeClassName != "kata" {
		t.Errorf("Spec.Options.RuntimeClassName: got %q want %q",
			plan.Spec.Options.RuntimeClassName, "kata")
	}
}

func TestScore_PartitionsCorrectly(t *testing.T) {
	// Deployment with no reasons should score 0.
	got := Score(schema.WorkloadResult{Kind: "Deployment"})
	if got != 0 {
		t.Errorf("Deployment no-reasons: got %d want 0", got)
	}
	// StatefulSet with one warn: 5 + 2 = 7.
	got = Score(schema.WorkloadResult{
		Kind: "StatefulSet",
		Reasons: []schema.Reason{
			{RuleID: "host-path-mount", Severity: schema.SeverityWarn},
		},
	})
	if got != 7 {
		t.Errorf("StatefulSet one warn: got %d want 7", got)
	}
	// DaemonSet with two warns: 3 + 3 = 6.
	got = Score(schema.WorkloadResult{
		Kind: "DaemonSet",
		Reasons: []schema.Reason{
			{RuleID: "host-path-mount", Severity: schema.SeverityWarn},
			{RuleID: "gpu-passthrough", Severity: schema.SeverityWarn},
		},
	})
	if got != 6 {
		t.Errorf("DaemonSet two warns: got %d want 6", got)
	}
}

func TestOrder_StableTiebreak(t *testing.T) {
	// Three workloads with the same score should sort by (Namespace, Kind, Name).
	in := []schema.WorkloadResult{
		{Kind: "Deployment", Namespace: "ns2", Name: "z"},
		{Kind: "Deployment", Namespace: "ns1", Name: "y"},
		{Kind: "Deployment", Namespace: "ns1", Name: "a"},
	}
	got := Order(in)
	wantNames := []string{"a", "y", "z"}
	for i, w := range wantNames {
		if got[i].Workload.Name != w {
			t.Errorf("Order[%d]: got %q want %q", i, got[i].Workload.Name, w)
		}
	}
	// Sanity: the input was not mutated (Order returns a new slice).
	if !reflect.DeepEqual(in[0].Name, "z") {
		t.Errorf("Order mutated its input: %v", in)
	}
}

// findStep returns a pointer to the step matching (kind, ns, name) or nil.
func findStep(p *schema.MigrationPlan, kind, ns, name string) *schema.PlanStep {
	for i := range p.Spec.Steps {
		s := &p.Spec.Steps[i]
		if s.Target.Kind == kind && s.Target.Namespace == ns && s.Target.Name == name {
			return s
		}
	}
	return nil
}

func namesOf(steps []schema.PlanStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Target.Name)
	}
	return out
}
