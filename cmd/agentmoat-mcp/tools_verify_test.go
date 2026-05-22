// Unit tests for the verify_migration tool. The probe path (in_pod_probe)
// is covered in detail by pkg/verifier/verifier_test.go; here we verify
// the MCP wiring plus one probe-on round-trip using a fake ExecRunner so
// the SPDY-exec path is bypassed.
package main

import (
	"context"
	"io"
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

// strPtr is a single-line helper because *string parameters appear in
// pod specs (RuntimeClassName) and Go has no shorthand for that.
func strPtr(s string) *string { return &s }

func runningPodWithRCN(ns, name string, labels map[string]string, rcn *string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			RuntimeClassName: rcn,
			Containers:       []corev1.Container{{Name: "main", Image: "nginx"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func deploymentWithSelector(ns, name string, sel map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
		},
	}
}

// writeVerifyPlan writes a one-step plan targeting the named Deployment.
func writeVerifyPlan(t *testing.T, ns, name string) string {
	t.Helper()
	plan := schema.NewMigrationPlan()
	plan.Metadata = schema.PlanMetadata{GeneratedAt: "2026-05-22T12:00:00Z", PlanHash: "h"}
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
	data, _ := yaml.Marshal(plan)
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

func TestVerifyMigrationHandler_HappyPath(t *testing.T) {
	t.Parallel()
	ns, name := "agentmoat-test", "web"
	labels := map[string]string{"app": "web"}
	client := fake.NewSimpleClientset(
		deploymentWithSelector(ns, name, labels),
		runningPodWithRCN(ns, "web-0", labels, strPtr("gvisor")),
	)
	srv := newTestServer(client)
	planPath := writeVerifyPlan(t, ns, name)

	result, err := callTool(t, srv, "verify_migration", map[string]any{
		"plan_path": planPath,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.VerifyReport
	readTool(t, result, &report)
	if report.Spec.Summary.OK != 1 {
		t.Errorf("Summary.OK: got %d, want 1; report=%+v", report.Spec.Summary.OK, report)
	}
}

// fakeProbe is a one-shot ExecRunner that returns the configured bytes.
// Allows the in_pod_probe path to be exercised without dialing SPDY.
type fakeProbe struct {
	stdout, stderr []byte
	err            error
}

func (f *fakeProbe) Exec(_ context.Context, _, _, _ string, _ []string) ([]byte, []byte, error) {
	return f.stdout, f.stderr, f.err
}

func TestVerifyMigrationHandler_InPodProbe(t *testing.T) {
	t.Parallel()
	ns, name := "agentmoat-test", "web"
	labels := map[string]string{"app": "web"}
	client := fake.NewSimpleClientset(
		deploymentWithSelector(ns, name, labels),
		runningPodWithRCN(ns, "web-0", labels, strPtr("gvisor")),
	)

	// Inject a fake probe that returns a gvisor-marker line so the
	// verifier's probe assertion stays OK.
	srv := newServer(Deps{
		Stderr:     io.Discard,
		KubeClient: client,
		ExecRunner: &fakeProbe{stdout: []byte("Linux x.y.z gVisor\n")},
	})

	result, err := callTool(t, srv, "verify_migration", map[string]any{
		"plan_path":    writeVerifyPlan(t, ns, name),
		"in_pod_probe": true,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.VerifyReport
	readTool(t, result, &report)
	if !report.Metadata.InPodProbe {
		t.Errorf("Metadata.InPodProbe: got false, want true")
	}
	if report.Spec.Summary.OK != 1 {
		t.Errorf("Summary.OK: got %d, want 1", report.Spec.Summary.OK)
	}
}
