// Package output: table renderers.
//
// We use github.com/charmbracelet/lipgloss/table for the columnar body. It
// gives us automatic column-width sizing (like text/tabwriter), plus
// header-styling, per-cell StyleFunc, and ANSI-aware width measurement so
// pre-colored cells line up correctly.
//
// Each renderer follows the same structure, in this order:
//
//  1. Bold title:        "agentmoat <verb>"
//  2. Dim subtitle:      metadata chips (plan-hash, dry-run, ...) when relevant
//  3. SUMMARY line:      one-liner where each "value count" pair is colored
//                        by the category's semantic palette (Success/Warn/...)
//  4. Body:              lipgloss/table with borderless layout, Bold headers,
//                        StatusBadge in the status column, right-aligned
//                        numeric columns.
//  5. Empty-state line:  rendered through s.Muted when no rows exist.
//
// When useColor is false the Styles fields are zero-value (see style.go)
// so every Render call is a no-op: the same code path produces plain
// text. This is what keeps tests and pipes deterministic.

package output

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// maxReasonsInline is the cap on per-row reason rendering. If more reasons
// fire, the table appends "(+N more)" so the operator knows to re-run
// with --output json for the full picture.
const maxReasonsInline = 3

// Width caps for free-form table cells. Without them a single long
// workload name or operator note can blow the table wider than the
// terminal and force lipgloss/table to wrap continuation lines that
// visually collide with the next row. The values are tuned by eye: wide
// enough that typical real-world values pass through untouched, narrow
// enough that pathological cases (auto-generated controller pod names,
// multi-sentence notes) get trimmed to "..." with a JSON-hint footer.
const (
	maxNameWidth     = 60
	maxFreeformWidth = 40
)

// renderScanReportTable is the ScanReport-specific table view.
func renderScanReportTable(report *schema.ScanReport, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)
	sum := report.Spec.Summary

	// Title line.
	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat scan")); err != nil {
		return err
	}

	// Subtitle chips. We skip the line entirely if no chips have content.
	if chips := scanChips(report); len(chips) > 0 {
		if _, err := fmt.Fprintln(w, s.Muted.Render(strings.Join(chips, "   "))); err != nil {
			return err
		}
	}

	// SUMMARY block: total headline followed by a horizontal bar chart so the
	// operator sees the compatible/review/incompatible ratios at a glance.
	if _, err := fmt.Fprintf(w, "%s  %d workloads\n", s.Bold.Render("SUMMARY"), sum.Total); err != nil {
		return err
	}
	items := []barItem{
		{Symbol: symbolOK, Label: "compatible", Count: sum.Compatible, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolWarn, Label: "review", Count: sum.NeedsReview, Style: s.Warn, NoColorFill: '▓'},
		{Symbol: symbolFail, Label: "incompatible", Count: sum.Incompatible, Style: s.Danger, NoColorFill: '▒'},
	}
	if chart := renderSummaryBar(items, summaryBarWidth(w), useColor); chart != "" {
		if _, err := fmt.Fprintln(w, chart); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(report.Spec.Workloads) == 0 {
		if _, err := fmt.Fprintln(w, s.Muted.Render("(no workloads found)")); err != nil {
			return err
		}
		return renderScanSource(s, report.Metadata.Source, w)
	}

	// Body table. Columns: NAMESPACE, KIND, NAME, RUNTIME, VERDICT, REASONS.
	// VERDICT is a pre-baked StatusBadge so column widths stay correct
	// (lipgloss/table measures display width, ANSI-aware).
	//
	// RUNTIME is the runtimeClassName the pod spec asks for today ("-" for
	// none). It is what tells a workload that is already in the sandbox
	// from a candidate, and it sits next to VERDICT because the pair is
	// the thing to read: "gvisor" beside "incompatible" is a workload that
	// is about to break, or already has.
	//
	// The per-workload Recommendation field is deliberately omitted from
	// the table. For "compatible" rows it is a canned line that repeats on
	// every row (see pkg/classifier: recommendationFor) and dominates table
	// width on large clusters; for review/incompatible rows the actionable
	// signal is in REASONS. Recommendation is still emitted in the JSON
	// and YAML payloads, the muted footer below points operators there.
	headers := []string{"NAMESPACE", "KIND", "NAME", "RUNTIME", "VERDICT", "REASONS"}
	t := newBaseTable(s, headers, nil /* numericCols: none here */)
	for _, wl := range report.Spec.Workloads {
		t.Row(
			displayNamespace(wl.Namespace),
			wl.Kind,
			truncateCell(wl.Name, maxNameWidth),
			displayRuntime(wl.RuntimeClassName),
			StatusBadge(s, string(wl.Compatibility)),
			summarizeReasons(wl.Reasons),
		)
	}
	if _, err := fmt.Fprintln(w, t.Render()); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, s.Muted.Render("(use --output json for full per-workload recommendations and reason details)")); err != nil {
		return err
	}
	return renderScanSource(s, report.Metadata.Source, w)
}

