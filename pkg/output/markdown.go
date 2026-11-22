// Package output: Markdown renderer (helpers, and the ScanReport view).
//
// Who reads this output
//
// A CI job summary, a pull-request comment, a merge-request note. In those
// places a person sees the rendered page and nothing else: no stderr, no
// exit code. So the Markdown form of a scan starts with the result of the
// gate (see Gate in renderer.go), then the numbers, then the workloads that
// need attention. The compatible workloads are in a collapsed block.
//
// Untrusted text
//
// A scan of manifests reads files that a pull request controls, and a pull
// request can come from a fork. Names, kinds, namespaces, file paths, and
// resource names in such files are strings that a stranger wrote, and
// `scan -f` does not check that they are valid Kubernetes names. Put into
// Markdown as they are, they could add a link, an image, a table row, or a
// line that says "Gate passed" to the report of the change that they are
// part of. This file gives every string from a document one of three
// treatments:
//
//   - A value (a name, a kind, a path, a rule ID) goes in a code span:
//     mdCode, or mdCodeCell in a table. Markdown and HTML are not read in a
//     code span.
//   - A sentence (a rule description, a gate summary, a warning) is escaped:
//     mdText. It reads as a sentence, and its punctuation has no effect. A
//     word in it that a renderer would link by itself (a bare URL, an email
//     address, an "@name") goes in a code span, because no escape for that
//     is the same on every renderer.
//   - A list of lines that hold values (gate details, the signs of
//     unrendered input) goes in a fenced block: mdDoc.fence.
//
// All three first replace control characters (mdScrub), because a newline
// ends a code span, a table row, and a paragraph.
//
// What this does not stop: a reference that the hosting service adds by its
// own rules to text that is plain to Markdown ("#123" on GitHub).
//
// The rules were checked against cmark-gfm, the parser that GitHub uses. To
// look at the documents that the tests render (the hostile ones too) in a
// real renderer, set AGENTMOAT_MD_DUMP to a directory and run the tests of
// this package.
//
// Size
//
// A job summary on GitHub is limited to 1 MiB for each step. The output is
// kept under that for any input: a table or a list shows at most mdMaxRows
// entries and then says how many it left out, and each value, sentence, and
// link has a length limit. The JSON output always has all of the entries. A
// test renders many more workloads than a table shows, with the longest
// strings, and checks the size.

package output

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/0hardik1/agentmoat/docs"
	"github.com/0hardik1/agentmoat/internal/schema"
)

const (
	// mdMaxRows is the most rows a table shows, and the most lines a list
	// or a fenced block shows.
	mdMaxRows = 50
	// mdMaxValue is the most characters of one value in a code span.
	mdMaxValue = 120
	// mdMaxSentence is the most characters of one escaped sentence.
	mdMaxSentence = 600
	// mdMaxLabel is the most characters of a severity, a status, or a
	// verdict. The longest real one is "already-applied".
	mdMaxLabel = 24
	// mdMaxRowRules is the most rules that one row of a workload table
	// names. The legend under the tables has all the rules.
	mdMaxRowRules = 6
	// mdMaxURL is the most characters of a link target.
	mdMaxURL = 300
)

// ruleDocBaseURL is where the per-rule explanation files are published.
// The Markdown legend and the SARIF helpUri link to <base><rule-id>.md.
const ruleDocBaseURL = "https://github.com/0hardik1/agentmoat/blob/main/docs/explanations/"

// ruleDocURL returns the link to a rule's explanation, or "" when the rule
// has no explanation file (a rule from a newer release).
func ruleDocURL(ruleID string) string {
	if docs.RuleExplanation(ruleID) == "" {
		return ""
	}
	return ruleDocBaseURL + ruleID + ".md"
}

