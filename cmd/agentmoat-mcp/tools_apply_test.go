// Unit tests for apply_plan and rollback_plan, with the load-bearing
// dry-run gate at center stage. The applier's own per-step behavior is
// covered in pkg/applier/applier_test.go; here we verify only the MCP
// wiring: argument parsing, the dry-run default, the explicit-false
// mutation path, the non-bool rejection.
package main

import (
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/internal/audit"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/planner"
)

// writeApplyPlan emits a one-step MigrationPlan on disk targeting the
// supplied namespace/name. The Spec.Steps[0].Target controls what the
// applier patches, which is what the dry-run assertion below hinges on.
func writeApplyPlan(t *testing.T, ns, name string) string {
	t.Helper()
	plan := schema.NewMigrationPlan()
	plan.Spec = schema.PlanSpec{
		Summary: schema.PlanSummary{Total: 1, Included: 1},
		Options: schema.PlannerOptions{RuntimeClassName: "gvisor"},
		Steps: []schema.PlanStep{{
			Order:            1,
			Target:           schema.WorkloadRef{Kind: "Deployment", Namespace: ns, Name: name},
			Action:           "set-runtime-class",
			RuntimeClassName: "gvisor",
		}},
		Excluded: []schema.ExcludedWorkload{},
	}
	// The loader verifies metadata.planHash against the steps/options, so
	// the fixture must carry the real content hash, not a placeholder.
	hash, err := planner.ComputePlanHash(plan.Spec.Steps, plan.Spec.Options)
	if err != nil {
		t.Fatalf("compute plan hash: %v", err)
	}
	plan.Metadata = schema.PlanMetadata{
		GeneratedAt: "2026-05-22T12:00:00Z",
		PlanHash:    hash,
	}
	data, err := yaml.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

// seededApplyClient returns a fake clientset pre-loaded with the
// namespace and deployment the test plan targets, so a real apply call
// has somewhere to write the patch.
func seededApplyClient(ns, name string) *fake.Clientset {
	return fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:1.25"}},
				}},
			},
		},
	)
}

// patchCount returns the number of `patch` actions the fake recorded
// against any resource. The applier's dry-run path never patches, so
// this is the canary the dry-run-gate tests assert against.
func patchCount(c *fake.Clientset) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

func TestApplyPlanHandler_DefaultsToDryRun(t *testing.T) {
	// Send each apply call's audit log into a temp file so concurrent test
	// runs don't fight over ~/.agentmoat/audit.jsonl.
	t.Setenv(audit.EnvPathOverride, filepath.Join(t.TempDir(), "audit.jsonl"))

	ns, name := "agentmoat-test", "web"
	client := seededApplyClient(ns, name)
	srv := newTestServer(client)

	result, err := callTool(t, srv, "apply_plan", map[string]any{
		"plan_path": writeApplyPlan(t, ns, name),
		// dry_run intentionally omitted; the handler must default to true.
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var ar schema.ApplyResult
	readTool(t, result, &ar)
	if !ar.Metadata.DryRun {
		t.Errorf("Metadata.DryRun: got false, want true (handler must default to dry-run)")
	}
	if got := patchCount(client); got != 0 {
		t.Errorf("dry-run produced %d patch actions, want 0; actions=%v", got, client.Actions())
	}
}

func TestApplyPlanHandler_ExplicitFalseMutates(t *testing.T) {
	t.Setenv(audit.EnvPathOverride, filepath.Join(t.TempDir(), "audit.jsonl"))

	ns, name := "agentmoat-test", "web"
	client := seededApplyClient(ns, name)
	// Auto-mark every patched Deployment as "rolled out" so the applier's
	// readiness wait does not block forever against the fake clientset.
	client.PrependReactor("patch", "deployments", func(a ktesting.Action) (handled bool, ret runtime.Object, err error) {
		pa := a.(ktesting.PatchAction)
		dep := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: pa.GetName(), Namespace: pa.GetNamespace()},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: 1,
				Conditions: []appsv1.DeploymentCondition{
					{Type: appsv1.DeploymentProgressing, Reason: "NewReplicaSetAvailable"},
				},
				Replicas:        1,
				UpdatedReplicas: 1,
				ReadyReplicas:   1,
			},
		}
		return false, dep, nil
	})
	srv := newTestServer(client)

	result, err := callTool(t, srv, "apply_plan", map[string]any{
		"plan_path": writeApplyPlan(t, ns, name),
		"dry_run":   false,
		// Skip audit and event emission to keep the assertion focused on
		// the patch action.
		"emit_events":   false,
		"audit_enabled": false,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var ar schema.ApplyResult
	readTool(t, result, &ar)
	if ar.Metadata.DryRun {
		t.Errorf("Metadata.DryRun: got true, want false (handler must propagate explicit false)")
	}
	if got := patchCount(client); got == 0 {
		t.Errorf("explicit dry_run=false produced 0 patches; applier did not run")
	}
}

func TestApplyPlanHandler_NonBoolDryRunRejected(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset())
	result, _ := callTool(t, srv, "apply_plan", map[string]any{
		"plan_path": "/dev/null", // Never reached: dry_run is rejected first.
		"dry_run":   "yes",
	})
	expectToolError(t, result, "boolean")
}

func TestRollbackPlanHandler_DefaultsToDryRun(t *testing.T) {
	t.Setenv(audit.EnvPathOverride, filepath.Join(t.TempDir(), "audit.jsonl"))

	ns, name := "agentmoat-test", "web"
	client := seededApplyClient(ns, name)
	srv := newTestServer(client)

	result, err := callTool(t, srv, "rollback_plan", map[string]any{
		"plan_path": writeApplyPlan(t, ns, name),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var rb schema.RollbackResult
	readTool(t, result, &rb)
	if !rb.Metadata.DryRun {
		t.Errorf("Metadata.DryRun: got false, want true (rollback handler must default to dry-run)")
	}
	if got := patchCount(client); got != 0 {
		t.Errorf("rollback dry-run produced %d patches, want 0", got)
	}
}
