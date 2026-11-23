// Tests for the two views of a VerifyReport from manifest files (`verify
// -f`): the table and the Markdown.
//
// What the views must do that a verify of a plan does not need:
//   - Say what "all" is. The report is about the workloads of the input that
//     ask for the class, and the count of the others is in the view.
//   - Say "not a pass" in words when a workload has no verdict. The exit
//     code for that is 1, and a job summary shows no exit code.
//   - Keep text from a manifest inert (a name, a kind, a reason with a kind
//     in it).
package output

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/verifier"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// manifestVerifyReport is a report of a verify from manifests: one workload
// that is ok through its selector, and notChecked workloads with no verdict.
func manifestVerifyReport(notChecked ...schema.VerifyNotChecked) *schema.VerifyReport {
	r := schema.NewVerifyReport()
	r.Metadata = schema.VerifyMetadata{
		Cluster: "prod-eu", RuntimeClassName: "gvisor",
		Source: &schema.ReportSource{Type: schema.SourceManifests, Inputs: 4},
	}
	r.Spec = schema.VerifySpec{
		Summary: schema.VerifySummary{Total: 1, OK: 1, NotChecked: len(notChecked), Excluded: 7},
		Results: []schema.VerifyResult{{
			Order: 1, Target: schema.WorkloadRef{Kind: "Rollout.argoproj.io", Namespace: "shop", Name: "canary"},
			Status: schema.VerifyStatusOK, Expected: "gvisor", Actual: "gvisor", Basis: schema.VerifyBasisSelector,
			Message: "checked the pods that the selector of the manifest selects; verify did not read the Rollout.argoproj.io object",
			Origin:  &schema.Origin{File: "k8s/canary.yaml", Line: 3},
		}},
		NotChecked: notChecked,
	}
	return r
}

// knativeNotChecked is a workload with no verdict.
func knativeNotChecked() schema.VerifyNotChecked {
	return schema.VerifyNotChecked{
		Target: schema.WorkloadRef{Kind: "Service.serving.knative.dev", Namespace: "shop", Name: "fn"},
		Reason: "verify has no reader for the kind Service.serving.knative.dev, and the object has no spec.selector that selects its pods by label",
		Origin: &schema.Origin{File: "k8s/fn.yaml", Line: 1},
	}
}

