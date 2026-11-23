// Package output: the renderers for a PatchResult (`agentmoat patch`).
//
// The table is for a terminal: the outcome of each selected workload, the
// workloads that were left out, and the diff of each file as `git diff`
// shows one. The Markdown form is for a job summary or the text of a pull
// request: it starts with what happened (were files changed, was something
// not patched), and the diff is in a fenced block.
//
// Everything a PatchResult shows comes from manifest files: names, paths,
// reasons with parser messages, and whole lines of the files in the diff.
// The Markdown form treats all of it as untrusted text, with the helpers of
// markdown.go: a code span for a value, mdText for a sentence, and a fenced
// block for the diff, in which nothing is read as Markdown. The size has a
// limit too: the diff is cut after mdMaxDiffLines lines, and a long line or
// reason is cut.
package output

import (
	"fmt"
	"io"
	"strings"
	"unicode"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// mdMaxDiffLines is the most lines of diff that the Markdown form shows, for
// all files together. A job summary has a size limit (1 MiB on GitHub), and
// a line is cut at mdMaxSentence characters, so this keeps the diff of any
// input below a quarter of the limit. The JSON output has the full diff.
const mdMaxDiffLines = 400

// mdMaxDiffWidth is the most characters of one line of the diff, and
// mdMaxReason the most characters of one reason or error in a patch
// document. They are less than mdMaxSentence: a patch document has four
// tables with a sentence in each row, and a diff, and all of it together
// must stay well below the size limit of a job summary. A line of a
// manifest and a reason of this project are much shorter than the two.
const (
	mdMaxDiffWidth = 240
	mdMaxReason    = 300
)

// ---------------------------------------------------------------------------
// Table.
// ---------------------------------------------------------------------------

// linePrinter writes lines and keeps the first error, so that a renderer
// with many lines has one error check at its end.
type linePrinter struct {
	w   io.Writer
	err error
}

func (p *linePrinter) line(text string) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, text)
	}
}

func (p *linePrinter) linef(format string, args ...any) {
	p.line(fmt.Sprintf(format, args...))
}

// renderPatchResultTable is the terminal view of a PatchResult.
func renderPatchResultTable(res *schema.PatchResult, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)
	sum := res.Spec.Summary
	p := &linePrinter{w: w}

	p.line(s.Bold.Render("agentmoat patch"))
	p.line(s.Muted.Render(fmt.Sprintf("runtime-class: %s   dry-run: %v", res.Metadata.RuntimeClassName, res.Metadata.DryRun)))

	p.linef("%s  %d workloads", s.Bold.Render("SUMMARY"), sum.Total)
	items := []barItem{
		{Symbol: symbolOK, Label: "patched", Count: sum.Patched, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolWarn, Label: "not-edited", Count: sum.NotEdited, Style: s.Warn, NoColorFill: '▓'},
		{Symbol: symbolFail, Label: "failed", Count: sum.Failed, Style: s.Danger, NoColorFill: '▒'},
		{Symbol: symbolDot, Label: "excluded", Count: sum.Excluded, Style: s.Muted, NoColorFill: '░'},
	}
	if chart := renderSummaryBar(items, summaryBarWidth(w), useColor); chart != "" {
		p.line(chart)
	}
	p.line("")
	if p.err != nil {
		return p.err
	}

	var tr trackingTruncator
	renderPatchChanges(p, s, res, &tr)
	renderPatchExcluded(p, s, res.Spec.Excluded, &tr)
	renderPatchSourceNotes(p, s, res.Metadata.Source)
	if p.err != nil {
		return p.err
	}
	// The hint about cut cells goes directly after the tables, before the
	// diff: after a long diff nobody connects it with the tables.
	if err := maybePrintTruncationHint(s, w, tr, "reasons"); err != nil {
		return err
	}
	renderPatchDiffs(p, s, res)
	return p.err
}

