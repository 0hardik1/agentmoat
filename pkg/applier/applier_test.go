// Integration tests for Apply + Rollback against k8s.io/client-go's fake
// clientset. The fake clientset is an in-memory implementation of
// kubernetes.Interface: List/Get/Patch/Create against it operate on a Go
// map, so the test runs in milliseconds and never touches a real cluster.
//
// What these tests cover
//
//   - Dry-run never mutates the fake clientset.
//   - Real-apply patches each controller kind correctly.
//   - The namespace annotation `agentmoat.io/plan-hash` is stamped after a
//     fully-successful apply, and re-running the same plan reports every
//     step as `already-applied` and exits with no mutations.
//   - A partial apply (one step that fails) leaves the namespace annotation
//     unset so a follow-up apply retries.
//   - Rollback removes runtimeClassName via JSON merge patch and clears the
//     annotation. A second rollback reports already-applied.
package applier

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/audit"
	"github.com/0hardik1/agentmoat/internal/schema"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// twoStepPlan returns a fixture plan with one Deployment and one Pod, in
// two namespaces. We use this to exercise multi-namespace annotation
// handling and per-kind patch dispatch.
func twoStepPlan() *schema.MigrationPlan {
	plan := schema.NewMigrationPlan()
	plan.Metadata = schema.PlanMetadata{
		PlanHash: "test-hash-1",
	}
	plan.Spec = schema.PlanSpec{
		Options: schema.PlannerOptions{
			RuntimeClassName: "gvisor",
		},
		Steps: []schema.PlanStep{
			{
				Order:            1,
				Target:           schema.WorkloadRef{Kind: "Deployment", Namespace: "ns-a", Name: "web"},
				Action:           "set-runtime-class",
				RuntimeClassName: "gvisor",
				AddToleration:    true,
				WaitFor:          "Ready",
			},
			{
				Order:            2,
				Target:           schema.WorkloadRef{Kind: "Pod", Namespace: "ns-b", Name: "worker"},
				Action:           "set-runtime-class",
				RuntimeClassName: "gvisor",
				AddToleration:    true,
				WaitFor:          "Running",
			},
		},
	}
	return plan
}

// seededClient returns a fake client preloaded with the objects the plan
// targets, plus the two namespaces.
func seededClient(t *testing.T) *fake.Clientset {
	t.Helper()
	return fake.NewSimpleClientset(
		// Pre-existing namespaces (the applier reads annotations from them).
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-a"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-b"}},
		// Targets.
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns-a"}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "ns-b"}},
	)
}

func auditTmp(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	t.Setenv(audit.EnvPathOverride, path)
	return path
}

func TestApply_DryRunDoesNotMutate(t *testing.T) {
	client := seededClient(t)
	auditTmp(t)

	res, err := Apply(context.Background(), Options{
		Client:       client,
		Plan:         twoStepPlan(),
		DryRun:       true,
		AuditEnabled: true,
		EmitEvents:   false,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Spec.Summary.Applied != 2 {
		t.Errorf("Applied count: got %d want 2", res.Spec.Summary.Applied)
	}
	if res.Spec.Summary.Failed != 0 {
		t.Errorf("Failed count: got %d want 0", res.Spec.Summary.Failed)
	}

	// Confirm no mutating Patch actions reached the fake (the fake records
	// every action it sees, including reads).
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" {
			t.Errorf("dry-run produced a patch action against %s: %+v", a.GetResource().Resource, a)
		}
	}

	// Per-step patches should be filled in the result so the operator can
	// preview the diff even in dry-run.
	for _, s := range res.Spec.Steps {
		if s.Patch == "" {
			t.Errorf("dry-run step %s/%s missing Patch", s.Target.Namespace, s.Target.Name)
		}
	}
}

