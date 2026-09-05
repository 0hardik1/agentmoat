// Tests for the GPU facts and findings (gpu.go, gpuFindings in
// evaluate.go). Table-driven; no client needed except for the Collect
// wiring test, which uses the fake clientset.
package preflight

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes/fake"
)

// gpuNode builds a Ready node with the given labels and one nvidia.com/gpu
// in capacity, matching the gvisorRC() selector when gvisor is true.
func gpuNode(name string, gvisor bool, labels map[string]string) *corev1.Node {
	n := node(name, withLabels())
	n.Status.Capacity = corev1.ResourceList{corev1.ResourceName(schema.NvidiaGPUResource): resource.MustParse("1")}
	if gvisor {
		n.Labels["runtime"] = "gvisor"
	}
	for k, v := range labels {
		n.Labels[k] = v
	}
	return n
}

func gfd(product, driver string) map[string]string {
	return map[string]string{schema.GFDProductLabel: product, schema.GFDDriverVersionLabel: driver}
}

func TestProductSupport(t *testing.T) {
	cases := map[string]schema.SupportStatus{
		"":                        schema.SupportUnknown,
		"Tesla-T4":                schema.SupportSupported,
		"NVIDIA-A100-SXM4-40GB":   schema.SupportSupported,
		"NVIDIA-A10G":             schema.SupportSupported,
		"NVIDIA-L4":               schema.SupportSupported,
		"NVIDIA-H100-80GB-HBM3":   schema.SupportSupported,
		"NVIDIA_H100_PCIe":        schema.SupportSupported,
		"NVIDIA-A10":              schema.SupportUnsupported,
		"NVIDIA-L40S":             schema.SupportUnsupported,
		"Tesla-V100-SXM2-16GB":    schema.SupportUnsupported,
		"NVIDIA-GeForce-RTX-4090": schema.SupportUnsupported,
	}
	for product, want := range cases {
		if got := ProductSupport(product); got != want {
			t.Errorf("ProductSupport(%q) = %s, want %s", product, got, want)
		}
	}
}

func TestDriverSupport(t *testing.T) {
	nv := &schema.NvproxyFacts{SupportedDrivers: []string{"535.183.06", "550.54.15"}}
	cases := []struct {
		driver string
		nv     *schema.NvproxyFacts
		want   schema.SupportStatus
	}{
		{"", nv, schema.SupportUnknown},
		{"535.183.06", nil, schema.SupportUnknown},
		{"535.183.06", nv, schema.SupportSupported},
		{"535.183", nv, schema.SupportUnsupported},
		{"560.35.03", nv, schema.SupportUnsupported},
	}
	for _, c := range cases {
		if got := DriverSupport(c.driver, c.nv); got != c.want {
			t.Errorf("DriverSupport(%q, probed=%v) = %s, want %s", c.driver, c.nv != nil, got, c.want)
		}
	}
}

