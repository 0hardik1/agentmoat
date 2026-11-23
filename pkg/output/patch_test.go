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
	const n = 12 * mdMaxRows
	// The two forms of a result. The edit in place has few lines for each
	// file. A Kustomize component has one new file for each workload: many
	// small diffs, a patch file in each row, and the directory in the
	// first lines.
	for _, kustomize := range []bool{false, true} {
		res := schema.NewPatchResult()
		res.Metadata = schema.PatchMetadata{DryRun: true, RuntimeClassName: long("class", 0), Source: &schema.ReportSource{Type: schema.SourceManifests, Inputs: 600}}
		if kustomize {
			res.Metadata.KustomizeDir = long("dir", 0)
		}
		for i := 0; i < n; i++ {
			res.Metadata.Source.Unrendered = append(res.Metadata.Source.Unrendered, long("unrendered", i))
			res.Metadata.Source.Duplicates = append(res.Metadata.Source.Duplicates, long("duplicate", i))
			status := []schema.PatchStatus{schema.PatchStatusPatched, schema.PatchStatusNotEdited, schema.PatchStatusFailed}[i%3]
			change := schema.PatchChange{
				Target: schema.WorkloadRef{Kind: long("kind", i), Namespace: long("ns", i), Name: long("name", i)},
				Origin: &schema.Origin{File: long("file", i), Line: i + 1}, Status: status, Line: i + 2, Reason: long("reason", i),
			}
			file := schema.PatchFile{Path: long("path", i), Added: 1, Error: long("error", i)}
			file.Diff = "--- a/x\n+++ b/x\n@@ -1,8 +1,9 @@\n"
			if kustomize {
				change.PatchFile, change.Line = long("patchfile", i), 0
				file.Created, file.Added = true, 8
				file.Diff = "--- /dev/null\n+++ b/x\n@@ -0,0 +1,8 @@\n"
			}
			for l := 0; l < 8; l++ {
				file.Diff += map[bool]string{false: " ", true: "+"}[kustomize] + long("line", i*10+l) + "\n"
			}
			res.Spec.Changes = append(res.Spec.Changes, change)
			res.Spec.Excluded = append(res.Spec.Excluded, schema.ExcludedWorkload{
				Target: schema.WorkloadRef{Kind: long("kind", i), Namespace: long("ns", i), Name: long("name", i)},
				Reason: long("xreason", i),
			})
			res.Spec.Files = append(res.Spec.Files, file)
		}
		res.Spec.Summary = schema.PatchSummary{Total: 2 * n, Selected: n, Patched: n / 3, NotEdited: n / 3, Failed: n / 3, Excluded: n, Files: n}

		out := renderMarkdown(t, res, nil)
		const limit = 1 << 20
		if len(out) > limit/2 {
			t.Errorf("kustomize %v: the document is %d bytes; want less than half of the %d byte limit", kustomize, len(out), limit)
		}
		t.Logf("kustomize %v: a patch result of %d hostile workloads and %d files is %d bytes", kustomize, n, n, len(out))

		// The diff is cut, and the document says how much is not shown.
		total := n * 11 // each file has 3 header lines and 8 content lines
		if want := fmt.Sprintf("_%d more not shown. The JSON output has all of them._", total-mdMaxDiffLines); !strings.Contains(out, want) {
			t.Errorf("kustomize %v: the document lacks %q", kustomize, want)
		}
	}
}

// ---------------------------------------------------------------------------
// The Kustomize form (PatchMetadata.KustomizeDir).
// ---------------------------------------------------------------------------

// componentDiff is the diff of the two new files of a component.
const componentDiff = "--- /dev/null\n+++ b/overlays/prod/gvisor/deployment.apps_shop_web.yaml\n@@ -0,0 +1,2 @@\n+# Generated by `agentmoat patch --kustomize`.\n+apiVersion: apps/v1\n"

