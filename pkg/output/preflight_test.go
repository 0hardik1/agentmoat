// Tests for the preflight renderer and the preflight/warnings blocks the
// plan, apply, and verify tables gained.
package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func samplePreflightReport() *schema.PreflightReport {
	r := schema.NewPreflightReport()
	r.Metadata = schema.PreflightMetadata{
		GeneratedAt: "2026-09-04T12:00:00Z", Cluster: "kind-agentmoat", AgentmoatVersion: "test", RuntimeClassName: "gvisor",
	}
	r.Spec = schema.PreflightSpec{
		Summary: schema.PreflightSummary{Ready: false, Total: 3, Error: 1, Warn: 1, Info: 1},
		Facts: schema.ClusterFacts{
			RuntimeClass: schema.RuntimeClassFacts{
				Name: "gvisor", Found: true, Handler: "gvisor",
				NodeSelector: map[string]string{"runtime": "gvisor"}, Tolerations: 1,
			},
			Nodes:    schema.NodeFacts{Total: 4, Ready: 4},
			Platform: schema.PlatformFacts{EKSAutoModeNodes: 2},
		},
		Findings: []schema.PreflightFinding{
			{ID: "runtimeclass-no-matching-nodes", Severity: schema.SeverityError,
				Message: `0 of 4 nodes match RuntimeClass "gvisor" nodeSelector runtime=gvisor`, Remediation: "label the nodes"},
			{ID: "eks-auto-mode-nodes", Severity: schema.SeverityWarn,
				Message: "2 of 4 node(s) in the cluster are EKS Auto Mode managed instances", Remediation: "add a node group"},
			{ID: "runtimeclass-no-overhead", Severity: schema.SeverityInfo, Message: "no overhead.podFixed"},
		},
	}
	return r
}

