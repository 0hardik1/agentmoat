// Unit tests for the assess_workload tool. The classifier reasons-fired
// path is covered in pkg/classifier; here we only check the MCP wiring:
// argument parsing, schema-envelope correctness, error wrapping.
package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func TestAssessWorkloadHandler_PrivilegedPod(t *testing.T) {
	t.Parallel()
	priv := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "naughty", Namespace: "agentmoat-test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "main",
				Image:           "nginx:1.25",
				SecurityContext: &corev1.SecurityContext{Privileged: &priv},
			}},
		},
	}
	client := fake.NewSimpleClientset(pod)
	srv := newTestServer(client)

	result, err := callTool(t, srv, "assess_workload", map[string]any{
		"kind":      "Pod",
		"namespace": "agentmoat-test",
		"name":      "naughty",
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var w schema.WorkloadResult
	readTool(t, result, &w)

	if w.Compatibility != schema.CompatibilityIncompatible {
		t.Errorf("Compatibility: got %q, want %q", w.Compatibility, schema.CompatibilityIncompatible)
	}
	if w.Name != "naughty" {
		t.Errorf("Name: got %q, want naughty", w.Name)
	}
}

func TestAssessWorkloadHandler_NotFound(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset())
	result, _ := callTool(t, srv, "assess_workload", map[string]any{
		"kind":      "Deployment",
		"namespace": "ns",
		"name":      "missing",
	})
	expectToolError(t, result, "missing")
}

func TestAssessWorkloadHandler_RequiredFieldsEnforced(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset())

	// Missing kind: the MCP request accessor's RequireString returns an
	// error, which the handler converts to a tool-result error.
	result, _ := callTool(t, srv, "assess_workload", map[string]any{
		"namespace": "ns",
		"name":      "x",
	})
	expectToolError(t, result, "kind")
}
