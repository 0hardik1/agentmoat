// Tests for the scanner package. The full Enumerate path is exercised by
// the e2e harness; these unit tests focus on the GetByRef entry point that
// powers pkg/agentmoat.AssessWorkload (and, by extension, the MCP server's
// `assess_workload` tool). A fake clientset stands in for the K8s API so
// the tests run with no cluster dependency, mirroring the pattern in
// pkg/applier/applier_test.go.
package scanner

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// podSpec returns a minimal valid PodSpec with one container so the
// converter helpers have something to copy into Workload.PodSpec.
func podSpec(image string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{Name: "main", Image: image}},
	}
}

// TestGetByRef_AllKinds drives every kind the scanner supports through
// the same fixture clientset so we catch any future divergence between
// the Enumerate-time and GetByRef-time converter calls.
func TestGetByRef_AllKinds(t *testing.T) {
	t.Parallel()

	ns := "agentmoat-test"
	objs := []runtime.Object{
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: ns, Labels: map[string]string{"app": "web"}},
			Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: podSpec("nginx:1.25")}},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ss", Namespace: ns},
			Spec:       appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{Spec: podSpec("redis:7")}},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ds", Namespace: ns},
			Spec:       appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: podSpec("fluent/fluentd:1.18")}},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: ns},
			Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: podSpec("busybox:1.36")}},
		},
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "cron", Namespace: ns},
			Spec: batchv1.CronJobSpec{
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: podSpec("alpine:3.20")}}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: ns},
			Spec:       podSpec("nginx:1.25"),
		},
	}
	client := fake.NewSimpleClientset(objs...)

	cases := []struct {
		kind, name, wantImage string
	}{
		{"Deployment", "dep", "nginx:1.25"},
		{"StatefulSet", "ss", "redis:7"},
		{"DaemonSet", "ds", "fluent/fluentd:1.18"},
		{"Job", "job", "busybox:1.36"},
		{"CronJob", "cron", "alpine:3.20"},
		{"Pod", "pod", "nginx:1.25"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			w, err := GetByRef(context.Background(), client, tc.kind, ns, tc.name)
			if err != nil {
				t.Fatalf("GetByRef %s/%s: %v", tc.kind, tc.name, err)
			}
			if w.Kind != tc.kind || w.Namespace != ns || w.Name != tc.name {
				t.Errorf("identity mismatch: got %+v", w)
			}
			if len(w.ImageRefs) != 1 || w.ImageRefs[0] != tc.wantImage {
				t.Errorf("ImageRefs: got %v, want [%s]", w.ImageRefs, tc.wantImage)
			}
		})
	}
}

func TestGetByRef_UnsupportedKind(t *testing.T) {
	t.Parallel()
	_, err := GetByRef(context.Background(), fake.NewSimpleClientset(), "ReplicaSet", "ns", "name")
	if err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Fatalf("expected 'unsupported kind' error, got %v", err)
	}
}

func TestGetByRef_NilClient(t *testing.T) {
	t.Parallel()
	_, err := GetByRef(context.Background(), nil, "Pod", "ns", "name")
	if err == nil || !strings.Contains(err.Error(), "nil client") {
		t.Fatalf("expected 'nil client' error, got %v", err)
	}
}

func TestGetByRef_MissingNamespaceOrName(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	if _, err := GetByRef(context.Background(), client, "Pod", "", "x"); err == nil {
		t.Errorf("expected error for empty namespace")
	}
	if _, err := GetByRef(context.Background(), client, "Pod", "ns", ""); err == nil {
		t.Errorf("expected error for empty name")
	}
}

func TestGetByRef_NotFound(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	_, err := GetByRef(context.Background(), client, "Deployment", "ns", "missing")
	if err == nil {
		t.Fatalf("expected not-found error, got nil")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should reference the workload name: %v", err)
	}
}
