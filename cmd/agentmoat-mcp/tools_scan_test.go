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

// e2eWorkloads is the manifest file `make e2e` applies to the kind cluster:
// 14 workloads, of which 8 are compatible, 3 need review, 3 are incompatible.
const e2eWorkloads = "../../test/e2e/manifests/workloads.yaml"

// TestScanClusterHandler_ManifestPaths: with manifest_paths the tool reads
// files and contacts no cluster. The injected client (which the handler
// always passes on) fails the test if it is called.
func TestScanClusterHandler_ManifestPaths(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	client.PrependReactor("*", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		t.Errorf("scan_cluster with manifest_paths called the Kubernetes API: %s %s", action.GetVerb(), action.GetResource().Resource)
		return true, nil, errors.New("no API calls in a manifest scan")
	})
	srv := newTestServer(client)

	result, err := callTool(t, srv, "scan_cluster", map[string]any{
		"manifest_paths": []any{e2eWorkloads},
		"namespaces":     []any{"agentmoat-e2e"},
		// Cluster inputs have no effect in this mode.
		"context":        "does-not-exist",
		"include_system": true,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.ScanReport
	readTool(t, result, &report)

	want := schema.Summary{Total: 14, Compatible: 8, NeedsReview: 3, Incompatible: 3}
	if report.Spec.Summary != want {
		t.Errorf("summary = %+v, want %+v", report.Spec.Summary, want)
	}
	if report.Metadata.Source == nil || report.Metadata.Source.Type != schema.SourceManifests {
		t.Errorf("source = %+v, want manifests", report.Metadata.Source)
	}
	if report.Metadata.Cluster != "" || report.Metadata.ClusterFacts != nil {
		t.Errorf("the report claims a cluster: %q, %v", report.Metadata.Cluster, report.Metadata.ClusterFacts)
	}
	for _, w := range report.Spec.Workloads {
		if w.Origin == nil || w.Origin.Line == 0 || w.Namespace != "agentmoat-e2e" {
			t.Errorf("%s %s: origin = %+v namespace = %q", w.Kind, w.Name, w.Origin, w.Namespace)
		}
	}
	if n := len(client.Actions()); n != 0 {
		t.Errorf("%d API actions, want 0", n)
	}
}

// TestScanClusterHandler_ManifestPathsErrors: bad input is a tool error with
// the same text the CLI prints.
func TestScanClusterHandler_ManifestPathsErrors(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset())

	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			// The server's standard input is the JSON-RPC channel.
			name: "standard input is not available",
			args: map[string]any{"manifest_paths": []any{"-"}},
			want: "standard input",
		},
		{
			name: "a path that does not exist",
			args: map[string]any{"manifest_paths": []any{"/no/such/manifests"}},
			want: "/no/such/manifests",
		},
		{
			name: "two namespaces",
			args: map[string]any{"manifest_paths": []any{e2eWorkloads}, "namespaces": []any{"a", "b"}},
			want: "one namespace",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result, err := callTool(t, srv, "scan_cluster", tc.args)
			if err != nil {
				t.Fatalf("call: %v", err)
			}
			expectToolError(t, result, tc.want)
		})
	}
}
