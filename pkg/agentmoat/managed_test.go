// Tests for the deploy tool markers and the plan warning from them.

package agentmoat

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestManagedBy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		labels       map[string]string
		annotations  map[string]string
		wantTool     string
		wantEvidence string
	}{
		{name: "no marker"},
		{
			name:        "Argo CD tracking annotation",
			annotations: map[string]string{"argocd.argoproj.io/tracking-id": "shop:apps/Deployment:shop/web"},
			wantTool:    "argocd", wantEvidence: "annotation argocd.argoproj.io/tracking-id",
		},
		{
			name:     "Argo CD instance label",
			labels:   map[string]string{"argocd.argoproj.io/instance": "shop"},
			wantTool: "argocd", wantEvidence: "label argocd.argoproj.io/instance",
		},
		{
			name:     "Flux kustomize-controller",
			labels:   map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps", "kustomize.toolkit.fluxcd.io/namespace": "flux-system"},
			wantTool: "flux", wantEvidence: "label kustomize.toolkit.fluxcd.io/name",
		},
		{
			// Flux installs a chart with Helm, so the object has the
			// markers of both. Flux owns the source.
			name:        "Flux helm-controller comes before Helm",
			labels:      map[string]string{"helm.toolkit.fluxcd.io/name": "web"},
			annotations: map[string]string{"meta.helm.sh/release-name": "web", "meta.helm.sh/release-namespace": "shop"},
			wantTool:    "flux", wantEvidence: "label helm.toolkit.fluxcd.io/name",
		},
		{
			name:        "Helm",
			annotations: map[string]string{"meta.helm.sh/release-name": "web"},
			wantTool:    "helm", wantEvidence: "annotation meta.helm.sh/release-name",
		},
		{
			name:        "Argo CD comes before Helm",
			annotations: map[string]string{"meta.helm.sh/release-name": "web", "argocd.argoproj.io/tracking-id": "x"},
			wantTool:    "argocd", wantEvidence: "annotation argocd.argoproj.io/tracking-id",
		},
		{
			// Charts and operators write these too. They are not
			// evidence of a deploy tool.
			name:   "the common labels are not a marker",
			labels: map[string]string{"app.kubernetes.io/instance": "web", "app.kubernetes.io/managed-by": "Helm"},
		},
		{
			// A marker key in the other map is not the marker.
			name:        "a label key as an annotation is not a marker",
			annotations: map[string]string{"kustomize.toolkit.fluxcd.io/name": "apps"},
			labels:      map[string]string{"meta.helm.sh/release-name": "web"},
		},
	}
	for _, tc := range tests {
		got := managedBy(tc.labels, tc.annotations)
		if tc.wantTool == "" {
			if got != nil {
				t.Errorf("%s: got %+v, want nil", tc.name, got)
			}
			continue
		}
		if got == nil || got.Tool != tc.wantTool || got.Evidence != tc.wantEvidence {
			t.Errorf("%s: got %+v, want %s (%s)", tc.name, got, tc.wantTool, tc.wantEvidence)
		}
	}
}

// TestScan_RecordsManagedBy: the marker is read from the controller's own
// labels and annotations, and the evidence has no value from the cluster.
func TestScan_RecordsManagedBy(t *testing.T) {
	t.Parallel()
	dep := func(name string, annotations map[string]string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop", Annotations: annotations},
			Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
			}}},
		}
	}
	client := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		dep("api", map[string]string{"argocd.argoproj.io/tracking-id": "secret-app:apps/Deployment:shop/api"}),
		dep("web", nil),
	)
	report, err := Scan(context.Background(), ScanOptions{KubeClient: client, AllNamespaces: true, SkipClusterFacts: true, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Spec.Workloads) != 2 {
		t.Fatalf("workloads = %+v", report.Spec.Workloads)
	}
	api, web := report.Spec.Workloads[0], report.Spec.Workloads[1]
	want := schema.ManagedBy{Tool: "argocd", Evidence: "annotation argocd.argoproj.io/tracking-id"}
	if api.Name != "api" || api.ManagedBy == nil || *api.ManagedBy != want {
		t.Errorf("api: managedBy = %+v, want %+v", api.ManagedBy, want)
	}
	if web.ManagedBy != nil {
		t.Errorf("web: managedBy = %+v, want nil", web.ManagedBy)
	}
}

// managedReport is a stored cluster scan: four compatible workloads (two
// from Argo CD, one from Helm, one with no marker) and one incompatible
// workload with a marker, which is not in a plan.
func managedReport(withMarkers bool) *schema.ScanReport {
	report := schema.NewScanReport()
	row := func(name, tool string, compat schema.Compatibility) schema.WorkloadResult {
		w := schema.WorkloadResult{Kind: "Deployment", Namespace: "shop", Name: name, Compatibility: compat}
		if withMarkers && tool != "" {
			w.ManagedBy = &schema.ManagedBy{Tool: tool, Evidence: "test"}
		}
		return w
	}
	report.Spec.Workloads = []schema.WorkloadResult{
		row("api", schema.ManagedByArgoCD, schema.CompatibilityCompatible),
		row("cart", schema.ManagedByHelm, schema.CompatibilityCompatible),
		row("cni", schema.ManagedByFlux, schema.CompatibilityIncompatible),
		row("jobs", "", schema.CompatibilityCompatible),
		row("web", schema.ManagedByArgoCD, schema.CompatibilityCompatible),
	}
	report.Spec.Summary = schema.Summary{Total: 5, Compatible: 4, Incompatible: 1}
	return report
}

