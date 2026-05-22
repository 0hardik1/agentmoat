// Tests for the table renderer (table.go) and the explain renderer
// (explain.go). These tests pin behavior at the "shape" level (substring
// assertions on title, summary keywords, headers, key data cells) rather
// than full golden files. Two reasons:
//
//  1. The plan explicitly asks for table-driven, not golden.
//  2. lipgloss/table chooses padding based on the widest cell, so a single
//     character change in fixture data would invalidate every byte of a
//     golden snapshot for no real signal.
//
// All tests use a bytes.Buffer writer (non-*os.File, so ColorEnabled
// returns false on its own) and pass RenderOptions{NoColor: true}. That
// guarantees plain text on every CI and dev machine, regardless of the
// terminal's color profile.

package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// renderToString is a small helper that runs Render with NoColor=true and
// returns the resulting string. Tests then make `strings.Contains` style
// assertions.
func renderToString(t *testing.T, doc any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(doc, FormatTable, &buf, RenderOptions{NoColor: true}); err != nil {
		t.Fatalf("Render returned error: %v", err)
	}
	return buf.String()
}

func TestRenderScanReportTable(t *testing.T) {
	t.Parallel()

	// Build a minimal ScanReport with one of each compatibility class so
	// we can assert that all three semantic colors and badges fire.
	report := schema.NewScanReport()
	report.Metadata.Cluster = "kind-agentmoat-e2e"
	report.Spec.Summary = schema.Summary{
		Total:        3,
		Compatible:   1,
		NeedsReview:  1,
		Incompatible: 1,
	}
	report.Spec.Workloads = []schema.WorkloadResult{
		{
			Kind: "Deployment", Namespace: "default", Name: "web",
			Compatibility:  schema.CompatibilityCompatible,
			Recommendation: "set runtimeClassName: gvisor",
		},
		{
			Kind: "Deployment", Namespace: "default", Name: "queue",
			Compatibility: schema.CompatibilityReview,
			Reasons: []schema.Reason{
				{RuleID: "network-throughput", Severity: schema.SeverityInfo},
			},
			Recommendation: "review network throughput",
		},
		{
			Kind: "DaemonSet", Namespace: "kube-system", Name: "node-exporter",
			Compatibility: schema.CompatibilityIncompatible,
			Reasons: []schema.Reason{
				{RuleID: "host-network", Severity: schema.SeverityError},
				{RuleID: "raw-socket", Severity: schema.SeverityError},
			},
			Recommendation: "do not migrate",
		},
	}

	out := renderToString(t, report)

	wantSubs := []string{
		"agentmoat scan",              // title
		"cluster: kind-agentmoat-e2e", // subtitle chip
		"SUMMARY",                     // summary label
		"3 workloads",                 // total noun on the SUMMARY headline
		// Bar chart trailing-label cells (count + percent). 1/3 ~= 33% each.
		"   1 (33%)",
		// Per-row fill runes (no-color mode picks the per-item NoColorFill).
		"█",                         // compatible row
		"▓",                         // review row
		"▒",                         // incompatible row
		"NAMESPACE", "KIND", "NAME", // headers
		"VERDICT", "REASONS",
		"web", // row data
		"node-exporter",
		"host-network",
		// Badge symbols (plain text in NoColor mode is just symbol+space+status)
		"✓ compatible",
		"⚠ review",
		"✗ incompatible",
		// Footer hint points operators at JSON for the per-row
		// Recommendation field that no longer appears as a column.
		"use --output json",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("scan output missing %q\nfull output:\n%s", s, out)
		}
	}
	// Negative checks: the RECOMMENDATION column was removed because its
	// canned text dominated table width on large clusters. The field is
	// still in the JSON/YAML payload; the table just no longer prints it.
	notWant := []string{
		"RECOMMENDATION",
		"set runtimeClassName: gvisor",
		"review network throughput",
		"do not migrate",
	}
	for _, s := range notWant {
		if strings.Contains(out, s) {
			t.Errorf("scan output should not contain %q (RECOMMENDATION column was removed)\nfull output:\n%s", s, out)
		}
	}
}

