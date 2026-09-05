// Tests for --facts loading and for facts-aware classification through the
// Scan and AssessWorkload orchestrators.
package agentmoat

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// t4Facts is a probed, all-good GPU cluster: two T4 gVisor nodes whose
// driver the installed runsc lists.
func t4Facts(probed bool) *schema.ClusterFacts {
	f := &schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor", Found: true, Handler: "gvisor", NodeSelector: map[string]string{"runtime": "gvisor"}},
		Nodes:        schema.NodeFacts{Total: 3, Ready: 3, MatchingSelector: 2, MatchingAndReady: 2, MatchingNames: []string{"gv-1", "gv-2"}},
		GPU: &schema.GPUFacts{Nodes: 2, MatchingNodes: 2, Groups: []schema.GPUNodeGroup{
			{Product: "Tesla-T4", Driver: "535.183.06", Nodes: 2, MatchingNodes: 2},
		}},
	}
	if probed {
		f.GPU.Nvproxy = &schema.NvproxyFacts{RunscVersion: "release-20260817.0", SupportedDrivers: []string{"535.183.06"}, Node: "gv-1", ProbedAt: "2026-09-04T00:00:00Z"}
	}
	preflight.RefreshGPUSupport(f)
	return f
}

func writeJSON(t *testing.T, name string, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// gpuPod is a standalone pod requesting one GPU.
func gpuPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-app", Namespace: "ns-gpu"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "app", Image: "nvidia/cuda:12.3.1-base",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}},
		}}},
	}
}

func TestLoadClusterFacts(t *testing.T) {
	t.Parallel()
	pf := schema.NewPreflightReport()
	pf.Spec.Facts = *t4Facts(true)
	sr := schema.NewScanReport()
	sr.Metadata.ClusterFacts = t4Facts(false)
	srNoFacts := schema.NewScanReport()
	plan := schema.NewMigrationPlan()

	cases := []struct {
		name    string
		path    string
		wantErr string
		probed  bool
	}{
		{name: "PreflightReport spec.facts", path: writeJSON(t, "pf.json", pf), probed: true},
		{name: "ScanReport metadata.clusterFacts", path: writeJSON(t, "scan.json", sr)},
		{name: "ScanReport without facts", path: writeJSON(t, "nofacts.json", srNoFacts), wantErr: "no metadata.clusterFacts"},
		{name: "wrong kind", path: writeJSON(t, "plan.json", plan), wantErr: `kind "MigrationPlan"`},
		{name: "missing file", path: filepath.Join(t.TempDir(), "nope.json"), wantErr: "no such file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			facts, err := loadClusterFacts(tc.path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if facts.GPU == nil || facts.GPU.Groups[0].Product != "Tesla-T4" || (facts.GPU.Nvproxy != nil) != tc.probed {
				t.Fatalf("facts = %+v", facts)
			}
		})
	}
}

