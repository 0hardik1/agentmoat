// Tests for the PatchResult renderers (patch.go).
//
// The Markdown form is the one that a pipeline shows, so most checks are for
// it: the first line says what happened for each outcome, text from the
// manifests cannot add structure to the document, and the size has a limit.
package output

import (
	"fmt"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// patchDiff is the diff of a file with one added line.
const patchDiff = "--- a/k8s/web.yaml\n+++ b/k8s/web.yaml\n@@ -7,6 +7,7 @@\n   template:\n     spec:\n+      runtimeClassName: gvisor\n       containers:\n"

// patchResult builds a PatchResult with one patched workload, one that is
// not edited, and one that is excluded.
func patchResult(dryRun bool) *schema.PatchResult {
	res := schema.NewPatchResult()
	res.Metadata = schema.PatchMetadata{
		DryRun: dryRun, RuntimeClassName: "gvisor",
		Source: &schema.ReportSource{Type: schema.SourceManifests, Inputs: 3},
	}
	res.Spec = schema.PatchSpec{
		Summary: schema.PatchSummary{Total: 3, Selected: 2, Patched: 1, NotEdited: 1, Excluded: 1, Files: 1},
		Changes: []schema.PatchChange{
			{
				Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "web"}, Compatibility: schema.CompatibilityCompatible,
				Origin: &schema.Origin{File: "k8s/web.yaml", Line: 1}, Status: schema.PatchStatusPatched, Line: 9,
			},
			{
				Target: schema.WorkloadRef{Kind: "Pod", Namespace: "shop", Name: "once"}, Compatibility: schema.CompatibilityCompatible,
				Origin: &schema.Origin{File: "k8s/once.json", Line: 1}, Status: schema.PatchStatusNotEdited,
				Reason: "JSON is not edited: add \"runtimeClassName\" to the pod spec by hand",
			},
		},
		Excluded: []schema.ExcludedWorkload{{
			Target:        schema.WorkloadRef{Kind: "DaemonSet", Namespace: "kube-system", Name: "cni"},
			Compatibility: schema.CompatibilityIncompatible, Reason: "workload is incompatible: host-network",
		}},
		Files: []schema.PatchFile{{Path: "k8s/web.yaml", Added: 1, Written: !dryRun, Diff: patchDiff}},
	}
	return res
}

func TestRenderPatchResultTable(t *testing.T) {
	t.Parallel()
	out := renderToString(t, patchResult(true))
	for _, want := range []string{
		"agentmoat patch\n",
		"runtime-class: gvisor   dry-run: true",
		"SUMMARY  3 workloads",
		"patched", "not-edited", "excluded",
		"k8s/web.yaml:9", // the line of the added line
		"k8s/once.json:1",
		"JSON is not edited",
		"Excluded workloads:",
		"workload is incompatible: host-network",
		"Diff (dry-run: no file was written):",
		"+      runtimeClassName: gvisor",
		"@@ -7,6 +7,7 @@",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the table lacks %q:\n%s", want, out)
		}
	}

	written := renderToString(t, patchResult(false))
	if !strings.Contains(written, "dry-run: false") || !strings.Contains(written, "\nDiff:\n") {
		t.Errorf("a real run must say so and show the diff:\n%s", written)
	}

	empty := schema.NewPatchResult()
	empty.Metadata.RuntimeClassName = "gvisor"
	if out := renderToString(t, empty); !strings.Contains(out, "nothing to change") || strings.Contains(out, "Diff") {
		t.Errorf("an empty result must say that there is nothing to change, with no diff:\n%s", out)
	}

	failed := patchResult(false)
	failed.Spec.Files[0].Written = false
	failed.Spec.Files[0].Error = "permission denied"
	if out := renderToString(t, failed); !strings.Contains(out, "k8s/web.yaml was not written: permission denied") {
		t.Errorf("a file that was not written must be named with the error:\n%s", out)
	}
}

// TestRenderPatchResultTable_DiffLinesAreSafeOnATerminal: a line of a diff
// is a line of a manifest. An escape sequence or a bidirectional override in
// it must not reach the terminal.
func TestRenderPatchResultTable_DiffLinesAreSafeOnATerminal(t *testing.T) {
	t.Parallel()
	res := patchResult(true)
	res.Spec.Files[0].Diff = "--- a/x.yaml\n+++ b/x.yaml\n@@ -1,3 +1,4 @@\n" +
		" # \x1b[2J\x1b[H forged\r\n" + // clear the screen, then a CRLF line ending
		" # a\rb \u202e reversed\n" + // a carriage return in the line, a right-to-left override
		"+\truntimeClassName: gvisor\n" // a tab becomes four spaces
	out := renderToString(t, res)
	for _, bad := range []string{"\x1b", "\r", "\u202e", "\t"} {
		if strings.Contains(out, bad) {
			t.Errorf("the table has the control character %q", bad)
		}
	}
	for _, want := range []string{" # \ufffd[2J\ufffd[H forged\n", " # a\ufffdb \ufffd reversed\n", "+    runtimeClassName: gvisor\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the table lacks %q:\n%q", want, out)
		}
	}
}