func TestVerifyMarkdown_Manifests(t *testing.T) {
	t.Parallel()
	failed := manifestVerifyReport(knativeNotChecked())
	failed.Spec.Results[0].Status = schema.VerifyStatusMismatch
	failed.Spec.Summary = schema.VerifySummary{Total: 1, Mismatch: 1, NotChecked: 1, Excluded: 7}
	onlyNotChecked := manifestVerifyReport(knativeNotChecked())
	onlyNotChecked.Spec.Results = []schema.VerifyResult{}
	onlyNotChecked.Spec.Summary = schema.VerifySummary{NotChecked: 1, Excluded: 7}

	tests := []struct {
		name   string
		report *schema.VerifyReport
		want   []string
		bad    []string
	}{
		{
			name:   "all ok",
			report: manifestVerifyReport(),
			want: []string{
				"✅ **All 1 workload(s) are verified.** These are the workloads of the input that ask for RuntimeClass `gvisor`.",
				"Source: 4 manifest input(s) · RuntimeClass: `gvisor`",
				"| Workloads | OK | Mismatch | Error | Not checked | Not looked at |",
				"| 1 | 1 | 0 | 0 | 0 | 7 |",
			},
			bad: []string{"Plan hash", "### Not checked", "not a pass"},
		},
		{
			name:   "a workload with no verdict",
			report: manifestVerifyReport(knativeNotChecked()),
			want: []string{
				"⚠️ **1 workload(s) could not be checked. This is not a pass.** It is also not evidence that the deploy failed: " +
					"verify has no verdict for these workloads, and the table below has the reason for each one. " +
					"The other 1 workload(s) are verified.",
				"| 1 | 1 | 0 | 0 | 1 | 7 |",
				"### Not checked (1)",
				"| Namespace | Kind | Name | Defined in | Why there is no verdict |",
				"| `shop` | `Service.serving.knative.dev` | `fn` | `k8s/fn.yaml:1` |",
			},
			bad: []string{"✅", "All 1 workload(s) are verified"},
		},
		{
			name:   "only workloads with no verdict",
			report: onlyNotChecked,
			want:   []string{"⚠️ **1 workload(s) could not be checked. This is not a pass.**", "| 0 | 0 | 0 | 0 | 1 | 7 |"},
			bad:    []string{"✅", "The other 0", "Nothing was verified"},
		},
		{
			// A failure is the headline, and the count of the workloads
			// with no verdict is in it too.
			name:   "a mismatch and a workload with no verdict",
			report: failed,
			want:   []string{"❌ **1 of 1 workload(s) are not verified.** 1 more could not be checked.", "### Not checked (1)", "### Mismatch or error (1)"},
			bad:    []string{"✅", "⚠️"},
		},
	}
	for _, tc := range tests {
		out := renderMarkdown(t, tc.report, nil)
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: the output lacks %q:\n%s", tc.name, want, out)
			}
		}
		for _, bad := range tc.bad {
			if strings.Contains(out, bad) {
				t.Errorf("%s: the output must not have %q:\n%s", tc.name, bad, out)
			}
		}
	}

	// The list of workloads with no verdict comes before the results: it
	// is the reason that the report is not a pass.
	out := renderMarkdown(t, manifestVerifyReport(knativeNotChecked()), nil)
	if notChecked, ok := strings.Index(out, "### Not checked"), strings.Index(out, "<summary>OK ("); notChecked < 0 || ok < 0 || notChecked > ok {
		t.Errorf("the not-checked list must come before the ok rows (offsets %d and %d):\n%s", notChecked, ok, out)
	}

	// A verify of a plan has none of the columns and no source.
	plan := renderMarkdown(t, sampleVerifyReport(), nil)
	for _, bad := range []string{"Not checked", "Not looked at", "Source:", "RuntimeClass:"} {
		if strings.Contains(plan, bad) {
			t.Errorf("a verify of a plan must not show %q:\n%s", bad, plan)
		}
	}
}

// TestVerifyMarkdown_HostileManifest: each name, kind, file, class, and
// reason of a verify from manifests is text from a file of a pull request.
// None of it can add structure, a link, or a mention to the report.
func TestVerifyMarkdown_HostileManifest(t *testing.T) {
	t.Parallel()
	report := manifestVerifyReport(schema.VerifyNotChecked{
		Target: schema.WorkloadRef{Kind: hostile("kind"), Namespace: hostile("ns"), Name: hostile("name")},
		Reason: "verify has no reader for the kind " + hostile("kind") + ", and the object has no spec.selector",
		Origin: &schema.Origin{File: hostile("file"), Line: 9},
	})
	report.Metadata.RuntimeClassName = hostile("class")
	r := &report.Spec.Results[0]
	r.Target = schema.WorkloadRef{Kind: hostile("kind"), Namespace: hostile("ns"), Name: hostile("name")}
	r.Status = schema.VerifyStatusMismatch
	r.Expected, r.Actual = hostile("expected"), hostile("actual")
	r.Message = "pod " + hostile("pod") + ` has runtimeClassName=""`
	report.Spec.Summary = schema.VerifySummary{Total: 1, Mismatch: 1, NotChecked: 1}
	assertInert(t, renderMarkdown(t, report, nil), []string{"## agentmoat verify", "### Not checked (1)", "### Mismatch or error (1)"})

	// The headline of a pass has the class name in it. The document then
	// has one line that starts with the pass mark (the headline of the
	// renderer), and the class name did not add a second one.
	ok := manifestVerifyReport()
	ok.Metadata.RuntimeClassName = hostile("class")
	out := renderMarkdown(t, ok, nil)
	assertNoLiveMarkup(t, out)
	assertTableShape(t, out)
	passLines, headings := 0, 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "✅") {
			passLines++
		}
		if strings.HasPrefix(line, "#") {
			headings++
		}
	}
	if passLines != 1 || headings != 1 {
		t.Errorf("%d line(s) start with the pass mark and %d with a heading mark; want 1 and 1:\n%s", passLines, headings, out)
	}
}