// renderMarkdownAny dispatches the document on concrete type to the
// matching Markdown renderer. The kinds here must agree with
// formatKinds[FormatMarkdown]; a test checks that.
func renderMarkdownAny(doc any, w io.Writer, opts RenderOptions) error {
	var d mdDoc
	switch v := doc.(type) {
	case *schema.ScanReport:
		writeScanReportMarkdown(&d, v, opts.Gate)
	case *schema.MigrationPlan:
		writeMigrationPlanMarkdown(&d, v)
	case *schema.ApplyResult:
		writeStepsMarkdown(&d, "apply", v.Metadata, v.Spec.Summary, v.Spec.Steps, v.Spec.PreflightFindings)
	case *schema.RollbackResult:
		writeStepsMarkdown(&d, "rollback", v.Metadata, v.Spec.Summary, v.Spec.Steps, nil)
	case *schema.VerifyReport:
		writeVerifyReportMarkdown(&d, v)
	case *schema.PreflightReport:
		writePreflightReportMarkdown(&d, v)
	default:
		return fmt.Errorf("output: no Markdown renderer for %T", doc)
	}
	_, err := io.WriteString(w, d.b.String())
	return err
}

// ---------------------------------------------------------------------------
// The document builder.
// ---------------------------------------------------------------------------

// mdDoc collects a Markdown document in memory. The renderers write to it
// and renderMarkdownAny writes it out once, so a renderer has no error to
// check on each line.
type mdDoc struct {
	b strings.Builder
}

// linef writes one line.
func (d *mdDoc) linef(format string, args ...any) {
	fmt.Fprintf(&d.b, format, args...)
	d.b.WriteByte('\n')
}

// blank writes an empty line. Markdown needs one before and after a table,
// a list, a heading, and a fenced block.
func (d *mdDoc) blank() {
	d.b.WriteByte('\n')
}

// heading writes a heading with an empty line after it.
func (d *mdDoc) heading(level int, text string) {
	d.linef("%s %s", strings.Repeat("#", level), text)
	d.blank()
}

// table writes a GitHub-flavored Markdown table and an empty line after it.
// The cells must be escaped already (mdCodeCell, mdText, or a number).
// Columns in numeric are right-aligned. Only the first mdMaxRows rows are
// written; a line under the table says how many were left out.
func (d *mdDoc) table(headers []string, numeric map[int]bool, rows [][]string) {
	d.linef("| %s |", strings.Join(headers, " | "))
	seps := make([]string, len(headers))
	for i := range headers {
		seps[i] = "---"
		if numeric[i] {
			seps[i] = "---:"
		}
	}
	d.linef("| %s |", strings.Join(seps, " | "))
	shown := rows
	if len(shown) > mdMaxRows {
		shown = shown[:mdMaxRows]
	}
	for _, row := range shown {
		d.linef("| %s |", strings.Join(row, " | "))
	}
	d.blank()
	d.leftOut(len(rows) - len(shown))
}

// leftOut says how many entries a capped table or list did not show.
func (d *mdDoc) leftOut(n int) {
	if n <= 0 {
		return
	}
	d.linef("_%d more not shown. The JSON output has all of them._", n)
	d.blank()
}

// fence writes lines in a fenced code block, with an empty line after it.
// Nothing in a fenced block is read as Markdown or HTML. The fence is
// longer than the longest run of backticks in the lines, so no line can
// close it. Only the first mdMaxRows lines are written.
func (d *mdDoc) fence(lines []string) {
	shown := lines
	if len(shown) > mdMaxRows {
		shown = shown[:mdMaxRows]
	}
	clean := make([]string, len(shown))
	longest := 2
	for i, line := range shown {
		clean[i] = mdShorten(mdScrub(line), mdMaxSentence)
		if run := longestRun(clean[i], '`'); run > longest {
			longest = run
		}
	}
	ticks := strings.Repeat("`", longest+1)
	d.linef("%stext", ticks)
	for _, line := range clean {
		d.linef("%s", line)
	}
	d.linef("%s", ticks)
	d.blank()
	d.leftOut(len(lines) - len(shown))
}

