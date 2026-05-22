// Unit tests for the propose_plan tool. Two flows: inline scan-then-plan
// (using a fake clientset) and scan-from-disk (writing a ScanReport to a
// temp file and passing scan_report_path).
package main

import (
	"os"
	"path/filepath"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func compatibleDeployment(ns, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx:1.25"}},
				},
			},
		},
	}
}

func TestProposePlanHandler_InlineScan(t *testing.T) {
	t.Parallel()
	ns := "agentmoat-test"
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		compatibleDeployment(ns, "web"),
	)
	srv := newTestServer(client)

	result, err := callTool(t, srv, "propose_plan", map[string]any{
		"namespaces": []any{ns},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var plan schema.MigrationPlan
	readTool(t, result, &plan)

	if plan.Kind != schema.KindMigrationPlan {
		t.Errorf("Kind: got %q, want %q", plan.Kind, schema.KindMigrationPlan)
	}
	if plan.Metadata.PlanHash == "" {
		t.Errorf("PlanHash should be populated")
	}
	if plan.Spec.Summary.Included != 1 {
		t.Errorf("Included: got %d, want 1", plan.Spec.Summary.Included)
	}
}

func TestProposePlanHandler_FromScanReportPath(t *testing.T) {
	t.Parallel()

	// Build a minimal ScanReport on disk.
	report := schema.NewScanReport()
	report.Metadata = schema.ReportMetadata{GeneratedAt: "2026-05-22T12:00:00Z"}
	report.Spec = schema.ReportSpec{
		Summary: schema.Summary{Total: 1, Compatible: 1},
		Workloads: []schema.WorkloadResult{{
			Kind: "Deployment", Namespace: "agentmoat-test", Name: "web",
			Compatibility: schema.CompatibilityCompatible,
		}},
	}
	data, err := yaml.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "scan.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := newTestServer(fake.NewSimpleClientset())
	result, err := callTool(t, srv, "propose_plan", map[string]any{
		"scan_report_path": path,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var plan schema.MigrationPlan
	readTool(t, result, &plan)
	if plan.Spec.Summary.Included != 1 {
		t.Errorf("Included: got %d, want 1", plan.Spec.Summary.Included)
	}
}

func TestProposePlanHandler_WrongKindFile(t *testing.T) {
	t.Parallel()
	// Write a MigrationPlan where a ScanReport is expected.
	wrong := schema.NewMigrationPlan()
	data, err := yaml.Marshal(wrong)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "wrong.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := newTestServer(fake.NewSimpleClientset())
	result, _ := callTool(t, srv, "propose_plan", map[string]any{
		"scan_report_path": path,
	})
	expectToolError(t, result, "ScanReport")
}
