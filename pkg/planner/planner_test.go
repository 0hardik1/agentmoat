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
				// Compatible but not plannable: a busybox Pod (no rules
				// fired). Standalone Pods are excluded by the kind gate:
				// spec.runtimeClassName is immutable on a live Pod.
				{
					Kind:          "Pod",
					Namespace:     "default",
					Name:          "sleeper",
					Compatibility: schema.CompatibilityCompatible,
				},
				// Compatible but not plannable: a one-shot Job. Excluded by
				// the kind gate: Job spec.template is immutable.
				{
					Kind:          "Job",
					Namespace:     "batch",
					Name:          "reindex",
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
					Kind:          "Deployment",
					Namespace:     "default",
					Name:          "gpu-worker",
					Compatibility: schema.CompatibilityReview,
					Reasons: []schema.Reason{
						{RuleID: "gpu-passthrough", Severity: schema.SeverityWarn},
					},
				},
				// Review: hostPath mount.
				{
					Kind:          "DaemonSet",
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

	// 4 Compatible, but the Pod and the Job are blocked by the kind gate
	// -> 2 steps. 2 Review + 3 Incompatible + 2 kind-gated -> 7 excluded.
	if got := plan.Spec.Summary.Total; got != 9 {
		t.Errorf("Summary.Total: got %d want 9", got)
	}
	if got := plan.Spec.Summary.Included; got != 2 {
		t.Errorf("Summary.Included: got %d want 2", got)
	}
	if got := plan.Spec.Summary.Excluded; got != 7 {
		t.Errorf("Summary.Excluded: got %d want 7", got)
	}

	// Spot-check that the excluded list carries the right kind of reason.
	foundIncompat := 0
	foundReview := 0
	foundKindGate := 0
	for _, e := range plan.Spec.Excluded {
		if strings.Contains(e.Reason, "cannot patch in place") {
			foundKindGate++
			continue
		}
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
	if foundKindGate != 2 {
		t.Errorf("kind-gated excluded count: got %d want 2 (Pod + Job)", foundKindGate)
	}
}

func TestPlan_ExcludesPodAndJobKinds(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// Neither the compatible Pod nor the compatible Job may appear as a
	// step: the API server rejects in-place runtimeClassName patches for
	// both kinds.
	if s := findStep(plan, "Pod", "default", "sleeper"); s != nil {
		t.Errorf("Pod 'default/sleeper' must not be a plan step (immutable spec)")
	}
	if s := findStep(plan, "Job", "batch", "reindex"); s != nil {
		t.Errorf("Job 'batch/reindex' must not be a plan step (immutable template)")
	}

	// Both must be excluded with an actionable reason.
	wantExcluded := map[string]string{
		"sleeper": "immutable",
		"reindex": "immutable",
	}
	for _, e := range plan.Spec.Excluded {
		want, ok := wantExcluded[e.Target.Name]
		if !ok {
			continue
		}
		delete(wantExcluded, e.Target.Name)
		if !strings.Contains(e.Reason, want) {
			t.Errorf("excluded %s: reason %q should mention %q", e.Target.Name, e.Reason, want)
		}
		if e.Compatibility != schema.CompatibilityCompatible {
			t.Errorf("excluded %s: compatibility should stay %q, got %q",
				e.Target.Name, schema.CompatibilityCompatible, e.Compatibility)
		}
	}
	for name := range wantExcluded {
		t.Errorf("workload %q missing from the excluded list", name)
	}
}

func TestPlan_IncludeReviewAddsThemToSteps(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{IncludeReview: true})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	// 2 plannable Compatible + 2 Review -> 4 steps.
	// 3 Incompatible + 2 kind-gated (Pod, Job) -> 5 excluded.
	if got := plan.Spec.Summary.Included; got != 4 {
		t.Errorf("Summary.Included with IncludeReview: got %d want 4", got)
	}
	if got := plan.Spec.Summary.Excluded; got != 5 {
		t.Errorf("Summary.Excluded with IncludeReview: got %d want 5", got)
	}
}

func TestPlan_OrdersLowestRiskFirst(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Spec.Steps) != 2 {
		t.Fatalf("step count: got %d want 2", len(plan.Spec.Steps))
	}

	// Expected order, derived by hand from ordering.Score():
	//   - Deployment "default/web": Deployment 0 + network-throughput 2 -> 2.
	//   - StatefulSet "data/cache": StatefulSet 5 + syscall-heavy 1 -> 6.
	wantOrder := []string{"web", "cache"}
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
	if deploymentStep.AddToleration {
		t.Errorf("AddToleration: got true, want false (placement is the RuntimeClass's job unless --add-toleration)")
	}
	if deploymentStep.WaitFor != "Ready" {
		t.Errorf("Deployment WaitFor: got %q want %q", deploymentStep.WaitFor, "Ready")
	}

	stsStep := findStep(plan, "StatefulSet", "data", "cache")
	if stsStep == nil {
		t.Fatalf("StatefulSet 'data/cache' missing from plan")
	}
	if stsStep.WaitFor != "Ready" {
		t.Errorf("StatefulSet WaitFor: got %q want %q", stsStep.WaitFor, "Ready")
	}
}

// TestPlan_AddTolerationOptIn: the toleration is off by default (the
// RuntimeClass's scheduling block owns placement) and on for every step
// when the option is set. Because PlannerOptions is hashed, the two plans
// must carry different hashes: an operator cannot apply an opt-in plan
// against a namespace stamped by the default one and be told
// "already-applied".
func TestPlan_AddTolerationOptIn(t *testing.T) {
	base, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan(default): %v", err)
	}
	optIn, err := Plan(fixtureReport(), Options{AddToleration: true})
	if err != nil {
		t.Fatalf("Plan(AddToleration): %v", err)
	}
	if len(optIn.Spec.Steps) == 0 || len(optIn.Spec.Steps) != len(base.Spec.Steps) {
		t.Fatalf("step count: default %d, opt-in %d", len(base.Spec.Steps), len(optIn.Spec.Steps))
	}
	for i, st := range optIn.Spec.Steps {
		if !st.AddToleration {
			t.Errorf("step %d: AddToleration false with the option set", i)
		}
		if base.Spec.Steps[i].AddToleration {
			t.Errorf("step %d: AddToleration true without the option", i)
		}
	}
	if !optIn.Spec.Options.AddToleration {
		t.Errorf("Spec.Options.AddToleration not recorded on the plan")
	}
	if base.Metadata.PlanHash == optIn.Metadata.PlanHash {
		t.Errorf("planHash identical with and without AddToleration; the option must be hashed")
	}
}

func TestComputePlanHash_MatchesPlanMetadata(t *testing.T) {
	plan, err := Plan(fixtureReport(), Options{})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	recomputed, err := ComputePlanHash(plan.Spec.Steps, plan.Spec.Options)
	if err != nil {
		t.Fatalf("ComputePlanHash: %v", err)
	}
	if recomputed != plan.Metadata.PlanHash {
		t.Errorf("recomputed hash %q != plan hash %q", recomputed, plan.Metadata.PlanHash)
	}

	// nil and empty step slices must hash identically: a plan with zero
	// steps round-trips through YAML as an absent (nil) list.
	h1, err := ComputePlanHash(nil, Options{})
	if err != nil {
		t.Fatalf("ComputePlanHash(nil): %v", err)
	}
	h2, err := ComputePlanHash([]schema.PlanStep{}, Options{})
	if err != nil {
		t.Fatalf("ComputePlanHash(empty): %v", err)
	}
	if h1 != h2 {
		t.Errorf("nil vs empty step slice hashes differ: %q vs %q", h1, h2)
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
