// Tests for the Apply orchestrator's preflight gate. The applier's per-step
// behavior is covered in pkg/applier; here we pin the contract the CLI and
// MCP server rely on: a not-ready cluster yields a blocked ApplyResult (no
// Go error, every step skipped, findings attached, nothing patched), the
// gate runs in dry-run too, --skip-preflight bypasses it, and a passing
// preflight rides along on the result.
package agentmoat

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
)

// applyWorkload is the namespace + Deployment that writeTestPlan targets.
func applyWorkload() []runtime.Object {
	return []runtime.Object{
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ns-a"}},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ns-a"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
			}}},
		},
	}
}

// gvisorReady is the RuntimeClass + labeled Ready node the gate wants.
func gvisorReady() []runtime.Object {
	return []runtime.Object{
		&nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "gvisor",
			Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"runtime": "gvisor"}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "gv-1", Labels: map[string]string{"runtime": "gvisor"}},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		},
	}
}

func patchActions(c *fake.Clientset) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == "patch" {
			n++
		}
	}
	return n
}

// assertBlockedByMissingRuntimeClass checks the shape of an ApplyResult the
// preflight refused: not ready, one error, the single step skipped with the
// finding id in its error text, the finding attached, the plan hash kept.
func assertBlockedByMissingRuntimeClass(t *testing.T, res *schema.ApplyResult) {
	t.Helper()
	if res.Kind != schema.KindApplyResult {
		t.Fatalf("kind = %s", res.Kind)
	}
	wantPreflight := &schema.PreflightSummary{Ready: false, Total: 1, Error: 1}
	if !reflect.DeepEqual(res.Metadata.Preflight, wantPreflight) {
		t.Fatalf("Metadata.Preflight = %+v, want %+v", res.Metadata.Preflight, wantPreflight)
	}
	wantSummary := schema.ApplySummary{Total: 1, Skipped: 1}
	if res.Spec.Summary != wantSummary {
		t.Fatalf("Summary = %+v, want %+v", res.Spec.Summary, wantSummary)
	}
	st := res.Spec.Steps[0]
	if st.Status != schema.StepStatusSkipped || !strings.Contains(st.Error, preflight.FindingRuntimeClassMissing) || st.Order != 1 {
		t.Fatalf("step = %+v", st)
	}
	if len(res.Spec.PreflightFindings) != 1 || res.Spec.PreflightFindings[0].ID != preflight.FindingRuntimeClassMissing {
		t.Fatalf("PreflightFindings = %+v", res.Spec.PreflightFindings)
	}
	if res.Metadata.PlanHash == "" {
		t.Fatal("PlanHash not carried onto the blocked result")
	}
}

func TestApply_BlockedByPreflight(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(applyWorkload()...) // no RuntimeClass, no gVisor node
	res, err := Apply(context.Background(), ApplyOptions{
		PlanPath: writeTestPlan(t), DryRun: false, KubeClient: client, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Apply returned an error; a blocked preflight must be a result: %v", err)
	}
	if res.Metadata.DryRun {
		t.Fatal("DryRun = true on a --dry-run=false call")
	}
	assertBlockedByMissingRuntimeClass(t, res)
	if n := patchActions(client); n != 0 {
		t.Fatalf("%d patch actions on a blocked apply, want 0", n)
	}
}

func TestApply_GateRunsInDryRun(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(applyWorkload()...)
	res, err := Apply(context.Background(), ApplyOptions{
		PlanPath: writeTestPlan(t), DryRun: true, KubeClient: client, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !res.Metadata.DryRun {
		t.Fatal("DryRun = false on a dry-run call")
	}
	assertBlockedByMissingRuntimeClass(t, res)
}

func TestApply_SkipPreflight(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(applyWorkload()...)
	res, err := Apply(context.Background(), ApplyOptions{
		PlanPath: writeTestPlan(t), DryRun: true, SkipPreflight: true, KubeClient: client, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Metadata.Preflight != nil || len(res.Spec.PreflightFindings) != 0 {
		t.Fatalf("preflight recorded despite SkipPreflight: %+v", res.Metadata.Preflight)
	}
	if res.Spec.Summary.Applied != 1 {
		t.Fatalf("Summary = %+v, want the dry-run step applied", res.Spec.Summary)
	}
}

func TestApply_ReadyPreflightAttached(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(append(applyWorkload(), gvisorReady()...)...)
	res, err := Apply(context.Background(), ApplyOptions{
		PlanPath: writeTestPlan(t), DryRun: true, KubeClient: client, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Metadata.Preflight == nil || !res.Metadata.Preflight.Ready {
		t.Fatalf("Metadata.Preflight = %+v, want ready", res.Metadata.Preflight)
	}
	if res.Spec.Summary.Applied != 1 || res.Spec.Summary.Skipped != 0 {
		t.Fatalf("Summary = %+v", res.Spec.Summary)
	}
	// The fixture RuntimeClass has no overhead: the info finding rides along
	// without blocking anything.
	if res.Metadata.Preflight.Info != 1 || len(res.Spec.PreflightFindings) != 1 ||
		res.Spec.PreflightFindings[0].ID != preflight.FindingRuntimeClassNoOverhead {
		t.Fatalf("findings = %+v", res.Spec.PreflightFindings)
	}
}

// TestRollback_NeverRunsPreflight: rolling back to runc needs no gVisor
// node, so a runc-only cluster must not block it.
func TestRollback_NeverRunsPreflight(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(applyWorkload()...)
	res, err := Rollback(context.Background(), RollbackOptions{
		PlanPath: writeTestPlan(t), DryRun: true, KubeClient: client, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if res.Metadata.Preflight != nil {
		t.Fatalf("rollback recorded a preflight: %+v", res.Metadata.Preflight)
	}
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "runtimeclasses" || a.GetResource().Resource == "nodes" {
			t.Fatalf("rollback read %s; it must not run the preflight", a.GetResource().Resource)
		}
	}
}