// scanChips builds the subtitle of the scan table: where the workloads came
// from (a cluster, or manifests), how many there are, and how many already
// ask for the sandbox. Every chip is optional.
func scanChips(report *schema.ScanReport) []string {
	sum := report.Spec.Summary
	chips := []string{}
	if c := report.Metadata.Cluster; c != "" {
		chips = append(chips, "cluster: "+c)
	}
	if src := report.Metadata.Source; src != nil {
		chips = append(chips, fmt.Sprintf("source: %s (%d input(s), no cluster)", src.Type, src.Inputs))
	}
	if total := sum.Total; total > 0 {
		chips = append(chips, fmt.Sprintf("scanned: %d", total))
	}
	if sum.Sandboxed > 0 {
		chips = append(chips, fmt.Sprintf("sandboxed (%s): %d", displayRuntime(report.Metadata.RuntimeClassName), sum.Sandboxed))
	}
	return chips
}

// renderScanSource writes what a scan of manifests has to say beyond its
// rows: what it read and did not classify, what is defined twice, and
// whether the input is the rendered form of the objects. Nil (a cluster
// scan) writes nothing.
//
// These lines are in the table, and not only on stderr, because the table
// must be honest when it is read alone: with --quiet, or pasted into a
// ticket. "0 incompatible" means little without "and 40 objects were not
// workloads".
func renderScanSource(s *Styles, src *schema.ReportSource, w io.Writer) error {
	if src == nil {
		return nil
	}
	if len(src.Skipped) > 0 {
		line := "not classified (no pod template): " + formatCounts(src.Skipped)
		if _, err := fmt.Fprintln(w, s.Muted.Render(line)); err != nil {
			return err
		}
	}
	for _, d := range src.Duplicates {
		if _, err := fmt.Fprintf(w, "%s %s\n", s.Warn.Render(symbolWarn), d); err != nil {
			return err
		}
	}
	if len(src.Unrendered) > 0 {
		head := fmt.Sprintf("%s not rendered: the verdicts describe the files, not what gets deployed (overlays are not applied)", symbolWarn)
		if _, err := fmt.Fprintln(w, s.Warn.Render(head)); err != nil {
			return err
		}
		for _, sign := range src.Unrendered {
			if _, err := fmt.Fprintf(w, "    %s\n", sign); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintln(w, s.Muted.Render("    scan the rendered objects: kustomize build <dir> | agentmoat scan -f -")); err != nil {
			return err
		}
	}
	return nil
}

// displayNamespace renders a namespace cell. A manifest that names no
// namespace has none; "-" says so better than a blank cell.
func displayNamespace(ns string) string {
	if ns == "" {
		return "-"
	}
	return ns
}

// displayRuntime renders a runtimeClassName cell, with "-" for none.
func displayRuntime(runtimeClass string) string {
	if runtimeClass == "" {
		return "-"
	}
	return runtimeClass
}

// formatCounts renders a count map as "2 ConfigMap, 1 Service", sorted by
// key so the line is the same on every run.
func formatCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return strings.Join(parts, ", ")
}