func TestRenderVerifyReportTable_Manifests(t *testing.T) {
	t.Parallel()
	out := renderToString(t, manifestVerifyReport(knativeNotChecked()))
	for _, want := range []string{
		"source: manifests   runtime-class: gvisor   in-pod-probe: false",
		"7 workload(s) of the input do not ask for the class and were not looked at",
		"NOT CHECKED (1): this is not a pass",
		"Service.serving.knative.dev/shop/fn: verify has no reader for the kind Service.serving.knative.dev",
		"Rollout.argoproj.io/shop/canary",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "plan-hash") {
		t.Errorf("a verify from manifests has no plan hash to show:\n%s", out)
	}
	// The block is above the results table.
	if block, table := strings.Index(out, "NOT CHECKED"), strings.Index(out, "KIND/NS/NAME"); block < 0 || table < 0 || block > table {
		t.Errorf("the NOT CHECKED block must come before the table (offsets %d and %d):\n%s", block, table, out)
	}

	// Only workloads with no verdict: the block is there, and the view
	// does not stop at "(no results)" before it.
	only := manifestVerifyReport(knativeNotChecked())
	only.Spec.Results = []schema.VerifyResult{}
	only.Spec.Summary = schema.VerifySummary{NotChecked: 1, Excluded: 7}
	onlyOut := renderToString(t, only)
	if !strings.Contains(onlyOut, "NOT CHECKED (1): this is not a pass") || !strings.Contains(onlyOut, "(no results)") {
		t.Errorf("a report with only workloads that are not checked:\n%s", onlyOut)
	}

	// No workload with no verdict: no block. A verify of a plan: no line
	// about the input.
	if strings.Contains(renderToString(t, manifestVerifyReport()), "NOT CHECKED") {
		t.Errorf("a verify with each workload checked has a NOT CHECKED block")
	}
	if plan := renderToString(t, sampleVerifyReport()); strings.Contains(plan, "of the input") || !strings.Contains(plan, "plan-hash: ") {
		t.Errorf("a verify of a plan changed:\n%s", plan)
	}
}

// TestRenderVerifyReportTable_ControlCharacters: a name in a manifest can
// have an escape sequence in it. On a terminal that could move the cursor
// and write over the report, so the table replaces control characters.
func TestRenderVerifyReportTable_ControlCharacters(t *testing.T) {
	t.Parallel()
	const esc = "\x1b[2K\x1b[1A"
	report := manifestVerifyReport(schema.VerifyNotChecked{
		Target: schema.WorkloadRef{Kind: "Service.serving.knative.dev", Namespace: "shop", Name: "fn" + esc},
		Reason: "no reader for the kind Service" + esc,
	})
	report.Metadata.RuntimeClassName = "gvisor" + esc
	r := &report.Spec.Results[0]
	r.Target.Name = "canary" + esc
	r.Status = schema.VerifyStatusMismatch
	r.Message = "pod canary-1" + esc + " has no class"
	report.Spec.Summary = schema.VerifySummary{Total: 1, Mismatch: 1, NotChecked: 1}

	out := renderToString(t, report)
	if strings.ContainsRune(out, '\x1b') {
		t.Errorf("the table has an escape character from the input:\n%q", out)
	}
	for _, want := range []string{"fn\uFFFD[2K", "canary\uFFFD[2K", "NOT OK\n  1  Rollout.argoproj.io/shop/canary\uFFFD[2K"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%q", want, out)
		}
	}
}