func TestCollect_GPUFacts(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		gvisorRC(),
		node("cpu-1"),
		gpuNode("t4-a", true, gfd("Tesla-T4", "535.183.06")),
		gpuNode("t4-b", true, gfd("Tesla-T4", "535.183.06")),
		gpuNode("v100", false, gfd("Tesla-V100-SXM2-16GB", "535.183.06")),
		gpuNode("legacy", true, map[string]string{
			schema.GFDProductLabel: "NVIDIA-A10G", schema.GFDDriverMajorLabel: "550",
			schema.GFDDriverMinorLabel: "54", schema.GFDDriverRevLabel: "15",
		}),
		gpuNode("mig", true, map[string]string{
			schema.GFDProductLabel: "NVIDIA-A100-SXM4-40GB", schema.GFDDriverVersionLabel: "535.183.06",
			schema.GFDMIGStrategyLabel: "single",
		}),
		gpuNode("bare", true, nil),
	)
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := &schema.GPUFacts{
		Nodes: 6, MatchingNodes: 5, MIGNodes: 1,
		Groups: []schema.GPUNodeGroup{
			{Nodes: 1, MatchingNodes: 1, ProductSupport: schema.SupportUnknown, DriverSupport: schema.SupportUnknown},
			{Product: "NVIDIA-A100-SXM4-40GB", Driver: "535.183.06", MIG: true, Nodes: 1, MatchingNodes: 1,
				ProductSupport: schema.SupportSupported, DriverSupport: schema.SupportUnknown},
			{Product: "NVIDIA-A10G", Driver: "550.54.15", Nodes: 1, MatchingNodes: 1,
				ProductSupport: schema.SupportSupported, DriverSupport: schema.SupportUnknown},
			{Product: "Tesla-T4", Driver: "535.183.06", Nodes: 2, MatchingNodes: 2,
				ProductSupport: schema.SupportSupported, DriverSupport: schema.SupportUnknown},
			{Product: "Tesla-V100-SXM2-16GB", Driver: "535.183.06", Nodes: 1,
				ProductSupport: schema.SupportUnsupported, DriverSupport: schema.SupportUnknown},
		},
	}
	if !reflect.DeepEqual(facts.GPU, want) {
		t.Fatalf("GPU facts:\n got %+v\nwant %+v", facts.GPU, want)
	}

	// The probe fills Nvproxy and refreshes: driver verdicts flip.
	facts.GPU.Nvproxy = &schema.NvproxyFacts{RunscVersion: "release-20260817.0", SupportedDrivers: []string{"535.183.06"}}
	RefreshGPUSupport(facts)
	got := map[string]schema.SupportStatus{}
	for _, g := range facts.GPU.Groups {
		got[g.Product] = g.DriverSupport
	}
	wantDrivers := map[string]schema.SupportStatus{
		"": schema.SupportUnknown, "NVIDIA-A100-SXM4-40GB": schema.SupportSupported,
		"NVIDIA-A10G": schema.SupportUnsupported, "Tesla-T4": schema.SupportSupported,
		"Tesla-V100-SXM2-16GB": schema.SupportSupported,
	}
	if !reflect.DeepEqual(got, wantDrivers) {
		t.Fatalf("driver verdicts after refresh = %v, want %v", got, wantDrivers)
	}
}

func TestCollect_NoGPUKeepsFactsAbsent(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorRC(), node("cpu-1"))
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.GPU != nil {
		t.Fatalf("GPU = %+v, want nil on a CPU-only cluster", facts.GPU)
	}
}

func TestHasGPU(t *testing.T) {
	plain := node("n")
	if HasGPU(plain) {
		t.Fatal("plain node reported as GPU")
	}
	labeled := node("n", withLabels(schema.GFDProductLabel, "Tesla-T4"))
	if !HasGPU(labeled) {
		t.Fatal("GFD-labeled node not reported as GPU")
	}
	mig := node("n")
	mig.Status.Capacity = corev1.ResourceList{"nvidia.com/mig-1g.5gb": resource.MustParse("7")}
	if !HasGPU(mig) {
		t.Fatal("MIG capacity node not reported as GPU")
	}
}

// gpuFacts builds a ready cluster fact set with the given GPU groups.
func gpuFacts(nv *schema.NvproxyFacts, groups ...schema.GPUNodeGroup) *schema.ClusterFacts {
	f := &schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor", Found: true, Handler: "gvisor",
			NodeSelector: map[string]string{"runtime": "gvisor"}, Overhead: map[string]string{"cpu": "250m"}},
		Nodes: schema.NodeFacts{Total: 3, Ready: 3, MatchingSelector: 2, MatchingAndReady: 2, MatchingNames: []string{"a", "b"}},
		GPU:   &schema.GPUFacts{Nvproxy: nv, Groups: groups},
	}
	for _, g := range groups {
		f.GPU.Nodes += g.Nodes
		f.GPU.MatchingNodes += g.MatchingNodes
		if g.MIG {
			f.GPU.MIGNodes += g.Nodes
		}
	}
	RefreshGPUSupport(f)
	return f
}