// renderMigrationPlanTable is the MigrationPlan-specific view: ordered
// steps with risk score and wait hint, followed by the excluded workloads.
func renderMigrationPlanTable(plan *schema.MigrationPlan, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)
	sum := plan.Spec.Summary

	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat plan")); err != nil {
		return err
	}

	// Subtitle chips: plan-hash and runtime-class. Both load-bearing.
	chips := []string{
		"plan-hash: " + plan.Metadata.PlanHash,
		"runtime-class: " + plan.Spec.Options.RuntimeClassName,
	}
	if _, err := fmt.Fprintln(w, s.Muted.Render(strings.Join(chips, "   "))); err != nil {
		return err
	}

	if err := renderPlanSummary(s, sum, w, useColor); err != nil {
		return err
	}

	// tr tracks whether any free-form cell (NOTES, REASON) got trimmed by
	// truncateCell. We only print the JSON-hint footer when something
	// actually got shortened, so short fixtures don't get a misleading
	// "look in the JSON" pointer.
	var tr trackingTruncator

	if len(plan.Spec.Steps) == 0 {
		if _, err := fmt.Fprintln(w, s.Muted.Render("(no included steps)")); err != nil {
			return err
		}
	} else {
		// "#" and "RISK" right-align; the rest left-align.
		headers := []string{"#", "NAMESPACE", "KIND", "NAME", "RISK", "WAIT-FOR", "NOTES"}
		t := newBaseTable(s, headers, map[int]bool{0: true, 4: true})
		for _, st := range plan.Spec.Steps {
			t.Row(
				strconv.Itoa(st.Order),
				st.Target.Namespace,
				st.Target.Kind,
				tr.cell(st.Target.Name, maxNameWidth),
				strconv.Itoa(st.RiskScore),
				st.WaitFor,
				tr.cell(st.Notes, maxFreeformWidth),
			)
		}
		if _, err := fmt.Fprintln(w, t.Render()); err != nil {
			return err
		}
	}

	// Cluster warnings (from the scan's ClusterFacts). Silent when none.
	if err := renderPlanWarnings(s, plan.Spec.Warnings, w); err != nil {
		return err
	}

	// Excluded workloads section. Only printed when there is something to
	// show; an "always show" blank section would just be visual noise.
	if len(plan.Spec.Excluded) > 0 {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, s.Bold.Render("Excluded workloads:")); err != nil {
			return err
		}
		headers := []string{"NAMESPACE", "KIND", "NAME", "COMPATIBILITY", "REASON"}
		t := newBaseTable(s, headers, nil)
		for _, e := range plan.Spec.Excluded {
			t.Row(
				e.Target.Namespace,
				e.Target.Kind,
				tr.cell(e.Target.Name, maxNameWidth),
				StatusBadge(s, string(e.Compatibility)),
				tr.cell(e.Reason, maxFreeformWidth),
			)
		}
		if _, err := fmt.Fprintln(w, t.Render()); err != nil {
			return err
		}
	}

	return maybePrintTruncationHint(s, w, tr, "notes and reasons")
}