// renderPatchChanges prints one row for each selected workload.
func renderPatchChanges(p *linePrinter, s *Styles, res *schema.PatchResult, tr *trackingTruncator) {
	if len(res.Spec.Changes) == 0 {
		p.line(s.Muted.Render("(nothing to change: no workload is compatible and without a RuntimeClass)"))
		return
	}
	headers := []string{"NAMESPACE", "KIND", "NAME", "STATUS", "WHERE", "NOTE"}
	t := newBaseTable(s, headers, nil)
	for _, c := range res.Spec.Changes {
		t.Row(
			c.Target.Namespace,
			c.Target.Kind,
			tr.cell(c.Target.Name, maxNameWidth),
			StatusBadge(s, string(c.Status)),
			patchWhere(c),
			tr.cell(patchNote(c), maxFreeformWidth),
		)
	}
	p.line(t.Render())
}

// patchWhere is the place of a change: the file and the line of the added
// line for a patched workload, and the file and the line where the object
// starts for the others.
func patchWhere(c schema.PatchChange) string {
	if c.Origin == nil {
		return ""
	}
	line := c.Origin.Line
	if c.Status == schema.PatchStatusPatched && c.Line > 0 {
		line = c.Line
	}
	if line == 0 {
		return c.Origin.File
	}
	return fmt.Sprintf("%s:%d", c.Origin.File, line)
}

// patchNote is the note of a change: the reason when there is one, and for
// a workload that needs review, the fact that it was included.
func patchNote(c schema.PatchChange) string {
	if c.Reason != "" {
		return c.Reason
	}
	if c.Compatibility == schema.CompatibilityReview {
		return "needs review; included by --include-review"
	}
	return ""
}

// renderPatchExcluded prints the workloads that were not selected.
func renderPatchExcluded(p *linePrinter, s *Styles, excluded []schema.ExcludedWorkload, tr *trackingTruncator) {
	if len(excluded) == 0 {
		return
	}
	p.line("")
	p.line(s.Bold.Render("Excluded workloads:"))
	t := newBaseTable(s, []string{"NAMESPACE", "KIND", "NAME", "COMPATIBILITY", "REASON"}, nil)
	for _, e := range excluded {
		t.Row(
			e.Target.Namespace,
			e.Target.Kind,
			tr.cell(e.Target.Name, maxNameWidth),
			StatusBadge(s, string(e.Compatibility)),
			tr.cell(e.Reason, maxFreeformWidth),
		)
	}
	p.line(t.Render())
}

// renderPatchSourceNotes prints what the reader must know about the input:
// an object that is there two times, and input that is not rendered.
func renderPatchSourceNotes(p *linePrinter, s *Styles, src *schema.ReportSource) {
	if src == nil {
		return
	}
	for _, d := range src.Duplicates {
		p.linef("%s %s", s.Warn.Render(symbolWarn), d)
	}
	if len(src.Unrendered) == 0 {
		return
	}
	p.line(s.Warn.Render(symbolWarn + " not rendered: the verdicts describe the files, not what gets deployed (overlays are not applied)"))
	for _, sign := range src.Unrendered {
		p.line("    " + sign)
	}
	p.line(s.Muted.Render("    after the edit, scan the rendered objects with the gate: kustomize build <dir> | agentmoat scan -f - --fail-on sandboxed"))
}

// renderPatchDiffs prints the diff of each file, with the colors of `git
// diff` when color is on.
func renderPatchDiffs(p *linePrinter, s *Styles, res *schema.PatchResult) {
	if len(res.Spec.Files) == 0 {
		return
	}
	p.line("")
	title := "Diff:"
	if res.Metadata.DryRun {
		title = "Diff (dry-run: no file was written):"
	}
	p.line(s.Bold.Render(title))
	for _, f := range res.Spec.Files {
		if f.Error != "" {
			p.linef("%s %s was not written: %s", s.Danger.Render(symbolFail), f.Path, f.Error)
		}
		for _, line := range strings.Split(strings.TrimSuffix(f.Diff, "\n"), "\n") {
			p.line(colorDiffLine(s, termLine(line)))
		}
	}
}

// termLine makes one line of a diff safe to show on a terminal. A line of a
// diff is a line of a manifest file, so it is text that the author of a pull
// request controls. An escape sequence in it could move the cursor and
// write over the report (for example over the summary), and a bidirectional
// override could show the text in another order than it is in the file.
//
// The carriage return at the end of a line of a CRLF file is dropped. A tab
// becomes four spaces (the style library does the same when it renders a
// line, so this keeps the view the same with color and without). Each other
// control character, and each format character, becomes U+FFFD, so that a
// reader can see that something was there. The JSON and YAML outputs have
// the diff as it is.
func termLine(line string) string {
	line = strings.ReplaceAll(strings.TrimSuffix(line, "\r"), "\t", "    ")
	return strings.Map(func(r rune) rune {
		switch {
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return unicode.ReplacementChar
		}
		return r
	}, line)
}