func TestEvaluate_GPUFindings(t *testing.T) {
	probed := &schema.NvproxyFacts{RunscVersion: "release-20260817.0", SupportedDrivers: []string{"535.183.06", "550.54.15"}}
	t4 := schema.GPUNodeGroup{Product: "Tesla-T4", Driver: "535.183.06", Nodes: 2, MatchingNodes: 2}
	cases := []struct {
		name    string
		facts   *schema.ClusterFacts
		wantIDs []string
		wantSev map[string]schema.Severity
		wantMsg string
	}{
		{
			name:    "no gpu nodes: silence",
			facts:   gpuFacts(nil),
			wantIDs: []string{},
		},
		{
			name:    "supported card, not probed: unconfirmed info",
			facts:   gpuFacts(nil, t4),
			wantIDs: []string{FindingGPUDriverUnconfirmed},
			wantSev: map[string]schema.Severity{FindingGPUDriverUnconfirmed: schema.SeverityInfo},
			wantMsg: "Tesla-T4 driver 535.183.06 on 2 node(s), 2 matching the RuntimeClass",
		},
		{
			name:    "supported card and driver: ready info",
			facts:   gpuFacts(probed, t4),
			wantIDs: []string{FindingGPUNvproxyReady},
			wantMsg: "runsc release-20260817.0 nvproxy supports",
		},
		{
			name:    "supported card, unsupported driver: warn",
			facts:   gpuFacts(probed, schema.GPUNodeGroup{Product: "Tesla-T4", Driver: "560.35.03", Nodes: 1, MatchingNodes: 1}),
			wantIDs: []string{FindingGPUDriverUnsupported},
			wantSev: map[string]schema.Severity{FindingGPUDriverUnsupported: schema.SeverityWarn},
			wantMsg: "supported drivers: 535.183.06, 550.54.15",
		},
		{
			name:    "unsupported card: warn",
			facts:   gpuFacts(probed, schema.GPUNodeGroup{Product: "Tesla-V100-SXM2-16GB", Driver: "535.183.06", Nodes: 1, MatchingNodes: 1}),
			wantIDs: []string{FindingGPUProductUnsupported},
			wantMsg: "supported: T4, A100, A10G, L4, H100",
		},
		{
			name:    "no GFD labels: unknown info",
			facts:   gpuFacts(probed, schema.GPUNodeGroup{Nodes: 1, MatchingNodes: 1}),
			wantIDs: []string{FindingGPUProductUnknown},
			wantMsg: "unlabeled card driver unknown on 1 node(s)",
		},
		{
			name:    "MIG wins over card support",
			facts:   gpuFacts(probed, schema.GPUNodeGroup{Product: "NVIDIA-A100-SXM4-40GB", Driver: "535.183.06", MIG: true, Nodes: 1, MatchingNodes: 1}),
			wantIDs: []string{FindingGPUMIGEnabled},
			wantMsg: "(MIG)",
		},
		{
			name: "mixed pool: one finding per bucket, warn before info",
			facts: gpuFacts(probed, t4,
				schema.GPUNodeGroup{Product: "Tesla-T4", Driver: "560.35.03", Nodes: 1, MatchingNodes: 1},
				schema.GPUNodeGroup{Product: "NVIDIA-L40S", Driver: "535.183.06", Nodes: 3},
			),
			wantIDs: []string{FindingGPUDriverUnsupported, FindingGPUProductUnsupported, FindingGPUNvproxyReady},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate(tc.facts)
			ids := []string{}
			for _, f := range got {
				ids = append(ids, f.ID)
				if f.Severity == schema.SeverityError {
					t.Errorf("GPU finding %s is error severity; GPU findings must never block", f.ID)
				}
				if want, ok := tc.wantSev[f.ID]; ok && f.Severity != want {
					t.Errorf("%s severity = %s, want %s", f.ID, f.Severity, want)
				}
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("finding ids = %v, want %v", ids, tc.wantIDs)
			}
			if tc.wantMsg != "" {
				all := ""
				for _, f := range got {
					all += f.Message + "\n"
				}
				if !strings.Contains(all, tc.wantMsg) {
					t.Errorf("messages missing %q:\n%s", tc.wantMsg, all)
				}
			}
		})
	}
}

func TestDescribeGPUGroup(t *testing.T) {
	g := schema.GPUNodeGroup{Product: "NVIDIA-A10G", Driver: "550.54.15", Nodes: 4, MatchingNodes: 0}
	if got := DescribeGPUGroup(g); got != "NVIDIA-A10G driver 550.54.15 on 4 node(s)" {
		t.Fatalf("DescribeGPUGroup = %q", got)
	}
	g = schema.GPUNodeGroup{MIG: true, Nodes: 12, MatchingNodes: 12}
	if got := DescribeGPUGroup(g); got != "unlabeled card (MIG) driver unknown on 12 node(s), 12 matching the RuntimeClass" {
		t.Fatalf("DescribeGPUGroup = %q", got)
	}
}