// renderPlanSummary prints the SUMMARY headline + horizontal bar chart
// for a MigrationPlan: included vs excluded counts. Extracted from
// renderMigrationPlanTable so the parent function reads as a top-level
// section walker.
func renderPlanSummary(s *Styles, sum schema.PlanSummary, w io.Writer, useColor bool) error {
	if _, err := fmt.Fprintf(w, "%s  %d workloads\n", s.Bold.Render("SUMMARY"), sum.Total); err != nil {
		return err
	}
	items := []barItem{
		{Symbol: symbolOK, Label: "included", Count: sum.Included, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolDot, Label: "excluded", Count: sum.Excluded, Style: s.Muted, NoColorFill: '░'},
	}
	if chart := renderSummaryBar(items, summaryBarWidth(w), useColor); chart != "" {
		if _, err := fmt.Fprintln(w, chart); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderApplyResultTable is the ApplyResult-specific view: per-step
// outcome with status and optional error.
//
// The `action` parameter ("apply" or "rollback") is used in the header so
// the renderer for RollbackResult can re-use this function.
func renderApplyResultTable(res *schema.ApplyResult, w io.Writer, action string, useColor bool) error {
	s := NewStyles(useColor)
	sum := res.Spec.Summary

	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat "+action)); err != nil {
		return err
	}

	chips := []string{
		"plan-hash: " + res.Metadata.PlanHash,
		fmt.Sprintf("dry-run: %v", res.Metadata.DryRun),
	}
	if _, err := fmt.Fprintln(w, s.Muted.Render(strings.Join(chips, "   "))); err != nil {
		return err
	}

	// PREFLIGHT block: only apply carries one (rollback never runs the
	// preflight, so its Metadata.Preflight is nil and this prints nothing).
	if err := renderApplyPreflightBlock(s, res.Metadata.Preflight, res.Spec.PreflightFindings, w); err != nil {
		return err
	}

	// SUMMARY block: total headline followed by a horizontal bar chart over
	// the four step outcomes.
	if _, err := fmt.Fprintf(w, "%s  %d steps\n", s.Bold.Render("SUMMARY"), sum.Total); err != nil {
		return err
	}
	items := []barItem{
		{Symbol: symbolOK, Label: "applied", Count: sum.Applied, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolArrow, Label: "already-applied", Count: sum.AlreadyApplied, Style: s.Info, NoColorFill: '▓'},
		{Symbol: symbolDot, Label: "skipped", Count: sum.Skipped, Style: s.Muted, NoColorFill: '░'},
		{Symbol: symbolFail, Label: "failed", Count: sum.Failed, Style: s.Danger, NoColorFill: '▒'},
	}
	if chart := renderSummaryBar(items, summaryBarWidth(w), useColor); chart != "" {
		if _, err := fmt.Fprintln(w, chart); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(res.Spec.Steps) == 0 {
		_, err := fmt.Fprintln(w, s.Muted.Render("(no steps)"))
		return err
	}

	// "#" is right-aligned.
	headers := []string{"#", "NAMESPACE", "KIND", "NAME", "STATUS", "NOTE"}
	t := newBaseTable(s, headers, map[int]bool{0: true})
	for _, st := range res.Spec.Steps {
		note := st.Error
		if note == "" && res.Metadata.DryRun && st.Patch != "" {
			note = compactPatchPreview(st.Patch)
		}
		t.Row(
			strconv.Itoa(st.Order),
			st.Target.Namespace,
			st.Target.Kind,
			st.Target.Name,
			StatusBadge(s, string(st.Status)),
			note,
		)
	}
	_, err := fmt.Fprintln(w, t.Render())
	return err
}

// renderRollbackResultTable rides on renderApplyResultTable with the action
// header swapped out.
func renderRollbackResultTable(res *schema.RollbackResult, w io.Writer, useColor bool) error {
	// RollbackResult is structurally identical to ApplyResult down to
	// Spec.Steps; the only difference is the Kind. We cast by hand here
	// rather than introduce a typeswitch.
	view := &schema.ApplyResult{
		APIVersion: res.APIVersion,
		Kind:       res.Kind,
		Metadata:   res.Metadata,
		Spec: schema.ApplySpec{
			Summary: res.Spec.Summary,
			Steps:   res.Spec.Steps,
		},
	}
	return renderApplyResultTable(view, w, "rollback", useColor)
}

// newBaseTable returns a borderless lipgloss table with the conventions
// every renderer in this file shares:
//
//   - All six borders off: no visible chrome; columns are separated by
//     padding only. Keeps the layout terminal-friendly and reduces visual
//     noise vs the previous tabwriter output.
//   - Headers Bold.
//   - Per-cell horizontal alignment: columns whose index is in numericCols
//     are right-aligned (numbers read better right-aligned); everything
//     else left-aligns.
//   - A small horizontal padding (one space per side) so adjacent cells
//     have breathing room.
//
// We deliberately set Border() to HiddenBorder before disabling every
// direction: HiddenBorder still occupies one cell of width per side which
// the All-direction-off setters then suppress, producing a tighter layout
// than NormalBorder-then-disable.
func newBaseTable(s *Styles, headers []string, numericCols map[int]bool) *table.Table {
	t := table.New().
		Border(lipgloss.HiddenBorder()).
		BorderTop(false).
		BorderBottom(false).
		BorderLeft(false).
		BorderRight(false).
		BorderHeader(false).
		BorderColumn(false).
		BorderRow(false).
		Headers(headers...).
		StyleFunc(func(row, col int) lipgloss.Style {
			// Base padding: one space each side keeps columns from
			// touching. We compose by starting from the lipgloss
			// default (zero-value Style) and then layering on the
			// header/alignment options as needed.
			st := lipgloss.NewStyle().Padding(0, 1)
			if row == table.HeaderRow {
				st = st.Inherit(s.Bold)
			}
			if numericCols[col] {
				st = st.Align(lipgloss.Right)
			}
			return st
		})
	return t
}

// summarizeReasons joins up to maxReasonsInline rule IDs with "; " for a
// table-friendly cell. Returns "-" for the no-reasons case.
func summarizeReasons(reasons []schema.Reason) string {
	if len(reasons) == 0 {
		return "-"
	}
	ids := make([]string, 0, len(reasons))
	for i, r := range reasons {
		if i >= maxReasonsInline {
			break
		}
		ids = append(ids, fmt.Sprintf("%s(%s)", r.RuleID, string(r.Severity)))
	}
	str := strings.Join(ids, "; ")
	if len(reasons) > maxReasonsInline {
		str += fmt.Sprintf(" (+%d more)", len(reasons)-maxReasonsInline)
	}
	return str
}

// renderVerifyReportTable is the VerifyReport-specific view: per-step verdict
// against the live cluster. One row per step; the PROBE column tells the
// operator at a glance whether the in-pod gVisor probe ran and what it found,
// and the NODE column whether the hosting nodes match the RuntimeClass
// nodeSelector.
func renderVerifyReportTable(report *schema.VerifyReport, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)

	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat verify")); err != nil {
		return err
	}
	waited := report.Metadata.Wait != nil
	if _, err := fmt.Fprintln(w, s.Muted.Render(strings.Join(verifyChips(report), "   "))); err != nil {
		return err
	}

	if err := writeVerifySummary(s, w, report, useColor); err != nil {
		return err
	}
	if err := writeVerifyNotCheckedTable(s, w, report.Spec.NotChecked); err != nil {
		return err
	}

	if len(report.Spec.Results) == 0 {
		_, err := fmt.Fprintln(w, s.Muted.Render("(no results)"))
		return err
	}

	var tr trackingTruncator

	// "#" right-aligned. The READY column is there only after a wait:
	// without one the verifier does not read readiness.
	headers := []string{"#", "STATUS", "KIND/NS/NAME", "EXPECTED", "ACTUAL", "PROBE", "NODE"}
	if waited {
		headers = append(headers, "READY")
	}
	headers = append(headers, "MESSAGE")
	t := newBaseTable(s, headers, map[int]bool{0: true})
	for _, r := range report.Spec.Results {
		row := []string{
			strconv.Itoa(r.Order),
			StatusBadge(s, string(r.Status)),
			// With manifests as the source, the names and the class are
			// text from a file, and a message can repeat them. termLine
			// replaces control characters; a name from a cluster has none.
			tr.cell(termLine(fmt.Sprintf("%s/%s/%s", r.Target.Kind, r.Target.Namespace, r.Target.Name)), maxNameWidth),
			termLine(r.Expected),
			termLine(displayActual(r.Actual)),
			summarizeProbe(r.Probe),
			summarizeNodePlacement(r.NodePlacement),
		}
		if waited {
			row = append(row, summarizeReadiness(r))
		}
		row = append(row, tr.cell(termLine(r.Message), maxFreeformWidth))
		t.Row(row...)
	}
	if _, err := fmt.Fprintln(w, t.Render()); err != nil {
		return err
	}
	if err := writeVerifyNotOK(s, w, report.Spec.Results); err != nil {
		return err
	}
	return maybePrintTruncationHint(s, w, tr, "messages")
}

