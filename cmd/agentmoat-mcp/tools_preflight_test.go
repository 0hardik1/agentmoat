// Unit tests for the preflight_cluster tool. The finding logic lives in
// pkg/preflight and is tested there; here we check the MCP wiring: the
// report shape comes back, ready/not-ready is honest, and the
// runtime_class_name argument reaches the orchestrator.
package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
)

func TestPreflightClusterHandler_NotReady(t *testing.T) {
	t.Parallel()
	// One runc node, no RuntimeClass: the classic day-zero cluster.
	srv := newTestServer(fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "runc-1"}}))

	result, err := callTool(t, srv, "preflight_cluster", map[string]any{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.PreflightReport
	readTool(t, result, &report)
	if report.Kind != schema.KindPreflightReport {
		t.Fatalf("kind = %q", report.Kind)
	}
	if report.Metadata.RuntimeClassName != "gvisor" {
		t.Errorf("RuntimeClassName default = %q, want gvisor", report.Metadata.RuntimeClassName)
	}
	if report.Spec.Summary.Ready || report.Spec.Summary.Error != 1 {
		t.Fatalf("summary = %+v, want 1 error / not ready", report.Spec.Summary)
	}
	if report.Spec.Findings[0].ID != preflight.FindingRuntimeClassMissing {
		t.Errorf("first finding = %s", report.Spec.Findings[0].ID)
	}
	if report.Spec.Facts.Nodes.Total != 1 {
		t.Errorf("Facts.Nodes.Total = %d", report.Spec.Facts.Nodes.Total)
	}
}

func TestPreflightClusterHandler_ReadyWithCustomRuntimeClass(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		&nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "sandbox"},
			Handler:    "runsc",
			Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"sandbox": "true"}},
		},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "sb-1", Labels: map[string]string{"sandbox": "true"}},
			Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
		},
	)
	srv := newTestServer(client)

	result, err := callTool(t, srv, "preflight_cluster", map[string]any{"runtime_class_name": "sandbox"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.PreflightReport
	readTool(t, result, &report)
	if report.Metadata.RuntimeClassName != "sandbox" || !report.Spec.Facts.RuntimeClass.Found {
		t.Fatalf("metadata/facts = %+v / %+v", report.Metadata, report.Spec.Facts.RuntimeClass)
	}
	if !report.Spec.Summary.Ready {
		t.Fatalf("summary = %+v findings=%+v, want ready", report.Spec.Summary, report.Spec.Findings)
	}
}
