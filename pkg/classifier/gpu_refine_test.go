// Tests for the gpu-passthrough refinement (gpu_refine.go) through the
// public ClassifyWithFacts entry point, so they also pin the plumbing:
// severity replaced, note appended, verdict aggregated.
package classifier

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

var probed = &schema.NvproxyFacts{RunscVersion: "release-20260817.0", SupportedDrivers: []string{"535.183.06", "550.54.15"}}

// facts builds ClusterFacts with the given GPU groups, matching nodes
// counted, and support verdicts refreshed.
func facts(nv *schema.NvproxyFacts, groups ...schema.GPUNodeGroup) *schema.ClusterFacts {
	f := &schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor", Found: true, NodeSelector: map[string]string{"runtime": "gvisor"}},
		GPU:          &schema.GPUFacts{Nvproxy: nv, Groups: groups},
	}
	for _, g := range groups {
		f.GPU.Nodes += g.Nodes
		f.GPU.MatchingNodes += g.MatchingNodes
	}
	preflight.RefreshGPUSupport(f)
	return f
}

func group(product, driver string, nodes, matching int) schema.GPUNodeGroup {
	return schema.GPUNodeGroup{Product: product, Driver: driver, Nodes: nodes, MatchingNodes: matching}
}

func gpuReason(t *testing.T, v Verdict) Reason {
	t.Helper()
	for _, r := range v.Reasons {
		if r.RuleID == "gpu-passthrough" {
			return r
		}
	}
	t.Fatalf("gpu-passthrough did not fire: %+v", v.Reasons)
	return Reason{}
}

func TestClassifyWithFacts_GPUPassthrough(t *testing.T) {
	t4 := group("Tesla-T4", "535.183.06", 2, 2)
	cases := []struct {
		name       string
		facts      *schema.ClusterFacts
		wantCompat Compatibility
		wantSev    Severity
		wantNote   string // "" means the description must be the bare rule text
	}{
		{name: "no facts: unchanged", facts: nil, wantCompat: Review, wantSev: SeverityWarn},
		{name: "facts without GPU nodes: unchanged", facts: &schema.ClusterFacts{}, wantCompat: Review, wantSev: SeverityWarn},
		{name: "supported card and listed driver: info, compatible",
			facts: facts(probed, t4), wantCompat: Compatible, wantSev: SeverityInfo,
			wantNote: "runsc release-20260817.0 nvproxy supports the card and host driver on every GPU node(s) matching the RuntimeClass (Tesla-T4 driver 535.183.06 on 2 node(s), 2 matching the RuntimeClass)."},
		{name: "supported card, driver not probed: unchanged with advice",
			facts: facts(nil, t4), wantCompat: Review, wantSev: SeverityWarn,
			wantNote: "has not been checked against runsc; run 'agentmoat probe nvproxy --dry-run=false'"},
		{name: "supported card, driver not listed: error",
			facts: facts(probed, group("Tesla-T4", "560.35.03", 1, 1)), wantCompat: Incompatible, wantSev: SeverityError,
			wantNote: "does not list the host driver"},
		{name: "unsupported card only: error",
			facts: facts(probed, group("Tesla-V100-SXM2-16GB", "535.183.06", 4, 4)), wantCompat: Incompatible, wantSev: SeverityError,
			wantNote: "no GPU node(s) matching the RuntimeClass has a card gVisor nvproxy supports"},
		{name: "MIG-sliced supported card only: error",
			facts:      facts(probed, schema.GPUNodeGroup{Product: "NVIDIA-A100-SXM4-40GB", Driver: "535.183.06", MIG: true, Nodes: 1, MatchingNodes: 1}),
			wantCompat: Incompatible, wantSev: SeverityError, wantNote: "(MIG)"},
		{name: "no GFD labels: unchanged with advice",
			facts: facts(probed, group("", "", 2, 2)), wantCompat: Review, wantSev: SeverityWarn,
			wantNote: "carry no GPU Feature Discovery labels"},
		{name: "mixed pool: warn with pin advice",
			facts: facts(probed, t4, group("NVIDIA-L40S", "535.183.06", 1, 1)), wantCompat: Review, wantSev: SeverityWarn,
			wantNote: "pin the workload with a nodeSelector on nvidia.com/gpu.product"},
		{name: "unsupported cards outside the RuntimeClass pool are ignored",
			facts: facts(probed, t4, group("Tesla-V100-SXM2-16GB", "535.183.06", 8, 0)), wantCompat: Compatible, wantSev: SeverityInfo,
			wantNote: "on every GPU node(s) matching the RuntimeClass (Tesla-T4 driver 535.183.06 on 2 node(s), 2 matching the RuntimeClass)."},
		{name: "no selector match anywhere: every GPU node counts",
			facts: facts(probed, group("Tesla-T4", "535.183.06", 2, 0)), wantCompat: Compatible, wantSev: SeverityInfo,
			wantNote: "on every GPU node(s) (Tesla-T4"},
	}
	reg := NewRegistry()
	RegisterBuiltins(reg)
	w := newWorkload(workloadOpts{gpuLimit: true})
	bare := Classify(w, reg)
	bareDesc := gpuReason(t, bare).Description

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyWithFacts(w, reg, tc.facts)
			if v.Compatibility != tc.wantCompat {
				t.Fatalf("compat = %s, want %s (reasons %+v)", v.Compatibility, tc.wantCompat, v.Reasons)
			}
			r := gpuReason(t, v)
			if r.Severity != tc.wantSev {
				t.Fatalf("severity = %s, want %s", r.Severity, tc.wantSev)
			}
			if tc.wantNote == "" {
				if r.Description != bareDesc {
					t.Fatalf("description changed without facts: %q", r.Description)
				}
				return
			}
			if !strings.HasPrefix(r.Description, bareDesc+" Cluster facts: ") || !strings.Contains(r.Description, tc.wantNote) {
				t.Fatalf("description = %q\nwant prefix %q and %q", r.Description, bareDesc+" Cluster facts: ", tc.wantNote)
			}
		})
	}
}