// writeVerifySummary prints the SUMMARY block of the verify table: the total
// headline, a horizontal bar chart over the verdicts of the results, and
// (for a verify from manifests) the count of the workloads that verify did
// not look at. An empty line ends the block.
func writeVerifySummary(s *Styles, w io.Writer, report *schema.VerifyReport, useColor bool) error {
	sum := report.Spec.Summary
	if _, err := fmt.Fprintf(w, "%s  %d results\n", s.Bold.Render("SUMMARY"), sum.Total); err != nil {
		return err
	}
	items := []barItem{
		{Symbol: symbolOK, Label: "ok", Count: sum.OK, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolWarn, Label: "mismatch", Count: sum.Mismatch, Style: s.Warn, NoColorFill: '▓'},
		{Symbol: symbolFail, Label: "error", Count: sum.Error, Style: s.Danger, NoColorFill: '▒'},
	}
	if chart := renderSummaryBar(items, summaryBarWidth(w), useColor); chart != "" {
		if _, err := fmt.Fprintln(w, chart); err != nil {
			return err
		}
	}
	if report.Metadata.Source != nil {
		if _, err := fmt.Fprintln(w, s.Muted.Render(fmt.Sprintf("%d workload(s) of the input do not ask for the class and were not looked at",
			sum.Excluded))); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// verifyChips is the line under the title of the verify table: where the
// list of workloads came from, and how the run was made.
func verifyChips(report *schema.VerifyReport) []string {
	chips := []string{
		"plan-hash: " + report.Metadata.PlanHash,
		fmt.Sprintf("in-pod-probe: %v", report.Metadata.InPodProbe),
	}
	if report.Metadata.Source != nil {
		// A verify from manifest files has no plan. The class name is a
		// value that the caller gave, so it goes through termLine.
		chips = []string{
			"source: manifests",
			"runtime-class: " + termLine(report.Metadata.RuntimeClassName),
			fmt.Sprintf("in-pod-probe: %v", report.Metadata.InPodProbe),
		}
	}
	// A report after a wait says so, and says how the wait ended: "3 not
	// ok" after one look and "3 not ok after 3 minutes" are different
	// facts for the person who decides on a rollback.
	if report.Metadata.Wait != nil {
		chips = append(chips, "wait: "+describeWait(report.Metadata.Wait))
	}
	return chips
}

// writeVerifyNotCheckedTable prints the workloads that a verify from
// manifests has no verdict for, above the results: a report with entries
// here is not a pass, and the reader must see that first. The names and the
// reasons have text from a manifest; termLine replaces control characters.
func writeVerifyNotCheckedTable(s *Styles, w io.Writer, notChecked []schema.VerifyNotChecked) error {
	if len(notChecked) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, s.Bold.Render(fmt.Sprintf("NOT CHECKED (%d): this is not a pass", len(notChecked)))); err != nil {
		return err
	}
	for i, n := range notChecked {
		if i == maxVerifyNotOK {
			if _, err := fmt.Fprintln(w, s.Muted.Render(fmt.Sprintf("  ... and %d more (use --output json)", len(notChecked)-maxVerifyNotOK))); err != nil {
				return err
			}
			break
		}
		if _, err := fmt.Fprintf(w, "  %s %s: %s\n", symbolWarn,
			termLine(fmt.Sprintf("%s/%s/%s", n.Target.Kind, n.Target.Namespace, n.Target.Name)), termLine(n.Reason)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// maxVerifyNotOK is the most steps that the NOT OK block below the verify
// table lists in full.
const maxVerifyNotOK = 10

// writeVerifyNotOK prints, below the table, the full message of each step
// that is not ok. The MESSAGE cell of the table is cut at maxFreeformWidth
// to keep the columns in line, and that cut removes the part that a person
// needs: in "pod cache-1 is not Ready: container app is waiting:
// CrashLoopBackOff" the reason is at the end. A pipeline log shows this
// table, and the person who reads it decides on a rollback.
func writeVerifyNotOK(s *Styles, w io.Writer, results []schema.VerifyResult) error {
	var notOK []schema.VerifyResult
	for _, r := range results {
		if r.Status != schema.VerifyStatusOK && r.Message != "" {
			notOK = append(notOK, r)
		}
	}
	if len(notOK) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, s.Bold.Render("NOT OK")); err != nil {
		return err
	}
	for i, r := range notOK {
		if i == maxVerifyNotOK {
			_, err := fmt.Fprintln(w, s.Muted.Render(fmt.Sprintf("  ... and %d more (use --output json)", len(notOK)-maxVerifyNotOK)))
			return err
		}
		if _, err := fmt.Fprintf(w, "  %d  %s: %s\n", r.Order,
			termLine(fmt.Sprintf("%s/%s/%s", r.Target.Kind, r.Target.Namespace, r.Target.Name)), termLine(r.Message)); err != nil {
			return err
		}
	}
	return nil
}

// describeWait says in a few words how the wait of a verify ended. The
// table and the Markdown view share it. Every part is made from numbers in
// the report, so the text needs no escape.
func describeWait(w *schema.VerifyWait) string {
	requested := time.Duration(w.RequestedSeconds) * time.Second
	waited := time.Duration(w.WaitedSeconds) * time.Second
	switch {
	case w.Interrupted:
		return fmt.Sprintf("interrupted after %s of %s", waited, requested)
	case w.TimedOut:
		return fmt.Sprintf("timed out after %s", requested)
	default:
		return fmt.Sprintf("done after %s of %s", waited, requested)
	}
}

// summarizeReadiness condenses the readiness of one step into a table cell:
// "2/2" (Ready pods of all pods), "no pods" when the verdict is from the pod
// template, "-" when the verifier did not get as far as the readiness check.
func summarizeReadiness(r schema.VerifyResult) string {
	switch {
	case r.Readiness != nil:
		return fmt.Sprintf("%d/%d", r.Readiness.Ready, r.Readiness.Pods)
	case r.Basis == schema.VerifyBasisTemplate:
		return "no pods"
	default:
		return "-"
	}
}

// displayActual returns "(empty)" for an unset Actual so the table row is
// readable; e2e jq assertions still see the empty string in JSON.
func displayActual(actual string) string {
	if actual == "" {
		return "(empty)"
	}
	return actual
}

// summarizeProbe condenses a ProbeResult into one table cell. "-" when the
// probe was not run, "ok" when markers were detected, "no markers" when the
// exec succeeded but nothing matched, "exec-err" when the call itself failed.
func summarizeProbe(p *schema.ProbeResult) string {
	if p == nil {
		return "-"
	}
	if p.Error != "" {
		return "exec-err"
	}
	if p.Detected {
		return "ok"
	}
	return "no markers"
}

// truncateCell shortens s to at most max runes, appending "..." when it
// has to trim. Rune-correct (not byte-truncating) so multi-byte
// characters survive. The empty string passes through unchanged, and a
// max smaller than 4 leaves s alone since the ellipsis would not fit.
func truncateCell(s string, max int) string {
	if max < 4 || utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-3]) + "..."
}

// trackingTruncator wraps truncateCell and remembers whether any call
// actually trimmed a cell. Renderers consult `.truncated` to decide
// whether to print the muted "use --output json for full ..." footer,
// so short fixtures don't get a misleading pointer.
type trackingTruncator struct {
	truncated bool
}

func (tr *trackingTruncator) cell(s string, max int) string {
	out := truncateCell(s, max)
	if out != s {
		tr.truncated = true
	}
	return out
}

// maybePrintTruncationHint prints a muted "(use --output json for full
// <suffix>)" footer when at least one cell was actually trimmed. Renderers
// that bound free-form cells call this once at the end so the operator
// knows where to look for the untrimmed text; renderers whose fixtures fit
// untouched stay silent.
func maybePrintTruncationHint(s *Styles, w io.Writer, tr trackingTruncator, suffix string) error {
	if !tr.truncated {
		return nil
	}
	_, err := fmt.Fprintln(w, s.Muted.Render("(use --output json for full "+suffix+")"))
	return err
}

// compactPatchPreview returns a one-line, length-bounded preview of a
// strategic-merge or JSON-merge patch body. We strip whitespace and
// truncate so a long CronJob patch doesn't wreck table alignment.
func compactPatchPreview(patch string) string {
	// Parse + re-marshal to drop any pretty-printing the producer applied.
	var v any
	if err := json.Unmarshal([]byte(patch), &v); err != nil {
		return patch
	}
	compact, err := json.Marshal(v)
	if err != nil {
		return patch
	}
	str := string(compact)
	const maxLen = 60
	if len(str) > maxLen {
		return str[:maxLen-3] + "..."
	}
	return str
}