func TestRenderPreflightReportTable(t *testing.T) {
	t.Parallel()
	out := renderToString(t, samplePreflightReport())
	for _, want := range []string{
		"agentmoat preflight",
		"runtime-class: gvisor",
		"cluster: kind-agentmoat",
		"SUMMARY", "3 findings", "✗ blocked",
		"FACTS",
		`RuntimeClass "gvisor": handler=gvisor  nodeSelector=runtime=gvisor  tolerations=1  overhead=(none)`,
		"Nodes: total=4  ready=4  matching-selector=0",
		"Platform: eks-auto-mode=2",
		"FINDINGS",
		"✗ error", "runtimeclass-no-matching-nodes", "fix: label the nodes",
		"⚠ warn", "eks-auto-mode-nodes",
		"• info", "runtimeclass-no-overhead",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preflight output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestRenderPreflightReportTable_Ready(t *testing.T) {
	t.Parallel()
	r := samplePreflightReport()
	r.Spec.Summary = schema.PreflightSummary{Ready: true}
	r.Spec.Findings = []schema.PreflightFinding{}
	r.Spec.Facts.RuntimeClass.Overhead = map[string]string{"memory": "140Mi", "cpu": "250m"}
	r.Spec.Facts.Nodes.MatchingNames = []string{"gv-a", "gv-b"}
	r.Spec.Facts.Platform = schema.PlatformFacts{}
	out := renderToString(t, r)
	for _, want := range []string{"✓ ready", "(no findings", "overhead=cpu=250m,memory=140Mi", "matching: gv-a, gv-b"} {
		if !strings.Contains(out, want) {
			t.Errorf("ready output missing %q\nfull output:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Platform:") {
		t.Errorf("Platform line printed with all-zero counts:\n%s", out)
	}
}

func TestRenderPreflightReportJSON(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Render(samplePreflightReport(), FormatJSON, &buf, RenderOptions{}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	for _, want := range []string{`"kind": "PreflightReport"`, `"ready": false`, `"runtimeClassName": "gvisor"`, `"id": "eks-auto-mode-nodes"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("json missing %q:\n%s", want, buf.String())
		}
	}
}

func blockedApplyResult() *schema.ApplyResult {
	res := schema.NewApplyResult()
	res.Metadata = schema.ApplyMetadata{
		PlanHash: "hash-b", DryRun: true,
		Preflight: &schema.PreflightSummary{Ready: false, Total: 1, Error: 1},
	}
	res.Spec = schema.ApplySpec{
		Summary: schema.ApplySummary{Total: 1, Skipped: 1},
		Steps: []schema.StepResult{{
			Order: 1, Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "ns-a", Name: "web"},
			Status: schema.StepStatusSkipped, Error: "blocked by preflight: runtimeclass-missing",
		}},
		PreflightFindings: []schema.PreflightFinding{{
			ID: "runtimeclass-missing", Severity: schema.SeverityError,
			Message: `RuntimeClass "gvisor" does not exist`, Remediation: "kubectl apply -f deploy/runtimeclass.yaml",
		}},
	}
	return res
}

func TestRenderApplyResultTable_PreflightBlocked(t *testing.T) {
	t.Parallel()
	out := renderToString(t, blockedApplyResult())
	for _, want := range []string{
		"PREFLIGHT", "✗ blocked", "(1 error, 0 warn, 0 info)",
		"runtimeclass-missing", `RuntimeClass "gvisor" does not exist`, "fix: kubectl apply",
		"nothing was mutated",
		"• skipped", "blocked by preflight: runtimeclass-missing",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("blocked apply output missing %q\nfull output:\n%s", want, out)
		}
	}
}

func TestRenderApplyResultTable_PreflightReady(t *testing.T) {
	t.Parallel()
	res := blockedApplyResult()
	res.Metadata.Preflight = &schema.PreflightSummary{Ready: true, Total: 1, Warn: 1}
	res.Spec.PreflightFindings = []schema.PreflightFinding{{ID: "bottlerocket-nodes", Severity: schema.SeverityWarn, Message: "some"}}
	out := renderToString(t, res)
	if !strings.Contains(out, "PREFLIGHT") || !strings.Contains(out, "✓ ready") || !strings.Contains(out, "(0 error, 1 warn, 0 info)") {
		t.Errorf("ready apply output lacks the PREFLIGHT headline:\n%s", out)
	}
	if strings.Contains(out, "nothing was mutated") || strings.Contains(out, "bottlerocket-nodes") {
		t.Errorf("ready apply must not list findings or the blocked footer:\n%s", out)
	}
}

func TestRenderApplyResultTable_NoPreflightBlockWhenSkipped(t *testing.T) {
	t.Parallel()
	res := blockedApplyResult()
	res.Metadata.Preflight = nil
	res.Spec.PreflightFindings = nil
	out := renderToString(t, res)
	if strings.Contains(out, "PREFLIGHT") {
		t.Errorf("PREFLIGHT block printed without a preflight summary:\n%s", out)
	}
	var buf bytes.Buffer
	if err := Render(res, FormatJSON, &buf, RenderOptions{}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Key-shaped checks: the skipped step's error text legitimately says
	// "blocked by preflight", so a bare substring would false-positive.
	for _, key := range []string{`"preflight":`, `"preflightFindings":`} {
		if strings.Contains(buf.String(), key) {
			t.Errorf("json carries %s with no preflight run (omitempty broken):\n%s", key, buf.String())
		}
	}
}

func TestRenderMigrationPlanTable_Warnings(t *testing.T) {
	t.Parallel()
	plan := schema.NewMigrationPlan()
	plan.Metadata.PlanHash = "hash-w"
	plan.Spec = schema.PlanSpec{
		Summary:  schema.PlanSummary{Total: 1, Included: 1},
		Options:  schema.PlannerOptions{RuntimeClassName: "gvisor"},
		Steps:    []schema.PlanStep{{Order: 1, Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "ns", Name: "web"}, WaitFor: "Ready"}},
		Excluded: []schema.ExcludedWorkload{},
		Warnings: []schema.PlanWarning{{ID: "runtimeclass-no-matching-nodes", Message: "0 of 3 nodes match"}},
	}
	out := renderToString(t, plan)
	for _, want := range []string{"Cluster warnings:", "⚠ warn", "runtimeclass-no-matching-nodes", "0 of 3 nodes match", "do not change the steps or the plan hash"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan output missing %q\nfull output:\n%s", want, out)
		}
	}
	plan.Spec.Warnings = nil
	if out := renderToString(t, plan); strings.Contains(out, "Cluster warnings") {
		t.Errorf("warnings section printed with no warnings:\n%s", out)
	}
}

func TestSummarizeNodePlacement(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		np   *schema.NodePlacement
		want string
	}{
		{"nil", nil, "-"},
		{"unchecked", &schema.NodePlacement{Checked: false, Message: "x"}, "unchecked"},
		{"ok", &schema.NodePlacement{Checked: true, Nodes: []string{"a"}}, "ok"},
		{"mismatch", &schema.NodePlacement{Checked: true, Mismatched: []string{"a", "b"}}, "2 mismatched"},
	}
	for _, tc := range tests {
		if got := summarizeNodePlacement(tc.np); got != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestRenderVerifyReportTable_NodeColumn(t *testing.T) {
	t.Parallel()
	r := sampleVerifyReport()
	r.Spec.Results[0].NodePlacement = &schema.NodePlacement{Checked: true, Nodes: []string{"gv-1"}}
	r.Spec.Results[1].NodePlacement = &schema.NodePlacement{Checked: true, Mismatched: []string{"runc-1"}}
	out := renderToString(t, r)
	for _, want := range []string{"NODE", "1 mismatched"} {
		if !strings.Contains(out, want) {
			t.Errorf("verify output missing %q\nfull output:\n%s", want, out)
		}
	}
}