func TestClassifyWithFacts_MIGRequestIsIncompatibleWithoutFacts(t *testing.T) {
	reg := NewRegistry()
	RegisterBuiltins(reg)
	w := newWorkload(workloadOpts{})
	w.PodSpec.Containers[0].Resources = corev1.ResourceRequirements{
		Limits: corev1.ResourceList{"nvidia.com/mig-3g.20gb": resource.MustParse("1"), "nvidia.com/mig-1g.5gb": resource.MustParse("2")},
	}
	v := Classify(w, reg)
	if v.Compatibility != Incompatible {
		t.Fatalf("compat = %s, want incompatible", v.Compatibility)
	}
	r := gpuReason(t, v)
	if r.Severity != SeverityError || !strings.Contains(r.Description, "MIG slice (nvidia.com/mig-1g.5gb)") {
		t.Fatalf("reason = %+v", r)
	}
	// Facts cannot rescue a MIG request: the workload itself asks for a slice.
	if v2 := ClassifyWithFacts(w, reg, facts(probed, group("Tesla-T4", "535.183.06", 2, 2))); v2.Compatibility != Incompatible {
		t.Fatalf("MIG request with good facts = %s, want incompatible", v2.Compatibility)
	}
}

func TestClassifyWithFacts_SharedGPUResourceMatches(t *testing.T) {
	reg := NewRegistry()
	RegisterBuiltins(reg)
	w := newWorkload(workloadOpts{})
	w.PodSpec.Containers[0].Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{"nvidia.com/gpu.shared": resource.MustParse("1")},
	}
	if v := Classify(w, reg); v.Compatibility != Review {
		t.Fatalf("time-sliced GPU request = %s, want review", v.Compatibility)
	}
}

func TestClassifyWithFacts_RespectsOverriddenBaseSeverity(t *testing.T) {
	reg := NewRegistry()
	RegisterBuiltins(reg)
	if err := reg.Override("gpu-passthrough", SeverityInfo); err != nil {
		t.Fatal(err)
	}
	w := newWorkload(workloadOpts{gpuLimit: true})
	// Unconfirmed driver keeps the operator's base (info), so the workload
	// stays compatible; unsupported hardware still raises to error.
	if v := ClassifyWithFacts(w, reg, facts(nil, group("Tesla-T4", "535.183.06", 1, 1))); v.Compatibility != Compatible {
		t.Fatalf("unconfirmed with info override = %s, want compatible", v.Compatibility)
	}
	if v := ClassifyWithFacts(w, reg, facts(probed, group("NVIDIA-L40S", "535.183.06", 1, 1))); v.Compatibility != Incompatible {
		t.Fatalf("unsupported card with info override = %s, want incompatible", v.Compatibility)
	}
}