func TestPlan_SourceManagedWarning(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	plan, err := Plan(context.Background(), PlanOptions{ScanReport: managedReport(true), Stderr: stderr})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Spec.Steps) != 4 {
		t.Fatalf("steps = %d, want 4", len(plan.Spec.Steps))
	}
	if len(plan.Spec.Warnings) != 1 || plan.Spec.Warnings[0].ID != WarningSourceManaged {
		t.Fatalf("warnings = %+v, want one %s", plan.Spec.Warnings, WarningSourceManaged)
	}
	msg := plan.Spec.Warnings[0].Message
	// The counts are of plan steps: the incompatible Flux workload is not
	// in the plan, so Flux is not in the list. The tools are in a fixed
	// order.
	if want := "3 of 4 workload(s) in this plan have the marker of a deploy tool (Argo CD: 2, Helm: 1). "; !strings.HasPrefix(msg, want) {
		t.Errorf("message = %q\nwant it to start with %q", msg, want)
	}
	for _, want := range []string{
		"changes the live objects only",
		"Add runtimeClassName to that source too",
		"depends on the tool and its settings",
		"A workload with no marker can also have such a source",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q: %s", want, msg)
		}
	}
	// The message makes no claim that it cannot support for every tool,
	// and names no command that does not exist.
	for _, bad := range []string{"revert", "agentmoat patch", "next sync"} {
		if strings.Contains(strings.ToLower(msg), bad) {
			t.Errorf("the message must not say %q: %s", bad, msg)
		}
	}
	// stderr has the warning, and not the line about the cluster: this
	// warning says nothing about what the cluster can host.
	if !strings.Contains(stderr.String(), "plan: 3 of 4 workload(s)") || strings.Contains(stderr.String(), "cluster warning") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

// TestPlan_ManagedByIsNotInThePlanHash: invariant 3. The same workloads
// give the same plan and the same hash, with or without the markers.
func TestPlan_ManagedByIsNotInThePlanHash(t *testing.T) {
	t.Parallel()
	with, err := Plan(context.Background(), PlanOptions{ScanReport: managedReport(true), Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	without, err := Plan(context.Background(), PlanOptions{ScanReport: managedReport(false), Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if with.Metadata.PlanHash == "" || with.Metadata.PlanHash != without.Metadata.PlanHash {
		t.Errorf("plan hash: %q with markers, %q without; they must be equal", with.Metadata.PlanHash, without.Metadata.PlanHash)
	}
	if len(without.Spec.Warnings) != 0 {
		t.Errorf("warnings with no marker = %+v, want none", without.Spec.Warnings)
	}
	if len(with.Spec.Steps) != len(without.Spec.Steps) {
		t.Fatalf("steps differ: %d and %d", len(with.Spec.Steps), len(without.Spec.Steps))
	}
	for i := range with.Spec.Steps {
		if with.Spec.Steps[i] != without.Spec.Steps[i] {
			t.Errorf("step %d differs: %+v and %+v", i, with.Spec.Steps[i], without.Spec.Steps[i])
		}
	}
}

// TestPlan_SourceManagedWarning_NotForAPreview: a plan from manifests
// changes no live object, so there is nothing that the source does not have.
func TestPlan_SourceManagedWarning_NotForAPreview(t *testing.T) {
	t.Parallel()
	report := managedReport(true)
	report.Metadata.Source = &schema.ReportSource{Type: schema.SourceManifests}
	plan, err := Plan(context.Background(), PlanOptions{ScanReport: report, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Spec.Warnings) != 1 || plan.Spec.Warnings[0].ID != WarningManifestSource {
		t.Errorf("warnings = %+v, want only %s", plan.Spec.Warnings, WarningManifestSource)
	}
}

// TestPlan_SourceManagedWarning_AfterClusterWarnings: the cluster warnings
// say if the plan can land at all, so they come first.
func TestPlan_SourceManagedWarning_AfterClusterWarnings(t *testing.T) {
	t.Parallel()
	report := managedReport(true)
	report.Metadata.ClusterFacts = &schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor", Found: false},
		Nodes:        schema.NodeFacts{Total: 3, Ready: 3},
	}
	stderr := &bytes.Buffer{}
	plan, err := Plan(context.Background(), PlanOptions{ScanReport: report, Stderr: stderr})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	n := len(plan.Spec.Warnings)
	if n < 2 || plan.Spec.Warnings[n-1].ID != WarningSourceManaged || plan.Spec.Warnings[0].ID == WarningSourceManaged {
		t.Errorf("warnings = %+v, want the cluster warnings and then %s", plan.Spec.Warnings, WarningSourceManaged)
	}
	if !strings.Contains(stderr.String(), "1 cluster warning(s)") {
		t.Errorf("the count of cluster warnings must not include %s: %q", WarningSourceManaged, stderr.String())
	}
}