// TestVerifyMarkdown_SizeOfALargeHostileReport: a job summary on GitHub is
// limited to 1 MiB for each step. Each row of the not-checked table has a
// reason with a kind from a manifest in it, so the table needs a limit on
// its rows and on each cell. A report with many more workloads than a table
// shows, with long names in characters that grow when they are escaped,
// must stay far below the limit.
func TestVerifyMarkdown_SizeOfALargeHostileReport(t *testing.T) {
	t.Parallel()
	long := func(tag string, i int) string {
		return fmt.Sprintf("%s-%05d-", tag, i) + strings.Repeat("&<|`*", 200)
	}
	const workloads = 4 * mdMaxRows
	report := schema.NewVerifyReport()
	report.Metadata = schema.VerifyMetadata{
		Cluster: long("cluster", 0), RuntimeClassName: long("class", 0),
		Source: &schema.ReportSource{Type: schema.SourceManifests, Inputs: workloads},
	}
	for i := 0; i < workloads; i++ {
		ref := schema.WorkloadRef{Kind: long("kind", i), Namespace: long("ns", i), Name: long("name", i)}
		origin := &schema.Origin{File: long("file", i), Line: i + 1}
		report.Spec.NotChecked = append(report.Spec.NotChecked, schema.VerifyNotChecked{
			Target: ref, Origin: origin, Reason: "verify has no reader for the kind " + long("kind", i) + long("reason", i),
		})
		report.Spec.Results = append(report.Spec.Results, schema.VerifyResult{
			Order: i + 1, Target: ref, Origin: origin, Status: schema.VerifyStatusMismatch,
			Expected: long("expected", i), Actual: long("actual", i), Message: long("message", i),
		})
	}
	report.Spec.Summary = schema.VerifySummary{Total: workloads, Mismatch: workloads, NotChecked: workloads}

	out := renderMarkdown(t, report, nil)
	const limit = 1 << 20 // 1 MiB, the GitHub limit for one step
	if len(out) > limit/4 {
		t.Errorf("the report is %d bytes; want less than a quarter of the %d byte limit", len(out), limit)
	}
	t.Logf("a verify report of %d hostile workloads with no verdict and %d results is %d bytes", workloads, workloads, len(out))
	if want := fmt.Sprintf("_%d more not shown. The JSON output has all of them._", workloads-mdMaxRows); strings.Count(out, want) != 2 {
		t.Errorf("each of the two tables must say that it left out %d rows:\n%s", workloads-mdMaxRows, out[:min(len(out), 2000)])
	}
}

// TestVerifyMarkdown_ReasonsOfTheVerifierAreNotCut: the Markdown renderer
// cuts a cell at a fixed length. The reasons that pkg/verifier writes for a
// workload with no verdict end with what to do, and a cut would remove that
// part. This runs the verifier on targets whose namespace and name have the
// full length that Kubernetes permits (63 and 253 characters), and looks for
// the end of each reason in the rendered table.
func TestVerifyMarkdown_ReasonsOfTheVerifierAreNotCut(t *testing.T) {
	t.Parallel()
	namespace := strings.Repeat("n", 63)
	name := func(first string) string { return first + strings.Repeat("a", 252) }
	targets := []verifier.Target{
		{Ref: schema.WorkloadRef{Kind: "Job", Namespace: namespace, Name: name("j")}, Expected: "gvisor"},
		{Ref: schema.WorkloadRef{Kind: "Pod", Namespace: namespace, Name: name("p")}, Expected: "gvisor"},
		{
			Ref:      schema.WorkloadRef{Kind: "Rollout.argoproj.io", Namespace: namespace, Name: name("r")},
			Expected: "gvisor",
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
		},
	}
	report, err := verifier.Verify(context.Background(), verifier.Options{
		Client: fake.NewSimpleClientset(), Targets: targets, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Spec.NotChecked) != len(targets) {
		t.Fatalf("not checked = %+v, want %d entries", report.Spec.NotChecked, len(targets))
	}
	report.Metadata.Source = &schema.ReportSource{Type: schema.SourceManifests, Inputs: 1}
	report.Metadata.RuntimeClassName = "gvisor"
	out := renderMarkdown(t, report, nil)
	for _, n := range report.Spec.NotChecked {
		sentences := strings.Split(n.Reason, ". ")
		end := sentences[len(sentences)-1]
		if len(end) < 30 {
			t.Fatalf("%s: the last sentence of the reason is %q; the test needs a sentence to look for", n.Target.Kind, end)
		}
		t.Logf("%s: a reason of %d characters (the limit of a cell is %d)", n.Target.Kind, len(n.Reason), mdMaxSentence)
		// mdText escapes only characters that these sentences do not have.
		if strings.Count(out, end) == 0 {
			t.Errorf("%s: the reason of %d characters is cut in the Markdown; its end %q is not there",
				n.Target.Kind, len(n.Reason), end)
		}
	}
}