func TestScan_FactsPathRefinesGPUVerdictWithoutNodeReads(t *testing.T) {
	t.Parallel()
	pf := schema.NewPreflightReport()
	pf.Spec.Facts = *t4Facts(true)
	client := fake.NewSimpleClientset(gpuPod())
	report, err := Scan(context.Background(), ScanOptions{
		KubeClient: client, Namespaces: []string{"ns-gpu"}, Stderr: io.Discard,
		FactsPath: writeJSON(t, "probe.json", pf),
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(report.Spec.Workloads) != 1 {
		t.Fatalf("workloads = %+v", report.Spec.Workloads)
	}
	w := report.Spec.Workloads[0]
	if w.Compatibility != schema.CompatibilityCompatible {
		t.Fatalf("gpu-app = %s, want compatible; reasons %+v", w.Compatibility, w.Reasons)
	}
	if w.Reasons[0].RuleID != "gpu-passthrough" || w.Reasons[0].Severity != schema.SeverityInfo ||
		!strings.Contains(w.Reasons[0].Description, "runsc release-20260817.0 nvproxy supports") {
		t.Fatalf("reason = %+v", w.Reasons[0])
	}
	if report.Metadata.ClusterFacts == nil || report.Metadata.ClusterFacts.GPU.Nvproxy == nil {
		t.Fatal("loaded facts must be recorded in the report")
	}
	for _, a := range client.Actions() {
		if a.GetResource().Resource == "nodes" || a.GetResource().Resource == "runtimeclasses" {
			t.Fatalf("--facts must replace the cluster reads, saw %s %s", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestScan_LiveGPUFactsLeaveDriverUnconfirmed(t *testing.T) {
	t.Parallel()
	objs := append(gvisorReady(), gpuPod())
	gpuNode := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "gv-gpu", Labels: map[string]string{
			"runtime": "gvisor", schema.GFDProductLabel: "Tesla-T4", schema.GFDDriverVersionLabel: "535.183.06",
		}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Capacity:   corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")},
		},
	}
	objs = append(objs, gpuNode)
	report, err := Scan(context.Background(), ScanOptions{KubeClient: fake.NewSimpleClientset(objs...), Namespaces: []string{"ns-gpu"}, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	w := report.Spec.Workloads[0]
	if w.Compatibility != schema.CompatibilityReview || !strings.Contains(w.Reasons[0].Description, "has not been checked against runsc") {
		t.Fatalf("gpu-app = %s / %q", w.Compatibility, w.Reasons[0].Description)
	}
	if report.Metadata.ClusterFacts.GPU == nil || report.Metadata.ClusterFacts.GPU.Nodes != 1 {
		t.Fatalf("GPU facts = %+v", report.Metadata.ClusterFacts.GPU)
	}
}

func TestScan_BadFactsPathIsAnError(t *testing.T) {
	t.Parallel()
	_, err := Scan(context.Background(), ScanOptions{
		KubeClient: fake.NewSimpleClientset(), Stderr: io.Discard, FactsPath: filepath.Join(t.TempDir(), "missing.json"),
	})
	if err == nil || !strings.Contains(err.Error(), "loading --facts") {
		t.Fatalf("err = %v", err)
	}
}

func TestAssessWorkload_UsesFacts(t *testing.T) {
	t.Parallel()
	pf := schema.NewPreflightReport()
	pf.Spec.Facts = *t4Facts(true)
	res, err := AssessWorkload(context.Background(), AssessWorkloadOptions{
		KubeClient: fake.NewSimpleClientset([]runtime.Object{gpuPod()}...), Kind: "Pod", Namespace: "ns-gpu", Name: "gpu-app",
		Stderr: io.Discard, FactsPath: writeJSON(t, "probe.json", pf),
	})
	if err != nil {
		t.Fatalf("AssessWorkload: %v", err)
	}
	if res.Compatibility != schema.CompatibilityCompatible || res.Reasons[0].Severity != schema.SeverityInfo {
		t.Fatalf("result = %+v", res)
	}
}

func TestPlan_FactsPathOverridesStoredScanFacts(t *testing.T) {
	t.Parallel()
	// Stored scan says the cluster is fine; the facts file says the
	// RuntimeClass is gone. The plan must warn from the file.
	sr := schema.NewScanReport()
	sr.Metadata.ClusterFacts = t4Facts(false)
	pf := schema.NewPreflightReport()
	pf.Spec.Facts = schema.ClusterFacts{RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor"}, Nodes: schema.NodeFacts{Total: 1, Ready: 1}}
	plan, err := Plan(context.Background(), PlanOptions{
		ScanReport:  sr,
		ScanOptions: ScanOptions{FactsPath: writeJSON(t, "pf.json", pf)},
		Planner:     schema.PlannerOptions{RuntimeClassName: "gvisor"},
		Stderr:      io.Discard,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Spec.Warnings) != 1 || plan.Spec.Warnings[0].ID != preflight.FindingRuntimeClassMissing {
		t.Fatalf("warnings = %+v", plan.Spec.Warnings)
	}
	if sr.Metadata.ClusterFacts.RuntimeClass.Found != true {
		t.Fatal("Plan must not mutate the caller's ScanReport")
	}
}