// details opens a collapsed block. The summary text must be plain text that
// the caller controls. GitHub and GitLab read Markdown inside the block
// only when an empty line follows the summary.
func (d *mdDoc) details(summary string) {
	d.linef("<details>")
	d.linef("<summary>%s</summary>", summary)
	d.blank()
}

// endDetails closes a collapsed block.
func (d *mdDoc) endDetails() {
	d.linef("</details>")
	d.blank()
}

// ---------------------------------------------------------------------------
// Escaping. See "Untrusted text" in the file header.
// ---------------------------------------------------------------------------

// mdScrub replaces the characters that can break the structure of a
// Markdown document or change the order of the text a person sees. White
// space controls (newline, tab) become a space. Other control characters,
// format characters (the bidirectional overrides, zero-width characters),
// and the Unicode line and paragraph separators become U+FFFD, so that a
// reader can see that something was there.
func mdScrub(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f':
			return ' '
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
			return unicode.ReplacementChar
		}
		return r
	}, s)
}

// mdShorten cuts s to at most limit characters and marks the cut.
func mdShorten(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit-1]) + "…"
}

// longestRun returns the length of the longest run of r in s.
func longestRun(s string, r rune) int {
	longest, run := 0, 0
	for _, c := range s {
		if c != r {
			run = 0
			continue
		}
		run++
		if run > longest {
			longest = run
		}
	}
	return longest
}

// mdCode puts a value in a code span. An empty value is shown as "-".
//
// The delimiter is one backtick longer than the longest run of backticks in
// the value, which is how Markdown lets a code span hold backticks. A value
// that starts or ends with a backtick gets a space on each side (Markdown
// removes one such pair), so the delimiter and the value do not run
// together.
func mdCode(s string) string {
	s = mdShorten(strings.TrimSpace(mdScrub(s)), mdMaxValue)
	if s == "" {
		return "-"
	}
	ticks := strings.Repeat("`", longestRun(s, '`')+1)
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return ticks + s + ticks
}

// mdCodeCell is mdCode for a table cell. A table is split into cells on "|"
// before code spans are read, so a pipe in the value must be written "\|".
func mdCodeCell(s string) string {
	return mdEscapePipes(mdCode(s))
}

// mdEscapePipes writes each "|" in s so that no table parser reads it as the
// end of a cell: after it, the run of backslashes before each pipe is odd.
//
// The value can have backslashes of its own before a pipe, and parsers do
// not agree on what an even run means. cmark-gfm keeps any pipe that has a
// backslash before it in the cell. A parser that pairs backslashes reads
// "\\|" as an escaped backslash and then the end of the cell, which would
// put the rest of the value in the next cell as live Markdown. An odd run is
// an escaped pipe for both.
//
// The cost: a value with an odd run of its own (a name that holds "\|") is
// shown with one backslash less. No valid Kubernetes name has that.
func mdEscapePipes(s string) string {
	var b strings.Builder
	backslashes := 0
	for _, r := range s {
		if r == '|' && backslashes%2 == 0 {
			b.WriteByte('\\')
		}
		if r == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
		b.WriteRune(r)
	}
	return b.String()
}

// mdTextEscaper makes the Markdown and HTML punctuation in a sentence
// inert. strings.Replacer reads its input once and does not look at its own
// output again, so "&" to "&amp;" does not touch the "&" of "&lt;".
var mdTextEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`\`, `\\`,
	"`", "\\`",
	"*", `\*`,
	"_", `\_`,
	"[", `\[`,
	"]", `\]`,
	"|", `\|`,
	"~", `\~`,
	"#", `\#`,
	"!", `\!`,
)

