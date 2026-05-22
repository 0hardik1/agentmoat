// Package output: table renderers.
//
// We use github.com/charmbracelet/lipgloss/table for the columnar body. It
// gives us automatic column-width sizing (like text/tabwriter), plus
// header-styling, per-cell StyleFunc, and ANSI-aware width measurement so
// pre-colored cells line up correctly.
//
// Each renderer follows the same skeleton, in this order:
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
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// maxReasonsInline is the cap on per-row reason rendering. If more reasons
// fire, the table appends "(+N more)" so the operator knows to re-run
// with --output json for the full picture.
const maxReasonsInline = 3

// renderScanReportTable is the ScanReport-specific table view.
func renderScanReportTable(report *schema.ScanReport, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)
	sum := report.Spec.Summary

	// Title line.
	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat scan")); err != nil {
		return err
	}

	// Subtitle: cluster + namespaces chips when present. Both are
	// optional; we skip the line entirely if no chips have content.
	chips := []string{}
	if c := report.Metadata.Cluster; c != "" {
		chips = append(chips, "cluster: "+c)
	}
	if total := sum.Total; total > 0 {
		chips = append(chips, fmt.Sprintf("scanned: %d", total))
	}
	if len(chips) > 0 {
		if _, err := fmt.Fprintln(w, s.Muted.Render(strings.Join(chips, "   "))); err != nil {
			return err
		}
	}

	// SUMMARY line: each count colored by its semantic class.
	summary := strings.Join([]string{
		s.Bold.Render("SUMMARY"),
		"compatible " + s.Success.Render(strconv.Itoa(sum.Compatible)),
		"review " + s.Warn.Render(strconv.Itoa(sum.NeedsReview)),
		"incompatible " + s.Danger.Render(strconv.Itoa(sum.Incompatible)),
	}, "   ")
	if _, err := fmt.Fprintln(w, summary); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(report.Spec.Workloads) == 0 {
		_, err := fmt.Fprintln(w, s.Muted.Render("(no workloads found)"))
		return err
	}

	// Body table. Columns: NAMESPACE, KIND, NAME, VERDICT, REASONS,
	// RECOMMENDATION. VERDICT is a pre-baked StatusBadge so column widths
	// stay correct (lipgloss/table measures display width, ANSI-aware).
	headers := []string{"NAMESPACE", "KIND", "NAME", "VERDICT", "REASONS", "RECOMMENDATION"}
	t := newBaseTable(s, headers, nil /* numericCols: none here */)
	for _, wl := range report.Spec.Workloads {
		t.Row(
			wl.Namespace,
			wl.Kind,
			wl.Name,
			StatusBadge(s, string(wl.Compatibility)),
			summarizeReasons(wl.Reasons),
			wl.Recommendation,
		)
	}
	_, err := fmt.Fprintln(w, t.Render())
	return err
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

	// SUMMARY: total/included/excluded with semantic colors.
	summary := strings.Join([]string{
		s.Bold.Render("SUMMARY"),
		"total " + s.Info.Render(strconv.Itoa(sum.Total)),
		"included " + s.Success.Render(strconv.Itoa(sum.Included)),
		"excluded " + s.Muted.Render(strconv.Itoa(sum.Excluded)),
	}, "   ")
	if _, err := fmt.Fprintln(w, summary); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

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
				st.Target.Name,
				strconv.Itoa(st.RiskScore),
				st.WaitFor,
				st.Notes,
			)
		}
		if _, err := fmt.Fprintln(w, t.Render()); err != nil {
			return err
		}
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
				e.Target.Name,
				StatusBadge(s, string(e.Compatibility)),
				e.Reason,
			)
		}
		if _, err := fmt.Fprintln(w, t.Render()); err != nil {
			return err
		}
	}
	return nil
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

	// SUMMARY: applied/already-applied/skipped/failed with semantic colors.
	summary := strings.Join([]string{
		s.Bold.Render("SUMMARY"),
		"applied " + s.Success.Render(strconv.Itoa(sum.Applied)),
		"already-applied " + s.Info.Render(strconv.Itoa(sum.AlreadyApplied)),
		"skipped " + s.Muted.Render(strconv.Itoa(sum.Skipped)),
		"failed " + s.Danger.Render(strconv.Itoa(sum.Failed)),
	}, "   ")
	if _, err := fmt.Fprintln(w, summary); err != nil {
		return err
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
// against the live cluster. One row per step; the Probe column tells the
// operator at a glance whether the in-pod gVisor probe ran and what it found.
func renderVerifyReportTable(report *schema.VerifyReport, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)
	sum := report.Spec.Summary

	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat verify")); err != nil {
		return err
	}

	chips := []string{
		"plan-hash: " + report.Metadata.PlanHash,
		fmt.Sprintf("in-pod-probe: %v", report.Metadata.InPodProbe),
	}
	if _, err := fmt.Fprintln(w, s.Muted.Render(strings.Join(chips, "   "))); err != nil {
		return err
	}

	// SUMMARY: ok/mismatch/error with semantic colors.
	summary := strings.Join([]string{
		s.Bold.Render("SUMMARY"),
		"ok " + s.Success.Render(strconv.Itoa(sum.OK)),
		"mismatch " + s.Warn.Render(strconv.Itoa(sum.Mismatch)),
		"error " + s.Danger.Render(strconv.Itoa(sum.Error)),
	}, "   ")
	if _, err := fmt.Fprintln(w, summary); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}

	if len(report.Spec.Results) == 0 {
		_, err := fmt.Fprintln(w, s.Muted.Render("(no results)"))
		return err
	}

	// "#" right-aligned.
	headers := []string{"#", "STATUS", "KIND/NS/NAME", "EXPECTED", "ACTUAL", "PROBE", "MESSAGE"}
	t := newBaseTable(s, headers, map[int]bool{0: true})
	for _, r := range report.Spec.Results {
		t.Row(
			strconv.Itoa(r.Order),
			StatusBadge(s, string(r.Status)),
			fmt.Sprintf("%s/%s/%s", r.Target.Kind, r.Target.Namespace, r.Target.Name),
			r.Expected,
			displayActual(r.Actual),
			summarizeProbe(r.Probe),
			r.Message,
		)
	}
	_, err := fmt.Fprintln(w, t.Render())
	return err
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
