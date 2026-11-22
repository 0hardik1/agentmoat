// Tests for the typed result of the scan gate: the outcome, the summary
// sentence, and the list of failing workloads. The exit-code side of the
// gate (Failed, Messages) is tested in scan_manifests_test.go.
//
// These fields have readers outside this package. The Markdown output shows
// the outcome and the summary where no stderr is visible, and the SARIF
// output sets levels from the failing list. So a wrong outcome here is a
// report that tells a reviewer the wrong thing.

package agentmoat

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func TestEvaluateGate_Outcome(t *testing.T) {
	t.Parallel()
	const (
		ok  = schema.CompatibilityCompatible
		rev = schema.CompatibilityReview
		bad = schema.CompatibilityIncompatible
	)
	ref := func(name string) schema.WorkloadRef {
		return schema.WorkloadRef{Kind: "Deployment", Namespace: "ns", Name: name}
	}
	tests := []struct {
		name        string
		report      *schema.ScanReport
		failOn      FailOn
		wantOutcome GateOutcome
		wantSummary string // must be in the summary
		wantFailing []schema.WorkloadRef
		wantDetails int
	}{
		{
			name:        "none: the gate is off, whatever the report holds",
			report:      gateReport("gvisor", gateRow("a", "gvisor", bad)),
			failOn:      FailOnNone,
			wantOutcome: GateOff,
			wantSummary: "does not pass or fail",
		},
		{
			name:        "incompatible: failed, and every incompatible workload is in the failing list",
			report:      gateReport("gvisor", gateRow("a", "", ok), gateRow("b", "", bad), gateRow("c", "gvisor", bad)),
			failOn:      FailOnIncompatible,
			wantOutcome: GateFailed,
			wantSummary: "2 of 3 workload(s) cannot run in gVisor",
			wantFailing: []schema.WorkloadRef{ref("b"), ref("c")},
		},
		{
			name:        "incompatible: review alone is a pass",
			report:      gateReport("gvisor", gateRow("a", "", rev), gateRow("b", "", ok)),
			failOn:      FailOnIncompatible,
			wantOutcome: GatePassed,
			wantSummary: "None of the 2 workload(s) is incompatible",
		},
		{
			name:        "incompatible: a scan with no workloads checked nothing, which is not a pass",
			report:      gateReport("gvisor"),
			failOn:      FailOnIncompatible,
			wantOutcome: GateNothingChecked,
			wantSummary: "no workloads",
		},
		{
			name:        "the empty setting is the default, incompatible",
			report:      gateReport("gvisor", gateRow("b", "", bad)),
			failOn:      "",
			wantOutcome: GateFailed,
			wantSummary: "1 of 1",
			wantFailing: []schema.WorkloadRef{ref("b")},
		},
		{
			name:        "sandboxed: failed; only the sandboxed workload is in the failing list",
			report:      gateReport("gvisor", gateRow("cni", "", bad), gateRow("gateway", "gvisor", bad)),
			failOn:      FailOnSandboxed,
			wantOutcome: GateFailed,
			wantSummary: `1 workload(s) ask for RuntimeClass "gvisor" and cannot run in it`,
			wantFailing: []schema.WorkloadRef{ref("gateway")},
			wantDetails: 1,
		},
		{
			name:        "sandboxed: passed, with an incompatible workload outside the sandbox",
			report:      gateReport("gvisor", gateRow("cni", "", bad), gateRow("agent", "gvisor", ok)),
			failOn:      FailOnSandboxed,
			wantOutcome: GatePassed,
			wantSummary: `1 workload(s) ask for RuntimeClass "gvisor" and none is incompatible`,
		},
		{
			name:        "sandboxed: nothing asks for the class",
			report:      gateReport("gvisor", gateRow("a", "gvisor-runsc", bad)),
			failOn:      FailOnSandboxed,
			wantOutcome: GateNothingChecked,
			wantSummary: `No workload asks for RuntimeClass "gvisor"`,
		},
		{
			// Two copies of one workload: one line of detail for each
			// incompatible copy, one entry in the failing list for the
			// identity.
			name: "sandboxed: copies of one workload are one failing identity",
			report: gateReport("gvisor",
				gateRow("web", "gvisor", ok), gateRow("web", "", bad), gateRow("web", "", bad)),
			failOn:      FailOnSandboxed,
			wantOutcome: GateFailed,
			wantSummary: "1 workload(s)",
			wantFailing: []schema.WorkloadRef{ref("web")},
			wantDetails: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate, err := EvaluateGate(tc.report, tc.failOn)
			if err != nil {
				t.Fatalf("EvaluateGate: %v", err)
			}
			if gate.Outcome != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", gate.Outcome, tc.wantOutcome)
			}
			if gate.Failed != (gate.Outcome == GateFailed) {
				t.Errorf("Failed = %v with Outcome %q; Failed must be true for GateFailed only", gate.Failed, gate.Outcome)
			}
			if !strings.Contains(gate.Summary, tc.wantSummary) {
				t.Errorf("Summary = %q, want it to hold %q", gate.Summary, tc.wantSummary)
			}
			if !reflect.DeepEqual(gate.Failing, tc.wantFailing) {
				t.Errorf("Failing = %v, want %v", gate.Failing, tc.wantFailing)
			}
			if len(gate.Details) != tc.wantDetails {
				t.Errorf("Details = %q, want %d line(s)", gate.Details, tc.wantDetails)
			}
			wantFailOn := tc.failOn
			if wantFailOn == "" {
				wantFailOn = FailOnIncompatible
			}
			if gate.FailOn != wantFailOn {
				t.Errorf("FailOn = %q, want %q", gate.FailOn, wantFailOn)
			}
		})
	}
}

// TestEvaluateGate_RefusedHasAResult: when the gate refuses, it returns the
// error and a result that says so. The caller renders the result, then
// returns the error. A zero result here would make the Markdown report show
// no gate line, and a report with no gate line reads as a pass.
func TestEvaluateGate_RefusedHasAResult(t *testing.T) {
	t.Parallel()
	report := gateReport("gvisor", gateRow("web", "", schema.CompatibilityIncompatible))
	report.Metadata.Source = &schema.ReportSource{
		Type:       schema.SourceManifests,
		Unrendered: []string{"kustomization file: overlay/kustomization.yaml"},
	}

	gate, err := EvaluateGate(report, FailOnSandboxed)
	if !errors.Is(err, ErrUnrenderedInput) {
		t.Fatalf("err = %v, want one that wraps ErrUnrenderedInput", err)
	}
	if gate.Outcome != GateRefused || gate.Failed {
		t.Errorf("Outcome = %q, Failed = %v; want %q and false", gate.Outcome, gate.Failed, GateRefused)
	}
	if gate.FailOn != FailOnSandboxed {
		t.Errorf("FailOn = %q, want %q", gate.FailOn, FailOnSandboxed)
	}
	for _, want := range []string{"not rendered", "overlay/kustomization.yaml", "kustomize build"} {
		if !strings.Contains(gate.Summary, want) {
			t.Errorf("Summary = %q, want it to hold %q", gate.Summary, want)
		}
	}
	if len(gate.Failing) != 0 {
		t.Errorf("Failing = %v, want none: a gate that did not look has no failures", gate.Failing)
	}

	// The same report under the other two settings is not refused.
	for _, failOn := range []FailOn{FailOnIncompatible, FailOnNone} {
		if _, err := EvaluateGate(report, failOn); err != nil {
			t.Errorf("--fail-on %s: err = %v, want nil (only sandboxed refuses unrendered input)", failOn, err)
		}
	}
}