// mdText escapes a sentence. The result is safe in a paragraph, a list
// item, and a table cell. Do not put it at the start of a line: a sentence
// that starts with "- " or "1. " would still open a list there.
//
// The sentence is read word by word (a word ends at a space or a pipe). A
// word that mdLinkLike accepts goes in a code span. All other text goes
// through mdTextEscaper.
func mdText(s string) string {
	s = mdShorten(strings.TrimSpace(mdScrub(s)), mdMaxSentence)
	var b strings.Builder
	for s != "" {
		end := strings.IndexAny(s, " |")
		if end < 0 {
			end = len(s)
		}
		if word := s[:end]; mdLinkLike(word) {
			b.WriteString(mdCode(word))
		} else {
			b.WriteString(mdTextEscaper.Replace(word))
		}
		if end < len(s) {
			// The separator: a space, or a pipe (written "\|").
			b.WriteString(mdTextEscaper.Replace(s[end : end+1]))
			end++
		}
		s = s[end:]
	}
	return b.String()
}

// mdLinkLike reports whether a renderer can make a link or a mention out of
// the word with no Markdown syntax in it. GitHub Flavored Markdown links a
// bare "http://", "https://", "ftp://", and "www." address and an email
// address, and GitHub makes a mention of "@name". A backslash does not stop
// all of these on all renderers (cmark-gfm links the address in
// "\[x\](http://e)"). A code span does, so that is where such a word goes.
func mdLinkLike(word string) bool {
	lower := strings.ToLower(word)
	return strings.Contains(word, "@") ||
		strings.Contains(lower, "://") ||
		strings.Contains(lower, "www.") ||
		strings.Contains(lower, "mailto:") ||
		strings.Contains(lower, "xmpp:")
}

// mdLabel escapes a short fixed word from a document: a severity, a status,
// a verdict. It is mdText with a small length limit, because these values
// are in every row of a table.
func mdLabel(s string) string {
	return mdText(mdShorten(strings.TrimSpace(mdScrub(s)), mdMaxLabel))
}

// mdSafeURL accepts an http or https URL made only of characters that
// cannot end a Markdown link target or start other syntax. No space, no
// parenthesis, no angle bracket, no quote, no backslash.
var mdSafeURL = regexp.MustCompile(`^https?://[A-Za-z0-9._~:/?#@!$&'*+,;=%-]+$`)

// mdLink returns a Markdown link, or "" when the URL is not one that
// mdSafeURL accepts. The label must be text that the caller controls.
func mdLink(label, url string) string {
	if len(url) > mdMaxURL || !mdSafeURL.MatchString(url) {
		return ""
	}
	return "[" + label + "](" + url + ")"
}

// mdCount renders a count map as "`ConfigMap` 2, `Service` 1", sorted by
// key so the line is the same on every run. At most mdMaxRows keys.
func mdCount(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	left := 0
	if len(keys) > mdMaxRows {
		left = len(keys) - mdMaxRows
		keys = keys[:mdMaxRows]
	}
	parts := make([]string, 0, len(keys)+1)
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", mdCode(k), counts[k]))
	}
	if left > 0 {
		parts = append(parts, fmt.Sprintf("and %d more kinds", left))
	}
	return strings.Join(parts, ", ")
}

// mdWhere renders the origin of a workload for a table cell: "file:line",
// "file", or "-" when the document does not know (a cluster scan).
func mdWhere(o *schema.Origin) string {
	if o == nil || o.File == "" {
		return "-"
	}
	if o.Line > 0 {
		return mdCodeCell(fmt.Sprintf("%s:%d", mdShorten(o.File, mdMaxValue-8), o.Line))
	}
	return mdCodeCell(o.File)
}

// ---------------------------------------------------------------------------
// ScanReport.
// ---------------------------------------------------------------------------