// kustomizeResult builds the result of a run with a component directory:
// one workload with a patch file, one that can have none, and one that the
// component has already.
func kustomizeResult(dryRun bool) *schema.PatchResult {
	res := schema.NewPatchResult()
	res.Metadata = schema.PatchMetadata{
		DryRun: dryRun, RuntimeClassName: "gvisor", KustomizeDir: "overlays/prod/gvisor",
		Source: &schema.ReportSource{Type: schema.SourceManifests, Inputs: 3},
	}
	res.Spec = schema.PatchSpec{
		Summary: schema.PatchSummary{Total: 3, Selected: 2, Patched: 1, NotEdited: 1, Excluded: 1, Files: 2},
		Changes: []schema.PatchChange{
			{
				Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "web"}, Compatibility: schema.CompatibilityCompatible,
				Origin: &schema.Origin{File: "base/web.yaml", Line: 1}, Status: schema.PatchStatusPatched,
				PatchFile: "overlays/prod/gvisor/deployment.apps_shop_web.yaml",
			},
			{
				Target: schema.WorkloadRef{Kind: "Job", Namespace: "shop", Name: "run-"}, Compatibility: schema.CompatibilityCompatible,
				Origin: &schema.Origin{File: "base/job.yaml", Line: 4}, Status: schema.PatchStatusNotEdited,
				Reason: "the object has generateName and no metadata.name",
			},
		},
		Excluded: []schema.ExcludedWorkload{{
			Target:        schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "api"},
			Compatibility: schema.CompatibilityCompatible,
			Reason:        "the component has the patch for it already (overlays/prod/gvisor/deployment.apps_shop_api.yaml)",
		}},
		Files: []schema.PatchFile{
			{Path: "overlays/prod/gvisor/deployment.apps_shop_web.yaml", Added: 2, Created: true, Written: !dryRun, Diff: componentDiff},
			{Path: "overlays/prod/gvisor/kustomization.yaml", Added: 1, Written: !dryRun, Diff: "--- a/overlays/prod/gvisor/kustomization.yaml\n+++ b/overlays/prod/gvisor/kustomization.yaml\n@@ -8,1 +8,2 @@\n - path: deployment.apps_shop_api.yaml\n+- path: deployment.apps_shop_web.yaml\n"},
		},
	}
	return res
}

func TestRenderPatchResultTable_Kustomize(t *testing.T) {
	t.Parallel()
	out := renderToString(t, kustomizeResult(true))
	for _, want := range []string{
		"runtime-class: gvisor   dry-run: true   kustomize component: overlays/prod/gvisor\n",
		// The place of a patched workload is its patch file. The place of
		// one with no patch is its source.
		"overlays/prod/gvisor/deployment.apps_shop_web.yaml",
		"base/job.yaml:4",
		"→ to use the component, add the directory overlays/prod/gvisor to `components:` in the kustomization that must move\n",
		"    give patch only what that kustomization reads from its `resources:`: a patch for an object that it does not read stops its build\n",
		"Diff (dry-run: no file was written):\n--- /dev/null\n+++ b/overlays/prod/gvisor/deployment.apps_shop_web.yaml\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the table lacks %q:\n%s", want, out)
		}
	}
	// The next step comes before the diff: after a long diff nobody reads
	// it.
	if strings.Index(out, "to use the component") > strings.Index(out, "Diff (dry-run") {
		t.Errorf("the next step is after the diff:\n%s", out)
	}

	// Nothing has a new patch: no next step, and the reason names the
	// component.
	done := kustomizeResult(true)
	done.Spec.Summary = schema.PatchSummary{Total: 1, Excluded: 1}
	done.Spec.Changes, done.Spec.Files = nil, nil
	out = renderToString(t, done)
	if !strings.Contains(out, "(nothing to change: no workload is compatible, without a RuntimeClass, and without its patch in the component)") || strings.Contains(out, "to use the component") {
		t.Errorf("a run with nothing to change:\n%s", out)
	}

	// The directory is an argument of the command. It is printed, so a
	// control character in it does not reach the terminal.
	hostileDir := kustomizeResult(true)
	hostileDir.Metadata.KustomizeDir = "over\x1b[2Jlay"
	hostileDir.Spec.Changes[0].PatchFile = "over\x1b[2Jlay/x.yaml"
	if out := renderToString(t, hostileDir); strings.Contains(out, "\x1b") || !strings.Contains(out, "over\ufffd[2Jlay/x.yaml") {
		t.Errorf("a control character of the directory is in the table:\n%q", out)
	}
}

