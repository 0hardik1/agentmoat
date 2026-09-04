// Tests for the Preflight orchestrator and the plan warnings derived from
// stored cluster facts.
package agentmoat

import (
	"context"
	"io"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
)

func TestPreflight_NotReadyIsAReport(t *testing.T) {
	t.Parallel()
	report, err := Preflight(context.Background(), PreflightOptions{
		KubeClient: fake.NewSimpleClientset(applyWorkload()...), Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if report.Kind != schema.KindPreflightReport || report.Metadata.RuntimeClassName != "gvisor" {
		t.Fatalf("envelope = %s / %s", report.Kind, report.Metadata.RuntimeClassName)
	}
	if report.Metadata.AgentmoatVersion != Version || report.Metadata.GeneratedAt == "" {
		t.Fatalf("metadata = %+v", report.Metadata)
	}
	if report.Spec.Summary.Ready || report.Spec.Findings[0].ID != preflight.FindingRuntimeClassMissing {
		t.Fatalf("summary=%+v findings=%+v", report.Spec.Summary, report.Spec.Findings)
	}
}

func TestPreflight_ReadyWithCustomName(t *testing.T) {
	t.Parallel()
	objs := gvisorReady()
	report, err := Preflight(context.Background(), PreflightOptions{
		KubeClient: fake.NewSimpleClientset(objs...), RuntimeClassName: "gvisor", Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if !report.Spec.Summary.Ready {
		t.Fatalf("summary=%+v findings=%+v", report.Spec.Summary, report.Spec.Findings)
	}
}

func TestPlanWarnings(t *testing.T) {
	t.Parallel()
	readyFacts := func() *schema.ClusterFacts {
		return &schema.ClusterFacts{
			RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor", Found: true, NodeSelector: map[string]string{"runtime": "gvisor"},
				Overhead: map[string]string{"memory": "140Mi"}},
			Nodes: schema.NodeFacts{Total: 2, Ready: 2, MatchingSelector: 1, MatchingAndReady: 1},
		}
	}
	tests := []struct {
		name    string
		facts   *schema.ClusterFacts
		rcName  string
		wantIDs string
	}{
		{"nil facts", nil, "gvisor", ""},
		{"ready cluster", readyFacts(), "gvisor", ""},
		{"default name matches", readyFacts(), "", ""},
		{
			"mismatched runtimeclass name",
			readyFacts(), "sandbox",
			WarningClusterFactsRuntimeClassMismatch,
		},
		{
			"error finding becomes a warning",
			func() *schema.ClusterFacts {
				f := readyFacts()
				f.Nodes.MatchingSelector, f.Nodes.MatchingAndReady = 0, 0
				return f
			}(),
			"gvisor",
			preflight.FindingRuntimeClassNoMatchingNodes,
		},
		{
			"info findings are dropped",
			func() *schema.ClusterFacts { f := readyFacts(); f.RuntimeClass.Overhead = nil; return f }(),
			"gvisor",
			"",
		},
		{
			"warn and error both kept, errors first",
			func() *schema.ClusterFacts {
				f := readyFacts()
				f.Nodes.MatchingSelector, f.Nodes.MatchingAndReady = 2, 2
				f.Nodes.MatchingTaintedWithoutToleration = 1
				f.Platform.EKSAutoModeMatchingNodes = 2
				return f
			}(),
			"gvisor",
			preflight.FindingEKSAutoModeNodes + "," + preflight.FindingRuntimeClassTaintWithoutToleration,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := planWarnings(tc.facts, tc.rcName)
			ids := make([]string, 0, len(got))
			for _, w := range got {
				ids = append(ids, w.ID)
				if w.Message == "" {
					t.Errorf("warning %s has an empty message", w.ID)
				}
			}
			if strings.Join(ids, ",") != tc.wantIDs {
				t.Fatalf("warnings = %q, want %q", strings.Join(ids, ","), tc.wantIDs)
			}
		})
	}
}

// TestPlan_WarningsFromStoredScan: a plan built from a ScanReport that
// carries facts gets the warnings, and the hash is unaffected by them.
func TestPlan_WarningsFromStoredScan(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Workloads = []schema.WorkloadResult{{
		Kind: "Deployment", Namespace: "ns-a", Name: "web", Compatibility: schema.CompatibilityCompatible,
	}}
	report.Spec.Summary = schema.Summary{Total: 1, Compatible: 1}

	plain, err := Plan(context.Background(), PlanOptions{ScanReport: report, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plain.Spec.Warnings) != 0 {
		t.Fatalf("warnings without facts = %+v", plain.Spec.Warnings)
	}

	report.Metadata.ClusterFacts = &schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor", Found: false},
		Nodes:        schema.NodeFacts{Total: 3, Ready: 3},
	}
	warned, err := Plan(context.Background(), PlanOptions{ScanReport: report, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(warned.Spec.Warnings) != 1 || warned.Spec.Warnings[0].ID != preflight.FindingRuntimeClassMissing {
		t.Fatalf("warnings = %+v", warned.Spec.Warnings)
	}
	if warned.Metadata.PlanHash != plain.Metadata.PlanHash {
		t.Fatalf("plan hash changed with warnings: %s vs %s", warned.Metadata.PlanHash, plain.Metadata.PlanHash)
	}
	if len(warned.Spec.Steps) != 1 || warned.Spec.Steps[0].AddToleration {
		t.Fatalf("steps = %+v", warned.Spec.Steps)
	}
}

// TestScan_ClusterFactsRecordedAndSkippable: the scan attaches facts by
// default and omits them on request.
func TestScan_ClusterFactsRecordedAndSkippable(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(append(applyWorkload(), gvisorReady()...)...)

	withFacts, err := Scan(context.Background(), ScanOptions{KubeClient: client, AllNamespaces: true, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	f := withFacts.Metadata.ClusterFacts
	if f == nil || !f.RuntimeClass.Found || f.Nodes.MatchingAndReady != 1 {
		t.Fatalf("ClusterFacts = %+v", f)
	}

	without, err := Scan(context.Background(), ScanOptions{KubeClient: client, AllNamespaces: true, SkipClusterFacts: true, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if without.Metadata.ClusterFacts != nil {
		t.Fatalf("ClusterFacts recorded despite SkipClusterFacts")
	}
}
