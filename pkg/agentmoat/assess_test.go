// Tests for the AssessWorkload orchestrator. AssessWorkload is the
// single-workload counterpart to Scan: scanner.GetByRef tests (in
// pkg/scanner/scanner_test.go) cover the fetch path; this file covers
// the orchestrator wiring (registry build, classifier call, schema
// envelope, error wrapping). A fake clientset stands in for the K8s API
// so the test runs with no cluster dependency.
package agentmoat

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// privilegedPod returns a Pod whose only container is privileged. The
// built-in classifier rule "privileged" fires on this so the verdict is
// guaranteed to land at Incompatible regardless of any other rule churn.
func privilegedPod(ns, name string) runtime.Object {
	t := true
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:            "main",
				Image:           "nginx:1.25",
				SecurityContext: &corev1.SecurityContext{Privileged: &t},
			}},
		},
	}
}

func TestAssessWorkload_PrivilegedPod_IsIncompatible(t *testing.T) {
	t.Parallel()
	ns, name := "agentmoat-test", "naughty"
	client := fake.NewSimpleClientset(privilegedPod(ns, name))

	result, err := AssessWorkload(context.Background(), AssessWorkloadOptions{
		KubeClient: client,
		Kind:       "Pod",
		Namespace:  ns,
		Name:       name,
	})
	if err != nil {
		t.Fatalf("AssessWorkload: %v", err)
	}
	if result.Compatibility != schema.CompatibilityIncompatible {
		t.Fatalf("Compatibility: got %q, want %q", result.Compatibility, schema.CompatibilityIncompatible)
	}
	if result.Kind != "Pod" || result.Namespace != ns || result.Name != name {
		t.Errorf("identity mismatch: got %+v", result)
	}
	// At least one reason should fire (the privileged rule, by ID).
	foundPrivileged := false
	for _, r := range result.Reasons {
		if r.RuleID == "privileged" {
			foundPrivileged = true
			break
		}
	}
	if !foundPrivileged {
		t.Errorf("expected 'privileged' rule to fire, got reasons: %+v", result.Reasons)
	}
}

func TestAssessWorkload_MissingFields_Rejected(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts AssessWorkloadOptions
	}{
		{"empty kind", AssessWorkloadOptions{Kind: "", Namespace: "ns", Name: "n", KubeClient: fake.NewSimpleClientset()}},
		{"empty namespace", AssessWorkloadOptions{Kind: "Pod", Namespace: "", Name: "n", KubeClient: fake.NewSimpleClientset()}},
		{"empty name", AssessWorkloadOptions{Kind: "Pod", Namespace: "ns", Name: "", KubeClient: fake.NewSimpleClientset()}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := AssessWorkload(context.Background(), tc.opts)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), "required") {
				t.Errorf("error should mention required fields: %v", err)
			}
		})
	}
}

func TestAssessWorkload_NotFound_WrappedError(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	_, err := AssessWorkload(context.Background(), AssessWorkloadOptions{
		KubeClient: client,
		Kind:       "Deployment",
		Namespace:  "ns",
		Name:       "missing",
	})
	if err == nil {
		t.Fatalf("expected not-found error")
	}
	if !strings.HasPrefix(err.Error(), "assess:") {
		t.Errorf("error should be wrapped with 'assess:': %v", err)
	}
}

func TestAssessWorkload_UnsupportedKind_WrappedError(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	_, err := AssessWorkload(context.Background(), AssessWorkloadOptions{
		KubeClient: client,
		Kind:       "ReplicaSet",
		Namespace:  "ns",
		Name:       "n",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("expected 'unsupported kind' error, got %v", err)
	}
}