// TestPatchMarkdown_Headline: the first line after the title says what
// happened, for each outcome. A reader of a job summary sees no exit code.
func TestPatchMarkdown_Headline(t *testing.T) {
	t.Parallel()
	one := []schema.PatchFile{{Path: "a.yaml", Added: 2, Diff: patchDiff}}
	oneWritten := []schema.PatchFile{{Path: "a.yaml", Added: 2, Written: true, Diff: patchDiff}}
	cases := []struct {
		name   string
		dryRun bool
		sum    schema.PatchSummary
		files  []schema.PatchFile
		want   string
	}{
		{"nothing selected", true, schema.PatchSummary{Total: 4, Excluded: 4}, nil,
			"➖ **Nothing to change.** No workload is compatible and without a RuntimeClass."},
		{"a dry-run", true, schema.PatchSummary{Total: 2, Selected: 2, Patched: 2, Files: 1}, one,
			"➖ **Dry run.** No file was changed. 2 line(s) would be added to 1 file(s). Pass `--dry-run=false` to write them."},
		{"a real run", false, schema.PatchSummary{Total: 2, Selected: 2, Patched: 2, Files: 1}, oneWritten,
			"✅ **2 line(s) added to 1 file(s).** Each selected workload now asks for the RuntimeClass."},
		{"a dry-run that is partial", true, schema.PatchSummary{Total: 3, Selected: 3, Patched: 2, NotEdited: 1, Files: 1}, one,
			"⚠️ **Dry run: 1 of 3 selected workload(s) cannot be patched.** No file was changed. For the others, 2 line(s) would be added to 1 file(s)."},
		{"a real run that is partial", false, schema.PatchSummary{Total: 3, Selected: 3, Patched: 2, Failed: 1, Files: 1}, oneWritten,
			"⚠️ **1 of 3 selected workload(s) were not patched.** 1 of 1 file(s) were written."},
		{"nothing could be patched", false, schema.PatchSummary{Total: 2, Selected: 2, NotEdited: 2}, nil,
			"❌ **None of the 2 selected workload(s) could be patched.** No file was changed."},
	}
	for _, tc := range cases {
		res := schema.NewPatchResult()
		res.Metadata = schema.PatchMetadata{DryRun: tc.dryRun, RuntimeClassName: "gvisor"}
		res.Spec = schema.PatchSpec{Summary: tc.sum, Files: tc.files}
		out := renderMarkdown(t, res, nil)
		title := "## agentmoat patch\n\n"
		if tc.dryRun {
			title = "## agentmoat patch (dry run)\n\n"
		}
		if !strings.HasPrefix(out, title+tc.want+"\n") {
			t.Errorf("%s: the document starts\n%s\nwant\n%s", tc.name, firstLines(out, 3), title+tc.want)
		}
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestPatchMarkdown(t *testing.T) {
	t.Parallel()
	out := renderMarkdown(t, patchResult(true), nil)
	for _, want := range []string{
		"RuntimeClass: `gvisor` · Manifest inputs: 3",
		"| Workloads | Selected | Patched | Not edited | Failed | Excluded |",
		"| 3 | 2 | 1 | 1 | 0 | 1 |",
		"### Not patched (1)\n",
		"| `shop` | `Pod` | `once` | not-edited | `k8s/once.json:1` | JSON is not edited",
		"### To patch (1)\n",
		"| `shop` | `Deployment` | `web` | patched | `k8s/web.yaml:9` |  |",
		"### Diff (not written)\n\n```diff\n--- a/k8s/web.yaml\n+++ b/k8s/web.yaml\n@@ -7,6 +7,7 @@\n",
		"+      runtimeClassName: gvisor\n",
		"<summary>Excluded (1)</summary>",
		"| `kube-system` | `DaemonSet` | `cni` | incompatible | workload is incompatible: host-network |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}

	written := renderMarkdown(t, patchResult(false), nil)
	if !strings.Contains(written, "### Patched (1)\n") || !strings.Contains(written, "### Diff\n\n```diff\n") {
		t.Errorf("a real run has the headings \"Patched\" and \"Diff\":\n%s", written)
	}

	review := patchResult(true)
	review.Metadata.IncludeReview = true
	review.Spec.Changes[0].Compatibility = schema.CompatibilityReview
	out = renderMarkdown(t, review, nil)
	if !strings.Contains(out, "Workloads that need review: included") || !strings.Contains(out, "needs review; included by --include-review") {
		t.Errorf("a result with --include-review must say so, and mark the workload:\n%s", out)
	}
}

// TestPatchMarkdown_HostileInput: every string of the result that comes from
// a manifest holds Markdown, HTML, links, and line breaks. None of it is
// read as Markdown: the headings are the ones of the renderer, each table
// row has the cells of its header, and the diff stays in its fenced block
// although its lines have runs of backticks.
func TestPatchMarkdown_HostileInput(t *testing.T) {
	t.Parallel()
	res := schema.NewPatchResult()
	res.Metadata = schema.PatchMetadata{
		DryRun: true, RuntimeClassName: hostile("class"), IncludeReview: true,
		Source: &schema.ReportSource{
			Type: schema.SourceManifests, Inputs: 2,
			Unrendered: []string{hostile("unrendered")},
			Duplicates: []string{hostile("duplicate")},
		},
	}
	res.Spec = schema.PatchSpec{
		Summary: schema.PatchSummary{Total: 3, Selected: 2, Patched: 1, NotEdited: 1, Excluded: 1, Files: 1},
		Changes: []schema.PatchChange{
			{
				Target:        schema.WorkloadRef{Kind: hostile("kind"), Namespace: hostile("ns"), Name: hostile("name")},
				Compatibility: schema.CompatibilityReview,
				Origin:        &schema.Origin{File: hostile("file"), Line: 3}, Status: schema.PatchStatusPatched, Line: 7,
			},
			{
				Target:        schema.WorkloadRef{Kind: hostile("kind2"), Namespace: hostile("ns2"), Name: hostile("name2")},
				Compatibility: schema.Compatibility(hostile("verdict")),
				Origin:        &schema.Origin{File: hostile("file2")}, Status: schema.PatchStatus(hostile("status")),
				Reason: hostile("reason"),
			},
		},
		Excluded: []schema.ExcludedWorkload{{
			Target:        schema.WorkloadRef{Kind: hostile("xkind"), Namespace: hostile("xns"), Name: hostile("xname")},
			Compatibility: schema.Compatibility(hostile("xverdict")), Reason: hostile("xreason"),
		}},
		Files: []schema.PatchFile{{
			Path: hostile("path"), Added: 1, Error: hostile("error"),
			// A diff whose lines try to close the fence and start a heading.
			Diff: "--- a/x\n+++ b/x\n@@ -1,2 +1,3 @@\n ```\n ````\n+" + hostile("line") + "\n ## Forged heading\n",
		}},
	}
	out := renderMarkdown(t, res, nil)
	assertInert(t, out, []string{
		"## agentmoat patch (dry run)",
		"### Not patched (1)",
		"### To patch (1)",
		"### Diff (not written)",
	})
}

// TestPatchMarkdown_Size: a job summary on GitHub takes 1 MiB for a step. A
// result with many workloads and a long diff, with text that grows when it
// is escaped, stays below half of that, and says what it left out.
func TestPatchMarkdown_Size(t *testing.T) {
	t.Parallel()
	long := func(tag string, i int) string {
		return fmt.Sprintf("%s-%05d-", tag, i) + strings.Repeat("&<|`*", 200)
	}
	res := schema.NewPatchResult()
	res.Metadata = schema.PatchMetadata{DryRun: true, RuntimeClassName: long("class", 0), Source: &schema.ReportSource{Type: schema.SourceManifests, Inputs: 600}}
	const n = 12 * mdMaxRows
	for i := 0; i < n; i++ {
		res.Metadata.Source.Unrendered = append(res.Metadata.Source.Unrendered, long("unrendered", i))
		res.Metadata.Source.Duplicates = append(res.Metadata.Source.Duplicates, long("duplicate", i))
		status := []schema.PatchStatus{schema.PatchStatusPatched, schema.PatchStatusNotEdited, schema.PatchStatusFailed}[i%3]
		res.Spec.Changes = append(res.Spec.Changes, schema.PatchChange{
			Target: schema.WorkloadRef{Kind: long("kind", i), Namespace: long("ns", i), Name: long("name", i)},
			Origin: &schema.Origin{File: long("file", i), Line: i + 1}, Status: status, Line: i + 2, Reason: long("reason", i),
		})
		res.Spec.Excluded = append(res.Spec.Excluded, schema.ExcludedWorkload{
			Target: schema.WorkloadRef{Kind: long("kind", i), Namespace: long("ns", i), Name: long("name", i)},
			Reason: long("xreason", i),
		})
		diff := "--- a/x\n+++ b/x\n@@ -1,8 +1,9 @@\n"
		for l := 0; l < 8; l++ {
			diff += " " + long("line", i*10+l) + "\n"
		}
		res.Spec.Files = append(res.Spec.Files, schema.PatchFile{Path: long("path", i), Added: 1, Diff: diff, Error: long("error", i)})
	}
	res.Spec.Summary = schema.PatchSummary{Total: 2 * n, Selected: n, Patched: n / 3, NotEdited: n / 3, Failed: n / 3, Excluded: n, Files: n}

	out := renderMarkdown(t, res, nil)
	const limit = 1 << 20
	if len(out) > limit/2 {
		t.Errorf("the document is %d bytes; want less than half of the %d byte limit", len(out), limit)
	}
	t.Logf("a patch result of %d hostile workloads and %d files is %d bytes", n, n, len(out))

	// The diff is cut, and the document says how much is not shown.
	total := n * 11 // each file has 3 header lines and 8 content lines
	if want := fmt.Sprintf("_%d more not shown. The JSON output has all of them._", total-mdMaxDiffLines); !strings.Contains(out, want) {
		t.Errorf("the document lacks %q", want)
	}
}