// colorDiffLine gives one line of a unified diff the color that git gives
// it. With color off, the styles return the text as it is.
func colorDiffLine(s *Styles, line string) string {
	switch {
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return s.Bold.Render(line)
	case strings.HasPrefix(line, "@@"):
		return s.Info.Render(line)
	case strings.HasPrefix(line, "+"):
		return s.Success.Render(line)
	case strings.HasPrefix(line, "-"):
		return s.Danger.Render(line)
	}
	return line
}

// ---------------------------------------------------------------------------
// Markdown.
// ---------------------------------------------------------------------------

// writePatchResultMarkdown renders a PatchResult for a job summary or a
// pull request.
func writePatchResultMarkdown(d *mdDoc, res *schema.PatchResult) {
	sum := res.Spec.Summary
	title := "agentmoat patch"
	if res.Metadata.DryRun {
		title += " (dry run)"
	}
	d.heading(2, title)

	// Say first what happened. A reader of a job summary wants to know two
	// things: were files changed, and is there something that the patch
	// could not do.
	d.linef("%s", patchHeadline(res))
	d.blank()
	chips := []string{mdChip("RuntimeClass", res.Metadata.RuntimeClassName)}
	if res.Metadata.IncludeReview {
		chips = append(chips, "Workloads that need review: included")
	}
	if src := res.Metadata.Source; src != nil && src.Inputs > 0 {
		chips = append(chips, fmt.Sprintf("Manifest inputs: %d", src.Inputs))
	}
	mdChips(d, chips...)

	d.table(
		[]string{"Workloads", "Selected", "Patched", "Not edited", "Failed", "Excluded"},
		map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true, 5: true},
		[][]string{{
			fmt.Sprint(sum.Total), fmt.Sprint(sum.Selected), fmt.Sprint(sum.Patched),
			fmt.Sprint(sum.NotEdited), fmt.Sprint(sum.Failed), fmt.Sprint(sum.Excluded),
		}},
	)

	writePatchSourceMarkdown(d, res.Metadata.Source)

	var attention, patched []schema.PatchChange
	for _, c := range res.Spec.Changes {
		if c.Status == schema.PatchStatusPatched {
			patched = append(patched, c)
		} else {
			attention = append(attention, c)
		}
	}
	if len(attention) > 0 {
		d.heading(3, fmt.Sprintf("Not patched (%d)", len(attention)))
		writePatchChangeTable(d, attention)
	}
	if len(patched) > 0 {
		title := "Patched"
		if res.Metadata.DryRun {
			title = "To patch"
		}
		d.heading(3, fmt.Sprintf("%s (%d)", title, len(patched)))
		writePatchChangeTable(d, patched)
	}
	writePatchDiffMarkdown(d, res)

	if len(res.Spec.Excluded) > 0 {
		d.details(fmt.Sprintf("Excluded (%d)", len(res.Spec.Excluded)))
		rows := make([][]string, 0, len(res.Spec.Excluded))
		for _, e := range res.Spec.Excluded {
			rows = append(rows, append(targetCells(e.Target), mdLabel(string(e.Compatibility)), mdText(mdShorten(e.Reason, mdMaxReason))))
		}
		d.table([]string{"Namespace", "Kind", "Name", "Verdict", "Reason"}, nil, rows)
		d.endDetails()
	}
}