func TestRenderScanReportTable_Empty(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Summary = schema.Summary{Total: 0}

	out := renderToString(t, report)
	// Title + SUMMARY still print; the placeholder is the part that
	// tells the user there were zero workloads to walk.
	if !strings.Contains(out, "agentmoat scan") {
		t.Errorf("empty scan missing title\nfull output:\n%s", out)
	}
	if !strings.Contains(out, "(no workloads found)") {
		t.Errorf("empty scan missing placeholder\nfull output:\n%s", out)
	}
}

// TestRenderScanReportTable_NoChartWhenEmpty pins the fall-through: when
// every bucket is zero, the chart helper returns "" so the renderer
// prints the SUMMARY headline + the empty-state placeholder, with no
// bar runes between them.
func TestRenderScanReportTable_NoChartWhenEmpty(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Summary = schema.Summary{Total: 0}
	out := renderToString(t, report)

	// SUMMARY headline still prints with the total noun.
	if !strings.Contains(out, "0 workloads") {
		t.Errorf("expected SUMMARY headline '0 workloads', got:\n%s", out)
	}
	// No chart should be emitted: none of the four fill runes appear.
	for _, r := range []rune{'█', '▓', '▒', '░'} {
		if strings.ContainsRune(out, r) {
			t.Errorf("expected no chart fill rune %q in empty output, got:\n%s", r, out)
		}
	}
	// Empty-state placeholder still prints.
	if !strings.Contains(out, "(no workloads found)") {
		t.Errorf("empty scan missing placeholder\nfull output:\n%s", out)
	}
}

func TestRenderMigrationPlanTable(t *testing.T) {
	t.Parallel()

	plan := schema.NewMigrationPlan()
	plan.Metadata.PlanHash = "sha256:abc123"
	plan.Spec.Options = schema.PlannerOptions{RuntimeClassName: "gvisor"}
	plan.Spec.Summary = schema.PlanSummary{Total: 2, Included: 1, Excluded: 1}
	plan.Spec.Steps = []schema.PlanStep{
		{
			Order: 1,
			Target: schema.WorkloadRef{
				Kind: "Deployment", Namespace: "default", Name: "web",
			},
			Action:           "set-runtime-class",
			RuntimeClassName: "gvisor",
			AddToleration:    true,
			WaitFor:          "Ready",
			RiskScore:        10,
			Notes:            "fronted by LB",
		},
	}
	plan.Spec.Excluded = []schema.ExcludedWorkload{
		{
			Target: schema.WorkloadRef{
				Kind: "DaemonSet", Namespace: "kube-system", Name: "node-exporter",
			},
			Compatibility: schema.CompatibilityIncompatible,
			Reason:        "host-network",
		},
	}

	out := renderToString(t, plan)
	wantSubs := []string{
		"agentmoat plan",
		"plan-hash: sha256:abc123",
		"runtime-class: gvisor",
		"SUMMARY",
		"2 workloads", // total noun on the SUMMARY headline
		// Bar chart trailing-label cells: 1 included + 1 excluded out of 2.
		"   1 (50%)",
		// Per-row fill runes: included uses '█', excluded uses '░'.
		"█", "░",
		// step table headers + data
		"#", "RISK", "WAIT-FOR", "NOTES",
		"web", "Ready", "fronted by LB",
		// excluded section
		"Excluded workloads:",
		"node-exporter", "host-network",
		"✗ incompatible",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("plan output missing %q\nfull output:\n%s", s, out)
		}
	}
}

func TestRenderMigrationPlanTable_NoIncluded(t *testing.T) {
	t.Parallel()
	plan := schema.NewMigrationPlan()
	plan.Metadata.PlanHash = "sha256:empty"
	plan.Spec.Options = schema.PlannerOptions{RuntimeClassName: "gvisor"}
	plan.Spec.Summary = schema.PlanSummary{Total: 0}

	out := renderToString(t, plan)
	if !strings.Contains(out, "(no included steps)") {
		t.Errorf("expected empty-state placeholder, got:\n%s", out)
	}
	// Excluded section should not print when there are no excluded entries.
	if strings.Contains(out, "Excluded workloads:") {
		t.Errorf("Excluded section should be omitted when empty, got:\n%s", out)
	}
}

