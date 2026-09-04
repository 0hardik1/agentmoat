// Tests for the preflight / cluster-facts wire types (preflight.go) and the
// additive fields they hang off existing envelopes. The load-bearing
// assertions are the omitempty ones: a document produced before this
// schema addition must serialize byte-for-byte the same today.
package schema

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestKindPreflightReportConstant(t *testing.T) {
	if KindPreflightReport != "PreflightReport" {
		t.Fatalf("KindPreflightReport = %q", KindPreflightReport)
	}
}

func TestNewPreflightReportEnvelope(t *testing.T) {
	r := NewPreflightReport()
	if r.APIVersion != APIVersion || r.Kind != KindPreflightReport {
		t.Fatalf("envelope = %s/%s", r.APIVersion, r.Kind)
	}
}

func TestPlatformConstants(t *testing.T) {
	if EKSAutoModeLabel != "eks.amazonaws.com/compute-type" || EKSAutoModeLabelValue != "auto" {
		t.Fatalf("EKS Auto Mode label = %s=%s", EKSAutoModeLabel, EKSAutoModeLabelValue)
	}
	if KarpenterNodePoolLabel != "karpenter.sh/nodepool" || BottlerocketOSImagePrefix != "Bottlerocket OS" {
		t.Fatalf("platform constants drifted")
	}
}