// TestPatchMarkdown_KustomizeHeadline: the first line for each outcome of a
// run with a component directory. It counts files, and it says that no
// source file changes.
func TestPatchMarkdown_KustomizeHeadline(t *testing.T) {
	t.Parallel()
	planned := []schema.PatchFile{{Path: "g/a.yaml", Added: 2, Created: true, Diff: componentDiff}, {Path: "g/kustomization.yaml", Added: 9, Created: true, Diff: componentDiff}}
	written := []schema.PatchFile{{Path: "g/a.yaml", Added: 2, Created: true, Written: true, Diff: componentDiff}, {Path: "g/kustomization.yaml", Added: 9, Created: true, Written: true, Diff: componentDiff}}
	onlyPatch := []schema.PatchFile{{Path: "g/a.yaml", Added: 2, Created: true, Written: true, Diff: componentDiff}, {Path: "g/kustomization.yaml", Added: 9, Created: true, Error: "changed after it was read", Diff: componentDiff}}
	cases := []struct {
		name   string
		dryRun bool
		sum    schema.PatchSummary
		files  []schema.PatchFile
		want   string
	}{
		{"nothing selected", true, schema.PatchSummary{Total: 4, Excluded: 4}, nil,
			"➖ **Nothing to change.** No workload is compatible, without a RuntimeClass, and without its patch in the Kustomize component `g`."},
		{"a dry-run", true, schema.PatchSummary{Total: 1, Selected: 1, Patched: 1, Files: 2}, planned,
			"➖ **Dry run.** No file was changed. 2 file(s) would be written in the Kustomize component `g`, and no source file changes. Pass `--dry-run=false` to write them."},
		{"a real run", false, schema.PatchSummary{Total: 1, Selected: 1, Patched: 1, Files: 2}, written,
			"✅ **2 file(s) written in the Kustomize component `g`.** No source file changed. The component has a patch for each selected workload."},
		{"a dry-run that is partial", true, schema.PatchSummary{Total: 2, Selected: 2, Patched: 1, NotEdited: 1, Files: 2}, planned,
			"⚠️ **Dry run: 1 of 2 selected workload(s) cannot get a patch.** No file was changed. For the others, 2 file(s) would be written in the Kustomize component `g`."},
		{"a real run that is partial", false, schema.PatchSummary{Total: 2, Selected: 2, Patched: 1, NotEdited: 1, Files: 2}, written,
			"⚠️ **1 of 2 selected workload(s) did not get a patch.** 2 of 2 file(s) were written in the Kustomize component `g`."},
		// The patch file is there and the list could not be written: a
		// file was written, and no workload has a patch that a build uses.
		{"the list was not written", false, schema.PatchSummary{Total: 1, Selected: 1, Failed: 1, Files: 2}, onlyPatch,
			"❌ **None of the 1 selected workload(s) got a patch that a build uses.** 1 of 2 file(s) were written in the Kustomize component `g`."},
		{"nothing can get a patch", true, schema.PatchSummary{Total: 1, Selected: 1, NotEdited: 1}, nil,
			"❌ **None of the 1 selected workload(s) could get a patch.** No file was changed."},
		{"nothing was written", false, schema.PatchSummary{Total: 1, Selected: 1, Failed: 1, Files: 1}, []schema.PatchFile{{Path: "g/a.yaml", Created: true, Error: "permission denied", Diff: componentDiff}},
			"❌ **None of the 1 selected workload(s) could get a patch.** No file was changed."},
	}
	for _, tc := range cases {
		res := schema.NewPatchResult()
		res.Metadata = schema.PatchMetadata{DryRun: tc.dryRun, RuntimeClassName: "gvisor", KustomizeDir: "g"}
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

func TestPatchMarkdown_Kustomize(t *testing.T) {
	t.Parallel()
	out := renderMarkdown(t, kustomizeResult(true), nil)
	for _, want := range []string{
		"RuntimeClass: `gvisor` · Kustomize component: `overlays/prod/gvisor` · Manifest inputs: 3",
		"**To use the component:** add the directory `overlays/prod/gvisor` to `components:` in the kustomization that must move. " +
			"Give patch only what that kustomization reads from its `resources:`: a patch for an object that it does not read stops its build.\n",
		"| `shop` | `Deployment` | `web` | patched | `overlays/prod/gvisor/deployment.apps_shop_web.yaml` |  |",
		"| `shop` | `Job` | `run-` | not-edited | `base/job.yaml:4` | the object has generateName and no metadata.name |",
		"### Diff (not written)\n\n```diff\n--- /dev/null\n+++ b/overlays/prod/gvisor/deployment.apps_shop_web.yaml\n@@ -0,0 +1,2 @@\n",
		"| `shop` | `Deployment` | `api` | compatible | the component has the patch for it already (overlays/prod/gvisor/deployment.apps\\_shop\\_api.yaml) |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}

	// No new patch, so no next step.
	done := kustomizeResult(false)
	done.Spec.Summary = schema.PatchSummary{Total: 1, Excluded: 1}
	done.Spec.Changes, done.Spec.Files = nil, nil
	if out := renderMarkdown(t, done, nil); strings.Contains(out, "To use the component") {
		t.Errorf("a run with nothing to change has a next step:\n%s", out)
	}
}

// TestPatchMarkdown_KustomizeHostileInput: the directory and the patch file
// of each change are in the first lines and in the tables. With Markdown in
// them, the document still has only the structure of the renderer.
func TestPatchMarkdown_KustomizeHostileInput(t *testing.T) {
	t.Parallel()
	res := kustomizeResult(true)
	res.Metadata.KustomizeDir = hostile("dir")
	res.Spec.Changes[0].PatchFile = hostile("patchfile")
	res.Spec.Changes[1].Reason = hostile("reason")
	res.Spec.Excluded[0].Reason = hostile("xreason")
	res.Spec.Files[0].Path = hostile("path")
	res.Spec.Files[0].Error = hostile("error")
	res.Spec.Files[0].Diff = "--- /dev/null\n+++ b/x\n@@ -0,0 +1,3 @@\n+```\n+" + hostile("line") + "\n+## Forged heading\n"
	out := renderMarkdown(t, res, nil)
	assertInert(t, out, []string{
		"## agentmoat patch (dry run)",
		"### Not patched (1)",
		"### To patch (1)",
		"### Diff (not written)",
	})
}