func TestRenderApplyResultTable(t *testing.T) {
	t.Parallel()

	res := schema.NewApplyResult()
	res.Metadata.PlanHash = "sha256:abc"
	res.Metadata.DryRun = true
	res.Spec.Summary = schema.ApplySummary{
		Total: 4, Applied: 2, AlreadyApplied: 1, Skipped: 0, Failed: 1,
	}
	res.Spec.Steps = []schema.StepResult{
		{
			Order: 1,
			Target: schema.WorkloadRef{
				Kind: "Deployment", Namespace: "default", Name: "web",
			},
			Status: schema.StepStatusApplied,
			Patch:  `{"spec":{"template":{"spec":{"runtimeClassName":"gvisor"}}}}`,
		},
		{
			Order: 2,
			Target: schema.WorkloadRef{
				Kind: "Deployment", Namespace: "default", Name: "queue",
			},
			Status: schema.StepStatusAlreadyApplied,
		},
		{
			Order: 3,
			Target: schema.WorkloadRef{
				Kind: "CronJob", Namespace: "batch", Name: "nightly",
			},
			Status: schema.StepStatusFailed,
			Error:  "API server rejected patch",
		},
	}

	out := renderToString(t, res)
	wantSubs := []string{
		"agentmoat apply",
		"plan-hash: sha256:abc",
		"dry-run: true",
		"SUMMARY",
		"4 steps", // total noun on the SUMMARY headline
		// Bar chart trailing-label cells. 2/4=50%, 1/4=25%, 0/4=0%, 1/4=25%.
		"   2 (50%)",
		"   1 (25%)",
		"   0 ( 0%)",
		// Per-row fill runes: applied=█, already-applied=▓, skipped omitted (count=0
		// renders as all spaces in its bar slot), failed=▒.
		"█", "▓", "▒",
		"#", "STATUS", "NOTE",
		"web", "queue", "nightly",
		"API server rejected patch",
		"✓ applied",
		"→ already-applied",
		"✗ failed",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("apply output missing %q\nfull output:\n%s", s, out)
		}
	}
}

func TestRenderApplyResultTable_Empty(t *testing.T) {
	t.Parallel()
	res := schema.NewApplyResult()
	res.Metadata.DryRun = false
	out := renderToString(t, res)
	if !strings.Contains(out, "(no steps)") {
		t.Errorf("expected empty-state placeholder, got:\n%s", out)
	}
	if !strings.Contains(out, "dry-run: false") {
		t.Errorf("expected dry-run: false chip, got:\n%s", out)
	}
}

func TestRenderRollbackResultTable(t *testing.T) {
	t.Parallel()

	rb := schema.NewRollbackResult()
	rb.Metadata.PlanHash = "sha256:abc"
	rb.Metadata.DryRun = false
	rb.Spec.Summary = schema.ApplySummary{Total: 1, Applied: 1}
	rb.Spec.Steps = []schema.StepResult{
		{
			Order: 1,
			Target: schema.WorkloadRef{
				Kind: "Deployment", Namespace: "default", Name: "web",
			},
			Status: schema.StepStatusApplied,
		},
	}

	out := renderToString(t, rb)
	wantSubs := []string{
		"agentmoat rollback", // title uses the action word
		"plan-hash: sha256:abc",
		"dry-run: false",
		"1 steps",     // total noun on the SUMMARY headline
		"   1 (100%)", // single non-zero bucket = 100% of total
		"█",           // applied fill rune
		"web",
		"✓ applied",
	}
	for _, s := range wantSubs {
		if !strings.Contains(out, s) {
			t.Errorf("rollback output missing %q\nfull output:\n%s", s, out)
		}
	}
}