// writeScanReportMarkdown is the ScanReport view: the gate, the counts, the
// workloads by verdict, what the rules mean, and what the scan did not
// classify.
func writeScanReportMarkdown(d *mdDoc, report *schema.ScanReport, gate *Gate) {
	sum := report.Spec.Summary
	runtimeClass := report.Metadata.RuntimeClassName
	if runtimeClass == "" {
		runtimeClass = schema.DefaultRuntimeClassName
	}

	d.heading(2, "agentmoat scan")
	writeGateMarkdown(d, gate)

	d.table(
		[]string{"Workloads", "Compatible", "Review", "Incompatible", "Sandboxed"},
		map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},
		[][]string{{
			fmt.Sprint(sum.Total), fmt.Sprint(sum.Compatible), fmt.Sprint(sum.NeedsReview),
			fmt.Sprint(sum.Incompatible), fmt.Sprint(sum.Sandboxed),
		}},
	)
	d.linef("%s Sandboxed: the workloads that ask for RuntimeClass %s.", scanSourceSentence(report), mdCode(runtimeClass))
	d.blank()

	src := report.Metadata.Source
	if src != nil && len(src.Unrendered) > 0 {
		d.linef("> **Warning: this input is not rendered.** A file scan reads each object as written and applies no overlay, " +
			"so the verdicts describe the files and not what gets deployed. " +
			"Scan the rendered objects: `kustomize build <dir> | agentmoat scan -f -`")
		d.blank()
		d.fence(src.Unrendered)
	}

	var incompatible, review, compatible []schema.WorkloadResult
	for _, w := range report.Spec.Workloads {
		switch w.Compatibility {
		case schema.CompatibilityIncompatible:
			incompatible = append(incompatible, w)
		case schema.CompatibilityReview:
			review = append(review, w)
		default:
			compatible = append(compatible, w)
		}
	}
	if len(incompatible) > 0 {
		d.heading(3, fmt.Sprintf("Incompatible (%d)", len(incompatible)))
		writeWorkloadTable(d, incompatible)
	}
	if len(review) > 0 {
		d.heading(3, fmt.Sprintf("Review (%d)", len(review)))
		writeWorkloadTable(d, review)
	}
	if len(compatible) > 0 {
		d.details(fmt.Sprintf("Compatible (%d)", len(compatible)))
		writeWorkloadTable(d, compatible)
		d.endDetails()
	}

	writeRuleLegend(d, report.Spec.Workloads)

	if src != nil && len(src.Skipped) > 0 {
		d.heading(3, "Not classified")
		d.linef("These objects have no pod template, so there is nothing to classify: %s.", mdCount(src.Skipped))
		d.blank()
	}
	if src != nil && len(src.Duplicates) > 0 {
		d.heading(3, "Defined more than one time")
		d.fence(src.Duplicates)
	}
}

// scanSourceSentence says where the workloads came from.
func scanSourceSentence(report *schema.ScanReport) string {
	if src := report.Metadata.Source; src != nil {
		return fmt.Sprintf("Source: %s (%d input(s)). No cluster was read.", mdCode(src.Type), src.Inputs)
	}
	if c := report.Metadata.Cluster; c != "" {
		return fmt.Sprintf("Cluster: %s.", mdCode(c))
	}
	return "Source: a cluster."
}

// writeGateMarkdown writes the result of the gate as the first thing in the
// report. A nil gate writes nothing: the caller evaluated none (a library
// caller). The CLI always gives one, so that a report with no gate line is
// never mistaken for a pass.
func writeGateMarkdown(d *mdDoc, gate *Gate) {
	if gate == nil {
		return
	}
	var mark, title string
	switch gate.Outcome {
	case GatePassed:
		mark, title = "✅", "Gate passed"
	case GateFailed:
		mark, title = "❌", "Gate failed"
	case GateNothingChecked:
		mark, title = "⚠️", "Gate checked nothing"
	case GateRefused:
		mark, title = "❌", "Gate could not answer"
	case GateOff:
		mark, title = "➖", "No gate"
	default:
		// An outcome this renderer does not know. Say so; do not let it
		// read as a pass.
		mark, title = "⚠️", "Gate result unknown"
	}
	d.linef("%s **%s** (%s). %s", mark, title, mdCode("--fail-on "+gate.FailOn), mdText(gate.Summary))
	d.blank()
	if len(gate.Details) > 0 {
		d.fence(gate.Details)
	}
}