func TestPreflightReportRoundTrip(t *testing.T) {
	in := NewPreflightReport()
	in.Metadata = PreflightMetadata{GeneratedAt: "2026-09-04T00:00:00Z", Cluster: "c", AgentmoatVersion: "v", RuntimeClassName: "gvisor"}
	in.Spec = PreflightSpec{
		Summary: PreflightSummary{Ready: false, Total: 2, Error: 1, Warn: 1},
		Facts: ClusterFacts{
			RuntimeClass: RuntimeClassFacts{Name: "gvisor", Found: true, Handler: "gvisor",
				NodeSelector: map[string]string{"runtime": "gvisor"}, Tolerations: 1, Overhead: map[string]string{"cpu": "250m"}},
			Nodes:    NodeFacts{Total: 3, Ready: 3, MatchingSelector: 1, MatchingAndReady: 1, MatchingNames: []string{"gv"}},
			Platform: PlatformFacts{EKSAutoModeNodes: 2, KarpenterNodes: 2},
		},
		Findings: []PreflightFinding{
			{ID: "eks-auto-mode-nodes", Severity: SeverityError, Message: "m", Remediation: "r"},
			{ID: "runtimeclass-taint-without-toleration", Severity: SeverityWarn, Message: "m2"},
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out PreflightReport
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*in, out) {
		t.Fatalf("round trip mismatch:\n in: %+v\nout: %+v", *in, out)
	}
	for _, key := range []string{`"clusterFacts"`, `"ready":false`, `"matchingTaintedWithoutToleration":0`, `"remediation":"r"`} {
		if key == `"clusterFacts"` {
			continue // not a PreflightReport key; see the ScanReport test below
		}
		if !strings.Contains(string(data), key) {
			t.Errorf("json missing %s:\n%s", key, data)
		}
	}
}

// TestAdditiveFieldsOmitEmpty pins that every field this change added to
// an existing envelope is omitted when unset, so pre-existing documents
// and their consumers see no difference.
func TestAdditiveFieldsOmitEmpty(t *testing.T) {
	cases := []struct {
		name   string
		doc    any
		absent []string
	}{
		{"ScanReport without facts", NewScanReport(), []string{"clusterFacts"}},
		{"MigrationPlan without warnings", NewMigrationPlan(), []string{"warnings", "addToleration\":true"}},
		{"PlannerOptions zero", PlannerOptions{}, []string{"addToleration"}},
		{"ApplyResult without preflight", NewApplyResult(), []string{"preflight", "preflightFindings"}},
		{"VerifyResult without placement", VerifyResult{}, []string{"nodePlacement"}},
	}
	for _, tc := range cases {
		data, err := json.Marshal(tc.doc)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		for _, key := range tc.absent {
			if strings.Contains(string(data), key) {
				t.Errorf("%s: json unexpectedly contains %q:\n%s", tc.name, key, data)
			}
		}
	}
}

// TestPlanStepAddTolerationAlwaysSerialized: PlanStep.AddToleration has no
// omitempty on purpose. The plan hash covers the steps, so flipping the
// serialization of a false value would change every existing hash.
func TestPlanStepAddTolerationAlwaysSerialized(t *testing.T) {
	data, err := json.Marshal(PlanStep{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"addToleration":false`) {
		t.Fatalf("PlanStep must always serialize addToleration: %s", data)
	}
}

func TestAdditiveFieldsPresentWhenSet(t *testing.T) {
	report := NewScanReport()
	report.Metadata.ClusterFacts = &ClusterFacts{RuntimeClass: RuntimeClassFacts{Name: "gvisor"}}
	plan := NewMigrationPlan()
	plan.Spec.Warnings = []PlanWarning{{ID: "x", Message: "y"}}
	plan.Spec.Options.AddToleration = true
	res := NewApplyResult()
	res.Metadata.Preflight = &PreflightSummary{Ready: true}
	res.Spec.PreflightFindings = []PreflightFinding{{ID: "runtimeclass-no-overhead", Severity: SeverityInfo, Message: "m"}}
	vr := VerifyResult{NodePlacement: &NodePlacement{Checked: true, Nodes: []string{"a"}, Mismatched: []string{"a"}, Message: "m"}}

	cases := []struct {
		name string
		doc  any
		want []string
	}{
		{"ScanReport facts", report, []string{`"clusterFacts":{`, `"name":"gvisor"`}},
		{"MigrationPlan warnings", plan, []string{`"warnings":[{"id":"x","message":"y"}]`, `"addToleration":true`}},
		{"ApplyResult preflight", res, []string{`"preflight":{"ready":true`, `"preflightFindings":[{"id":"runtimeclass-no-overhead"`}},
		{"VerifyResult placement", vr, []string{`"nodePlacement":{"checked":true,"nodes":["a"],"mismatched":["a"],"message":"m"}`}},
	}
	for _, tc := range cases {
		data, err := json.Marshal(tc.doc)
		if err != nil {
			t.Fatalf("%s: marshal: %v", tc.name, err)
		}
		for _, key := range tc.want {
			if !strings.Contains(string(data), key) {
				t.Errorf("%s: json missing %s:\n%s", tc.name, key, data)
			}
		}
	}
}

func TestGPUFactsRoundTripAndOmitEmpty(t *testing.T) {
	// A CPU-only cluster serializes exactly as before the GPU addition.
	plain, _ := json.Marshal(ClusterFacts{})
	if strings.Contains(string(plain), `"gpu"`) {
		t.Fatalf("ClusterFacts without GPU nodes must omit gpu: %s", plain)
	}
	meta, _ := json.Marshal(PreflightMetadata{})
	if strings.Contains(string(meta), `"probe"`) {
		t.Fatalf("PreflightMetadata without a probe must omit probe: %s", meta)
	}

	in := ClusterFacts{GPU: &GPUFacts{
		Nodes: 3, MatchingNodes: 2, MIGNodes: 1,
		Groups: []GPUNodeGroup{
			{Product: "Tesla-T4", Driver: "535.183.06", Nodes: 2, MatchingNodes: 2,
				ProductSupport: SupportSupported, DriverSupport: SupportSupported},
			{MIG: true, Nodes: 1, ProductSupport: SupportUnknown, DriverSupport: SupportUnknown},
		},
		Nvproxy: &NvproxyFacts{RunscVersion: "release-20260817.0", SupportedDrivers: []string{"535.183.06"},
			Node: "gv-1", ProbedAt: "2026-09-04T00:00:00Z"},
	}}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"gpu":`, `"groups":`, `"productSupport":"supported"`, `"nvproxy":`, `"supportedDrivers":`, `"mig":true`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("serialized GPU facts missing %s: %s", key, data)
		}
	}
	var out ClusterFacts
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch:\n in %+v\nout %+v", in, out)
	}
}

func TestProbeMetadataRoundTrip(t *testing.T) {
	in := PreflightMetadata{RuntimeClassName: "gvisor", Probe: &ProbeMetadata{
		DryRun: true, Namespace: "default", PodName: "agentmoat-nvproxy-probe", Node: "gv-1",
		Image: "busybox:1.36.1", RunscPath: "/usr/local/bin/runsc",
	}}
	data, _ := json.Marshal(in)
	var out PreflightMetadata
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Fatalf("round trip mismatch: %+v vs %+v", in, out)
	}
	if !strings.Contains(string(data), `"succeeded":false`) {
		t.Fatalf("succeeded must always serialize so a dry run reads as not-run: %s", data)
	}
}

func TestNvidiaConstants(t *testing.T) {
	if GFDProductLabel != "nvidia.com/gpu.product" || GFDDriverVersionLabel != "nvidia.com/cuda.driver-version.full" ||
		GFDMIGStrategyLabel != "nvidia.com/mig.strategy" || NvidiaGPUResource != "nvidia.com/gpu" ||
		NvidiaMIGResourcePrefix != "nvidia.com/mig-" {
		t.Fatalf("NVIDIA label constants drifted from the GFD/device-plugin names")
	}
}
