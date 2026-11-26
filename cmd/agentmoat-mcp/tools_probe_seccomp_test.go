// Unit tests for the probe_seccomp tool: the dry-run gate. pkg/seccompprobe
// covers the trials, the reader and the findings.
package main

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	"github.com/0hardik1/agentmoat/pkg/seccompprobe"
)

func TestProbeSeccompHandler_DryRunByDefault(t *testing.T) {
	t.Parallel()
	gv := "gvisor"
	objs := append(gvisorReadyObjects(),
		&nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: seccompprobe.DefaultAuditRuntimeClass}, Handler: seccompprobe.DefaultAuditRuntimeClass},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: "web"},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RuntimeClassName: &gv, Containers: []corev1.Container{{Name: "app", Image: "example/app:1"}},
			}}},
		})
	client := fake.NewSimpleClientset(objs...)
	srv := newTestServer(client)

	for _, args := range []map[string]any{{}, {"dry_run": true}} {
		result, err := callTool(t, srv, "probe_seccomp", args)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		var report schema.PreflightReport
		readTool(t, result, &report)
		if report.Metadata.SeccompProbe == nil || !report.Metadata.SeccompProbe.DryRun {
			t.Fatalf("args %v: metadata = %+v", args, report.Metadata.SeccompProbe)
		}
		found := false
		for _, f := range report.Spec.Findings {
			found = found || f.ID == preflight.FindingSeccompProbeDryRun
		}
		if !found || len(report.Spec.Facts.Seccomp.Workloads) != 1 {
			t.Fatalf("args %v: findings %+v", args, report.Spec.Findings)
		}
	}
	if podActions(client, "create") != 0 {
		t.Fatal("dry run created a pod")
	}
}