// patchHeadline is the first line of the Markdown form.
func patchHeadline(res *schema.PatchResult) string {
	sum := res.Spec.Summary
	lines := 0
	written := 0
	for _, f := range res.Spec.Files {
		lines += f.Added
		if f.Written {
			written++
		}
	}
	problems := sum.NotEdited + sum.Failed
	switch {
	case sum.Selected == 0:
		return "➖ **Nothing to change.** No workload is compatible and without a RuntimeClass."
	case problems > 0 && sum.Patched == 0:
		return fmt.Sprintf("❌ **None of the %d selected workload(s) could be patched.** No file was changed.", sum.Selected)
	case problems > 0 && res.Metadata.DryRun:
		return fmt.Sprintf("⚠️ **Dry run: %d of %d selected workload(s) cannot be patched.** No file was changed. "+
			"For the others, %d line(s) would be added to %d file(s).", problems, sum.Selected, lines, sum.Files)
	case problems > 0:
		return fmt.Sprintf("⚠️ **%d of %d selected workload(s) were not patched.** %d of %d file(s) were written.",
			problems, sum.Selected, written, sum.Files)
	case res.Metadata.DryRun:
		return fmt.Sprintf("➖ **Dry run.** No file was changed. %d line(s) would be added to %d file(s). "+
			"Pass `--dry-run=false` to write them.", lines, sum.Files)
	}
	return fmt.Sprintf("✅ **%d line(s) added to %d file(s).** Each selected workload now asks for the RuntimeClass.", lines, sum.Files)
}

// writePatchSourceMarkdown says when the input is not rendered, or has an
// object two times. The verdicts then need a second look.
func writePatchSourceMarkdown(d *mdDoc, src *schema.ReportSource) {
	if src == nil {
		return
	}
	if len(src.Unrendered) > 0 {
		d.linef("⚠️ **The input is not rendered** (%d sign(s): kustomization files or partial objects). "+
			"The verdicts describe the files and not what gets deployed. After the edit, scan the rendered objects with the gate "+
			"(`--fail-on sandboxed`).", len(src.Unrendered))
		d.blank()
		d.fence(src.Unrendered)
	}
	if len(src.Duplicates) > 0 {
		d.linef("⚠️ **%d workload(s) are defined more than one time.**", len(src.Duplicates))
		d.blank()
		d.fence(src.Duplicates)
	}
}

// writePatchChangeTable writes one row for each selected workload.
func writePatchChangeTable(d *mdDoc, changes []schema.PatchChange) {
	rows := make([][]string, 0, len(changes))
	for _, c := range changes {
		where := ""
		if w := patchWhere(c); w != "" {
			where = mdCodeCell(w)
		}
		rows = append(rows, append(targetCells(c.Target), mdLabel(string(c.Status)), where, mdText(mdShorten(patchNote(c), mdMaxReason))))
	}
	d.table([]string{"Namespace", "Kind", "Name", "Status", "Where", "Note"}, nil, rows)
}

// writePatchDiffMarkdown writes the diff of the files in one fenced block.
// The block is a `diff` block, so GitHub and GitLab give the added lines a
// color. At most mdMaxDiffLines lines are written, and at most mdMaxRows
// files that were not written are named.
func writePatchDiffMarkdown(d *mdDoc, res *schema.PatchResult) {
	var lines []string
	var notWritten []schema.PatchFile
	for _, f := range res.Spec.Files {
		for _, line := range strings.Split(strings.TrimSuffix(f.Diff, "\n"), "\n") {
			lines = append(lines, mdShorten(diffLine(line), mdMaxDiffWidth))
		}
		if f.Error != "" {
			notWritten = append(notWritten, f)
		}
	}
	if len(lines) == 0 {
		return
	}
	title := "Diff"
	if res.Metadata.DryRun {
		title = "Diff (not written)"
	}
	d.heading(3, title)

	shown := notWritten
	if len(shown) > mdMaxRows {
		shown = shown[:mdMaxRows]
	}
	for _, f := range shown {
		d.linef("- ❌ %s was not written: %s", mdCode(f.Path), mdText(mdShorten(f.Error, mdMaxReason)))
	}
	if len(shown) > 0 {
		d.blank()
		d.leftOut(len(notWritten) - len(shown))
	}
	d.fenceAs("diff", lines, mdMaxDiffLines)
}

// diffLine returns one line of a unified diff for the fenced block. Each
// line of a diff starts with a marker (a space, "+", "-", "@", or "\"), and
// the text of a file comes after it. A line that has no marker is not from
// the diff writer of this project; it gets a space, so that no line of the
// document starts with text from a file, also inside the block.
func diffLine(line string) string {
	if line == "" || strings.ContainsRune(" +-@\\", rune(line[0])) {
		return line
	}
	return " " + line
}