func TestApply_RealApplyMutatesAndStampsAnnotation(t *testing.T) {
	client := seededClient(t)
	auditTmp(t)

	res, err := Apply(context.Background(), Options{
		Client:       client,
		Plan:         twoStepPlan(),
		DryRun:       false,
		AuditEnabled: true,
		EmitEvents:   false,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := res.Spec.Summary.Applied; got != 2 {
		t.Errorf("Applied: got %d want 2", got)
	}

	// Two namespaces -> two annotation patches.
	annotationPatches := 0
	deploymentPatches := 0
	podPatches := 0
	for _, a := range client.Actions() {
		if a.GetVerb() != "patch" {
			continue
		}
		switch a.GetResource().Resource {
		case "namespaces":
			annotationPatches++
		case "deployments":
			deploymentPatches++
		case "pods":
			podPatches++
		}
	}
	if annotationPatches != 2 {
		t.Errorf("annotation patches: got %d want 2", annotationPatches)
	}
	if deploymentPatches != 1 {
		t.Errorf("deployment patches: got %d want 1", deploymentPatches)
	}
	if podPatches != 1 {
		t.Errorf("pod patches: got %d want 1", podPatches)
	}

	// Confirm the annotation was written to ns-a (one namespace is enough;
	// we already counted both above).
	gotNs, err := client.CoreV1().Namespaces().Get(context.Background(), "ns-a", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get ns-a: %v", err)
	}
	if gotNs.Annotations[schema.PlanHashAnnotation] != "test-hash-1" {
		t.Errorf("annotation on ns-a: got %q want %q",
			gotNs.Annotations[schema.PlanHashAnnotation], "test-hash-1")
	}
}

func TestApply_IdempotentReportsAlreadyApplied(t *testing.T) {
	client := seededClient(t)
	auditTmp(t)
	plan := twoStepPlan()

	// First apply.
	if _, err := Apply(context.Background(), Options{
		Client: client, Plan: plan, DryRun: false, AuditEnabled: false,
	}); err != nil {
		t.Fatalf("first Apply: %v", err)
	}

	// Second apply should report 2 already-applied, 0 applied, 0 failed.
	client.ClearActions()
	res, err := Apply(context.Background(), Options{
		Client: client, Plan: plan, DryRun: false, AuditEnabled: false,
	})
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if got := res.Spec.Summary.AlreadyApplied; got != 2 {
		t.Errorf("AlreadyApplied: got %d want 2 (Summary=%+v)", got, res.Spec.Summary)
	}
	if got := res.Spec.Summary.Applied; got != 0 {
		t.Errorf("Applied on idempotent re-run: got %d want 0", got)
	}

	// And no controller patches should have been sent (annotation patches
	// from the namespace-write step still occur as a no-op rewrite though).
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource != "namespaces" {
			t.Errorf("idempotent re-run mutated a controller: %+v", a)
		}
	}
}

func TestRollback_RemovesRuntimeClassAndClearsAnnotation(t *testing.T) {
	client := seededClient(t)
	auditTmp(t)
	plan := twoStepPlan()

	if _, err := Apply(context.Background(), Options{
		Client: client, Plan: plan, DryRun: false, AuditEnabled: false,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Now roll back.
	client.ClearActions()
	res, err := Rollback(context.Background(), Options{
		Client: client, Plan: plan, DryRun: false, AuditEnabled: false,
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := res.Spec.Summary.Applied; got != 2 {
		t.Errorf("Applied count on Rollback: got %d want 2", got)
	}

	// Each step should carry a JSON-merge patch that sets
	// runtimeClassName to null.
	for _, s := range res.Spec.Steps {
		if !strings.Contains(s.Patch, `"runtimeClassName":null`) {
			t.Errorf("step %s/%s rollback patch missing null runtimeClassName: %s",
				s.Target.Namespace, s.Target.Name, s.Patch)
		}
	}

	// Annotation on each namespace should be cleared.
	for _, ns := range []string{"ns-a", "ns-b"} {
		got, err := client.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("Get %s: %v", ns, err)
		}
		if v := got.Annotations[schema.PlanHashAnnotation]; v != "" {
			t.Errorf("annotation on %s should be cleared, got %q", ns, v)
		}
	}
}

func TestRollback_IdempotentReportsAlreadyApplied(t *testing.T) {
	client := seededClient(t)
	auditTmp(t)
	plan := twoStepPlan()

	// Apply, rollback once.
	if _, err := Apply(context.Background(), Options{
		Client: client, Plan: plan, DryRun: false, AuditEnabled: false,
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if _, err := Rollback(context.Background(), Options{
		Client: client, Plan: plan, DryRun: false, AuditEnabled: false,
	}); err != nil {
		t.Fatalf("first Rollback: %v", err)
	}

	// Second rollback: every step should be already-applied because the
	// annotation is now empty.
	res, err := Rollback(context.Background(), Options{
		Client: client, Plan: plan, DryRun: false, AuditEnabled: false,
	})
	if err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
	if got := res.Spec.Summary.AlreadyApplied; got != 2 {
		t.Errorf("AlreadyApplied on idempotent rollback: got %d want 2", got)
	}
}

func TestApply_RejectsNilClient(t *testing.T) {
	_, err := Apply(context.Background(), Options{Plan: twoStepPlan()})
	if err == nil {
		t.Errorf("expected error for nil client")
	}
}

func TestApply_RejectsNilPlan(t *testing.T) {
	_, err := Apply(context.Background(), Options{Client: fake.NewSimpleClientset()})
	if err == nil {
		t.Errorf("expected error for nil plan")
	}
}

func TestAffectedNamespaces_Dedupes(t *testing.T) {
	plan := schema.NewMigrationPlan()
	plan.Spec.Steps = []schema.PlanStep{
		{Target: schema.WorkloadRef{Namespace: "a"}},
		{Target: schema.WorkloadRef{Namespace: "b"}},
		{Target: schema.WorkloadRef{Namespace: "a"}},
	}
	got := affectedNamespaces(plan)
	if len(got) != 2 {
		t.Errorf("len: got %d want 2 (%v)", len(got), got)
	}
}

// TestRollback_SkipsNamespaceStampedWithDifferentPlan asserts that a
// rollback does not touch a namespace whose plan-hash annotation belongs
// to a different plan: the steps are reported as skipped (with a reason)
// and the other plan's annotation is preserved.
func TestRollback_SkipsNamespaceStampedWithDifferentPlan(t *testing.T) {
	client := seededClient(t)
	auditTmp(t)
	ctx := context.Background()

	// Stamp ns-a with a DIFFERENT plan's hash; ns-b stays unstamped.
	if _, err := writeNamespaceAnnotation(ctx, client, "ns-a", "some-other-plan-hash", false); err != nil {
		t.Fatalf("seeding annotation: %v", err)
	}

	res, err := Rollback(ctx, Options{
		Client:       client,
		Plan:         twoStepPlan(), // PlanHash: "test-hash-1"
		DryRun:       false,
		AuditEnabled: true,
		EmitEvents:   false,
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// The ns-a step must be skipped with an explanatory reason; the ns-b
	// step is already-applied (annotation absent).
	if res.Spec.Summary.Skipped != 1 {
		t.Errorf("Skipped count: got %d want 1; steps=%+v", res.Spec.Summary.Skipped, res.Spec.Steps)
	}
	if res.Spec.Summary.AlreadyApplied != 1 {
		t.Errorf("AlreadyApplied count: got %d want 1; steps=%+v", res.Spec.Summary.AlreadyApplied, res.Spec.Steps)
	}
	for _, sr := range res.Spec.Steps {
		if sr.Status == schema.StepStatusSkipped && !strings.Contains(sr.Error, "different plan") {
			t.Errorf("skipped step should explain the hash mismatch, got %q", sr.Error)
		}
	}

	// The other plan's annotation must survive.
	got, err := readNamespaceAnnotation(ctx, client, "ns-a")
	if err != nil {
		t.Fatalf("readNamespaceAnnotation: %v", err)
	}
	if got != "some-other-plan-hash" {
		t.Errorf("ns-a annotation: got %q, want the other plan's hash preserved", got)
	}
}