// TestRender_PlainOutputHasNoANSI is a guardrail: with NoColor:true and a
// bytes.Buffer writer, the output must not contain ANSI escape codes (so
// piped output is "real" plain text downstream consumers can parse).
func TestRender_PlainOutputHasNoANSI(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Summary = schema.Summary{Total: 1, Compatible: 1}
	report.Spec.Workloads = []schema.WorkloadResult{
		{Kind: "Deployment", Namespace: "ns", Name: "x",
			Compatibility: schema.CompatibilityCompatible},
	}
	out := renderToString(t, report)
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("plain output contains ESC (0x1b), want none. output:\n%q", out)
	}
}

// TestRender_JSONIsByteIdenticalRegardlessOfOpts pins the schema-stability
// invariant: machine output must not change based on color/quiet flags.
func TestRender_JSONIsByteIdenticalRegardlessOfOpts(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Summary = schema.Summary{Total: 1, Compatible: 1}
	report.Spec.Workloads = []schema.WorkloadResult{
		{Kind: "Deployment", Namespace: "ns", Name: "x",
			Compatibility: schema.CompatibilityCompatible},
	}
	var a, b, c bytes.Buffer
	if err := Render(report, FormatJSON, &a, RenderOptions{NoColor: false}); err != nil {
		t.Fatal(err)
	}
	if err := Render(report, FormatJSON, &b, RenderOptions{NoColor: true}); err != nil {
		t.Fatal(err)
	}
	if err := Render(report, FormatJSON, &c, RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) || !bytes.Equal(b.Bytes(), c.Bytes()) {
		t.Errorf("JSON output varies with RenderOptions:\nNoColor=false:\n%s\nNoColor=true:\n%s\nzero-value:\n%s",
			a.String(), b.String(), c.String())
	}
}

// TestRender_YAMLIsByteIdenticalRegardlessOfOpts is the YAML counterpart.
func TestRender_YAMLIsByteIdenticalRegardlessOfOpts(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Summary = schema.Summary{Total: 0}
	var a, b bytes.Buffer
	if err := Render(report, FormatYAML, &a, RenderOptions{NoColor: false}); err != nil {
		t.Fatal(err)
	}
	if err := Render(report, FormatYAML, &b, RenderOptions{NoColor: true}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Errorf("YAML output varies with RenderOptions")
	}
}

