// Unit tests for the scan_cluster tool. The handler is exercised end-to-
// end against a fake clientset, mirroring pkg/applier/applier_test.go's
// pattern: build a clientset with seed objects, call the handler, assert
// on the JSON body that comes back via mcp.CallToolResult.
package main

import (
	"errors"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func deployment(ns, name, image string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: image}},
				},
			},
		},
	}
}

func TestScanClusterHandler_HappyPath(t *testing.T) {
	t.Parallel()
	ns := "agentmoat-test"
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}},
		deployment(ns, "a", "nginx:1.25"),
		deployment(ns, "b", "redis:7"),
	)
	srv := newTestServer(client)

	result, err := callTool(t, srv, "scan_cluster", map[string]any{
		"namespaces": []any{ns},
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.ScanReport
	readTool(t, result, &report)

	if report.Kind != schema.KindScanReport {
		t.Errorf("Kind: got %q, want %q", report.Kind, schema.KindScanReport)
	}
	if report.Spec.Summary.Total != 2 {
		t.Errorf("Total: got %d, want 2", report.Spec.Summary.Total)
	}
	if got := len(report.Spec.Workloads); got != 2 {
		t.Errorf("Workloads: got %d, want 2", got)
	}
}

func TestScanClusterHandler_ListErrorPropagates(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	// Force the Deployments List() call to fail. The scanner enumerates
	// six kinds; this is the first one, so the failure short-circuits the
	// whole scan.
	client.PrependReactor("list", "deployments", func(_ ktesting.Action) (handled bool, ret runtime.Object, err error) {
		return true, nil, errors.New("simulated kube failure")
	})
	srv := newTestServer(client)

	result, err := callTool(t, srv, "scan_cluster", map[string]any{})
	if err != nil {
		t.Fatalf("unexpected go-level error: %v", err)
	}
	// The orchestrator wraps as "enumerating workloads"; the
	// tool-result error should surface that text.
	expectToolError(t, result, "simulated kube failure")
}