// writeWorkloadTable writes one row for each workload.
func writeWorkloadTable(d *mdDoc, workloads []schema.WorkloadResult) {
	rows := make([][]string, 0, len(workloads))
	for _, w := range workloads {
		rules := make([]string, 0, len(w.Reasons))
		for i, r := range w.Reasons {
			if i == mdMaxRowRules {
				rules = append(rules, fmt.Sprintf("%d more", len(w.Reasons)-i))
				break
			}
			rules = append(rules, fmt.Sprintf("%s (%s)", mdCodeCell(r.RuleID), mdLabel(string(r.Severity))))
		}
		ruleCell := "-"
		if len(rules) > 0 {
			ruleCell = strings.Join(rules, ", ")
		}
		rows = append(rows, []string{
			mdCodeCell(w.Namespace),
			mdCodeCell(w.Kind),
			mdCodeCell(w.Name),
			mdCodeCell(w.RuntimeClassName),
			ruleCell,
			mdWhere(w.Origin),
		})
	}
	d.table([]string{"Namespace", "Kind", "Name", "Runtime", "Rules", "Where"}, nil, rows)
}

// legendEntry is one line of the rule legend: a rule, and one description
// text that it gave.
type legendEntry struct {
	ruleID      string
	severity    schema.Severity
	description string
	url         string
	workloads   int
}

// severityRank orders severities for display: error, warn, info, then
// anything else.
func severityRank(s schema.Severity) int {
	switch s {
	case schema.SeverityError:
		return 0
	case schema.SeverityWarn:
		return 1
	case schema.SeverityInfo:
		return 2
	}
	return 3
}

// writeRuleLegend writes what each rule in the report means, one time for
// each different text, with the number of workloads that have it.
//
// A description is not always the same for one rule: the classifier can add
// a sentence about the workload (the GPU rule names the resource that was
// requested). So the legend groups by the text, not by the rule ID, and a
// rule can have more than one line.
func writeRuleLegend(d *mdDoc, workloads []schema.WorkloadResult) {
	type key struct {
		ruleID      string
		severity    schema.Severity
		description string
	}
	entries := map[key]*legendEntry{}
	for _, w := range workloads {
		for _, r := range w.Reasons {
			k := key{r.RuleID, r.Severity, r.Description}
			e := entries[k]
			if e == nil {
				e = &legendEntry{ruleID: r.RuleID, severity: r.Severity, description: r.Description, url: r.RemediationURL}
				entries[k] = e
			}
			e.workloads++
		}
	}
	if len(entries) == 0 {
		return
	}
	list := make([]*legendEntry, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if ra, rb := severityRank(a.severity), severityRank(b.severity); ra != rb {
			return ra < rb
		}
		if a.ruleID != b.ruleID {
			return a.ruleID < b.ruleID
		}
		return a.description < b.description
	})

	d.heading(3, "What the rules say")
	shown := list
	if len(shown) > mdMaxRows {
		shown = shown[:mdMaxRows]
	}
	for _, e := range shown {
		line := fmt.Sprintf("- %s (%s, %d workload(s)): %s",
			mdCode(e.ruleID), mdLabel(string(e.severity)), e.workloads, mdText(e.description))
		var links []string
		if l := mdLink("explanation", ruleDocURL(e.ruleID)); l != "" {
			links = append(links, l)
		}
		if l := mdLink("gVisor documentation", e.url); l != "" {
			links = append(links, l)
		}
		if len(links) > 0 {
			line += " " + strings.Join(links, " · ")
		}
		d.linef("%s", line)
	}
	d.blank()
	d.leftOut(len(list) - len(shown))
}