// TestSummarizeReasons exercises the inline cap and the "(+N more)" tail
// directly (it is a pure helper, so the assertion is exact).
func TestSummarizeReasons(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   []schema.Reason
		want string
	}{
		{"none", nil, "-"},
		{"one", []schema.Reason{
			{RuleID: "a", Severity: schema.SeverityError},
		}, "a(error)"},
		{"three", []schema.Reason{
			{RuleID: "a", Severity: schema.SeverityError},
			{RuleID: "b", Severity: schema.SeverityWarn},
			{RuleID: "c", Severity: schema.SeverityInfo},
		}, "a(error); b(warn); c(info)"},
		{"more than cap", []schema.Reason{
			{RuleID: "a", Severity: schema.SeverityError},
			{RuleID: "b", Severity: schema.SeverityWarn},
			{RuleID: "c", Severity: schema.SeverityInfo},
			{RuleID: "d", Severity: schema.SeverityInfo},
			{RuleID: "e", Severity: schema.SeverityInfo},
		}, "a(error); b(warn); c(info) (+2 more)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeReasons(tc.in)
			if got != tc.want {
				t.Errorf("summarizeReasons(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTruncateCell exercises the rune-correct cell truncator that bounds
// free-form table cells (workload names, notes, messages) so a single
// pathological value cannot wrap the table past the terminal edge.
func TestTruncateCell(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"empty", "", 10, ""},
		{"under max", "abc", 10, "abc"},
		{"exactly max", "abcdefghij", 10, "abcdefghij"},
		{"over max trims with ellipsis", "abcdefghijkl", 10, "abcdefg..."},
		// Multi-byte: each glyph counts as 1 rune, not as its byte length.
		// "héllo" is 5 runes (one is 2 bytes) so it passes through at max=5.
		{"multibyte under max", "héllo", 5, "héllo"},
		// Beyond max, runes are counted not bytes; with max=4 we keep 1
		// rune then append "..." for a 4-rune output.
		{"multibyte trimmed", "héllo", 4, "h..."},
		// max < 4 cannot hold the ellipsis so we return s unchanged
		// rather than producing a misleading "..." prefix.
		{"max too small", "abcdef", 3, "abcdef"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateCell(tc.in, tc.max)
			if got != tc.want {
				t.Errorf("truncateCell(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
		})
	}
}

// TestTrackingTruncator pins the bookkeeping side of the truncator:
// .truncated must flip only when a cell actually got shortened, since
// renderers gate the JSON-hint footer on that flag.
func TestTrackingTruncator(t *testing.T) {
	t.Parallel()
	var tr trackingTruncator
	if got := tr.cell("short", 10); got != "short" || tr.truncated {
		t.Errorf("under-max cell tripped truncated flag: out=%q, truncated=%v", got, tr.truncated)
	}
	if got := tr.cell("this is way over the limit", 10); got != "this is..." || !tr.truncated {
		t.Errorf("over-max cell should set truncated and shorten: out=%q, truncated=%v", got, tr.truncated)
	}
}

// TestCompactPatchPreview pins the truncation and compaction behavior of
// the apply-table patch-preview helper.
func TestCompactPatchPreview(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "compacts whitespace",
			in:   "{ \"a\": 1 }",
			want: `{"a":1}`,
		},
		{
			name: "truncates at 60 chars",
			in:   `{"spec":{"template":{"spec":{"runtimeClassName":"gvisor","tolerations":[{"key":"runtime","value":"gvisor","effect":"NoSchedule"}]}}}}`,
			// compactPatchPreview keeps the first 57 chars and appends "..."
			// for a total length of 60.
			want: `{"spec":{"template":{"spec":{"runtimeClassName":"gvisor",...`,
		},
		{
			name: "invalid JSON passed through",
			in:   "not json",
			want: "not json",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := compactPatchPreview(tc.in)
			if got != tc.want {
				t.Errorf("compactPatchPreview = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// VerifyReport renderer tests.
//
// These tests preserve the schema/envelope assertions main contributed
// (testdata round-trips, kind/apiVersion checks) but adapt the substring
// list to the new lipgloss-style header + SUMMARY shape.
// ---------------------------------------------------------------------------

func sampleVerifyReport() *schema.VerifyReport {
	r := schema.NewVerifyReport()
	r.Metadata = schema.VerifyMetadata{
		GeneratedAt:      "2026-05-22T12:00:00Z",
		Cluster:          "kind-agentmoat-e2e",
		AgentmoatVersion: "test",
		PlanHash:         "hash-1",
		InPodProbe:       true,
	}
	r.Spec = schema.VerifySpec{
		Summary: schema.VerifySummary{Total: 2, OK: 1, Mismatch: 1, Error: 0},
		Results: []schema.VerifyResult{
			{
				Order:    1,
				Target:   schema.WorkloadRef{Kind: "Deployment", Namespace: "ns-a", Name: "web"},
				Status:   schema.VerifyStatusOK,
				Expected: "gvisor",
				Actual:   "gvisor",
				Message:  "probe confirmed gVisor markers",
				Probe:    &schema.ProbeResult{Pod: "web-abc", Detected: true, Markers: "gvisor"},
			},
			{
				Order:    2,
				Target:   schema.WorkloadRef{Kind: "StatefulSet", Namespace: "ns-b", Name: "cache"},
				Status:   schema.VerifyStatusMismatch,
				Expected: "gvisor",
				Actual:   "",
				Message:  "pod runtimeClassName empty",
			},
		},
	}
	return r
}

// TestRenderVerifyReportTable asserts the table renderer emits one row per
// step with the expected columns and that the per-row STATUS / EXPECTED /
// ACTUAL / PROBE values land in the right cells.
func TestRenderVerifyReportTable(t *testing.T) {
	t.Parallel()
	out := renderToString(t, sampleVerifyReport())

	wantStrings := []string{
		"agentmoat verify",   // title (Bold in TTY, plain here)
		"plan-hash: hash-1",  // subtitle chip
		"in-pod-probe: true", // subtitle chip
		"SUMMARY",            // summary label
		"2 results",          // total noun on the SUMMARY headline
		// Bar chart trailing-label cells: 1/2 ok, 1/2 mismatch, 0/2 error.
		"   1 (50%)",
		"   0 ( 0%)",
		// Per-row fill runes: ok=█, mismatch=▓; error row has count=0 so no fill.
		"█", "▓",
		"#", "STATUS", // headers
		"KIND/NS/NAME",       //
		"EXPECTED", "ACTUAL", //
		"PROBE", "MESSAGE", //
		"Deployment/ns-a/web", // row data
		"StatefulSet/ns-b/cache",
		"(empty)",    // displayActual for the mismatch row
		"✓ ok",       // badges in NoColor mode
		"⚠ mismatch", //
	}
	for _, want := range wantStrings {
		if !strings.Contains(out, want) {
			t.Errorf("verify output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRenderVerifyReportTable_Empty pins the empty-results placeholder.
func TestRenderVerifyReportTable_Empty(t *testing.T) {
	t.Parallel()
	r := schema.NewVerifyReport()
	r.Metadata.PlanHash = "hash-empty"
	r.Metadata.InPodProbe = false
	out := renderToString(t, r)
	if !strings.Contains(out, "(no results)") {
		t.Errorf("expected empty placeholder, got:\n%s", out)
	}
	if !strings.Contains(out, "in-pod-probe: false") {
		t.Errorf("expected in-pod-probe: false chip, got:\n%s", out)
	}
}

// TestRenderVerifyReportJSONYAML asserts the generic encoders produce a
// document whose key fields are preserved. We don't pin byte layouts; the
// e2e relies on jq paths into .spec.summary.* and .spec.results[].* so just
// confirming the paths exist is enough.
func TestRenderVerifyReportJSONYAML(t *testing.T) {
	t.Parallel()
	r := sampleVerifyReport()

	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Render(r, FormatJSON, &buf, RenderOptions{}); err != nil {
			t.Fatalf("Render(JSON): %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal JSON: %v", err)
		}
		assertVerifyEnvelope(t, got)
	})

	t.Run("yaml", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Render(r, FormatYAML, &buf, RenderOptions{}); err != nil {
			t.Fatalf("Render(YAML): %v", err)
		}
		var got map[string]any
		if err := yaml.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal YAML: %v", err)
		}
		assertVerifyEnvelope(t, got)
	})
}

func assertVerifyEnvelope(t *testing.T, got map[string]any) {
	t.Helper()
	if got["kind"] != "VerifyReport" {
		t.Errorf("kind: got %v, want VerifyReport", got["kind"])
	}
	if got["apiVersion"] != schema.APIVersion {
		t.Errorf("apiVersion: got %v, want %s", got["apiVersion"], schema.APIVersion)
	}
	spec, ok := got["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type")
	}
	summary, ok := spec["summary"].(map[string]any)
	if !ok {
		t.Fatalf("spec.summary missing or wrong type")
	}
	for _, k := range []string{"total", "ok", "mismatch", "error"} {
		if _, ok := summary[k]; !ok {
			t.Errorf("spec.summary missing key %q", k)
		}
	}
	results, ok := spec["results"].([]any)
	if !ok {
		t.Fatalf("spec.results missing or wrong type")
	}
	if len(results) != 2 {
		t.Errorf("spec.results length: got %d, want 2", len(results))
	}
}

// ---------------------------------------------------------------------------
// ExplainDocument renderer tests.
// ---------------------------------------------------------------------------

// TestRenderExplainDocumentTable_TopicMode_Plain asserts that with NoColor=true
// (production-realistic for pipes and CI) the renderer prints the raw
// markdown content. The e2e asserts the stdout begins with "# " so we cover
// that here too.
func TestRenderExplainDocumentTable_TopicMode_Plain(t *testing.T) {
	t.Parallel()
	d := schema.NewExplainDocument()
	d.Spec = schema.ExplainSpec{
		Topic:   "runtimeclass",
		Content: "# RuntimeClass 101\n\nbody.\n",
		Topics:  []string{"runtimeclass"},
	}
	out := renderToString(t, d)
	if !strings.HasPrefix(out, "# RuntimeClass 101") {
		t.Errorf("output should start with '# ': got %q", out)
	}
	if !strings.Contains(out, "body.") {
		t.Errorf("output missing body: %q", out)
	}
}

// TestRenderExplainDocumentTable_TopicMode_AutoDowngradesNonTTY asserts that
// even when the caller hasn't set NoColor (RenderOptions{}), the renderer
// downgrades to plain because the writer is non-*os.File. This mirrors the
// production call path: stdout-as-pipe -> ColorEnabled returns false -> we
// skip glamour. Downstream tools see the raw markdown they expect.
func TestRenderExplainDocumentTable_TopicMode_AutoDowngradesNonTTY(t *testing.T) {
	t.Parallel()
	d := schema.NewExplainDocument()
	d.Spec = schema.ExplainSpec{
		Topic:   "runtimeclass",
		Content: "# RuntimeClass 101\n\nbody.\n",
	}
	var buf bytes.Buffer
	if err := Render(d, FormatTable, &buf, RenderOptions{NoColor: false}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("expected no ANSI (non-TTY writer downgrades color): output:\n%q", out)
	}
	if !strings.HasPrefix(out, "# RuntimeClass 101") {
		t.Errorf("output should still start with raw markdown: %q", out)
	}
}

// TestRenderExplainDocumentTable_ListMode asserts the renderer prints the
// available topic names when Spec.Content is empty.
func TestRenderExplainDocumentTable_ListMode(t *testing.T) {
	t.Parallel()
	d := schema.NewExplainDocument()
	d.Spec = schema.ExplainSpec{
		Topics: []string{"compatibility", "gvisor", "performance", "runtimeclass", "threat-model"},
	}
	out := renderToString(t, d)
	if !strings.Contains(out, "agentmoat explain: available topics") {
		t.Errorf("list-mode output missing header, got:\n%s", out)
	}
	for _, topic := range d.Spec.Topics {
		if !strings.Contains(out, topic) {
			t.Errorf("list-mode output missing topic %q\noutput: %s", topic, out)
		}
	}
}

// TestRenderExplainDocumentJSON ensures the envelope round-trips through the
// generic JSON encoder.
func TestRenderExplainDocumentJSON(t *testing.T) {
	t.Parallel()
	d := schema.NewExplainDocument()
	d.Spec = schema.ExplainSpec{
		Topic:   "runtimeclass",
		Content: "# RuntimeClass 101\n",
		Topics:  []string{"runtimeclass"},
	}
	var buf bytes.Buffer
	if err := Render(d, FormatJSON, &buf, RenderOptions{}); err != nil {
		t.Fatalf("Render(JSON): %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["kind"] != "ExplainDocument" {
		t.Errorf("kind: got %v", got["kind"])
	}
	spec, ok := got["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing")
	}
	if spec["topic"] != "runtimeclass" {
		t.Errorf("topic: got %v", spec["topic"])
	}
}

// TestRenderUnknownType asserts the dispatcher errors cleanly when given a
// type it does not know how to render. Pins behavior the CLI relies on.
func TestRenderUnknownType(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Render(struct{ Foo int }{Foo: 1}, FormatTable, &buf, RenderOptions{})
	if err == nil {
		t.Fatalf("expected error for unknown type")
	}
	if !strings.Contains(err.Error(), "no table renderer") {
		t.Errorf("error should mention unknown type: %v", err)
	}
}
