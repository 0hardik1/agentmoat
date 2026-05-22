// Package output: table renderers.
//
// We use text/tabwriter for the columnar view. It is a stdlib utility that
// aligns columns automatically (think `column -t` without the friction).
//
// One renderer per Kind: ScanReport, MigrationPlan, ApplyResult,
// RollbackResult. Each chooses its own column layout because the most-useful
// columns differ (scan has Compatibility + Reasons; plan has Order + Risk +
// WaitFor; apply has Status + Patch summary).

package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// maxReasonsInline is the cap on per-row reason rendering. If more reasons
// fire, the table appends "(+N more)" so the operator knows to re-run
// with --output json for the full picture.
const maxReasonsInline = 3

// renderScanReportTable is the ScanReport-specific table view.
func renderScanReportTable(report *schema.ScanReport, w io.Writer) error {
	s := report.Spec.Summary
	_, _ = fmt.Fprintf(w, "agentmoat scan: %d workloads scanned\n", s.Total)
	_, _ = fmt.Fprintf(w, "  compatible: %d   review: %d   incompatible: %d\n\n",
		s.Compatible, s.NeedsReview, s.Incompatible)

	if len(report.Spec.Workloads) == 0 {
		_, _ = fmt.Fprintln(w, "(no workloads found)")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()

	_, _ = fmt.Fprintln(tw, "NAMESPACE\tKIND\tNAME\tVERDICT\tREASONS\tRECOMMENDATION")
	for _, wl := range report.Spec.Workloads {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			wl.Namespace,
			wl.Kind,
			wl.Name,
			string(wl.Compatibility),
			summarizeReasons(wl.Reasons),
			wl.Recommendation,
		)
	}
	return nil
}

// renderMigrationPlanTable is the MigrationPlan-specific view: ordered
// steps with risk score and wait hint, followed by the excluded workloads.
func renderMigrationPlanTable(plan *schema.MigrationPlan, w io.Writer) error {
	s := plan.Spec.Summary
	_, _ = fmt.Fprintf(w, "agentmoat plan: %d steps total (included: %d, excluded: %d)\n",
		s.Total, s.Included, s.Excluded)
	_, _ = fmt.Fprintf(w, "  plan-hash: %s\n", plan.Metadata.PlanHash)
	_, _ = fmt.Fprintf(w, "  runtime-class: %s\n\n", plan.Spec.Options.RuntimeClassName)

	if len(plan.Spec.Steps) == 0 {
		_, _ = fmt.Fprintln(w, "(no included steps)")
	} else {
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "#\tNAMESPACE\tKIND\tNAME\tRISK\tWAIT-FOR\tNOTES")
		for _, st := range plan.Spec.Steps {
			_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%s\t%s\n",
				st.Order,
				st.Target.Namespace,
				st.Target.Kind,
				st.Target.Name,
				st.RiskScore,
				st.WaitFor,
				st.Notes,
			)
		}
		_ = tw.Flush()
	}

	if len(plan.Spec.Excluded) > 0 {
		_, _ = fmt.Fprintln(w, "")
		_, _ = fmt.Fprintln(w, "Excluded workloads:")
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "NAMESPACE\tKIND\tNAME\tCOMPATIBILITY\tREASON")
		for _, e := range plan.Spec.Excluded {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				e.Target.Namespace,
				e.Target.Kind,
				e.Target.Name,
				string(e.Compatibility),
				e.Reason,
			)
		}
		_ = tw.Flush()
	}
	return nil
}

// renderApplyResultTable is the ApplyResult-specific view: per-step
// outcome with status and optional error.
//
// The `action` parameter ("apply" or "rollback") is used in the header so
// the renderer for RollbackResult can re-use this function.
func renderApplyResultTable(res *schema.ApplyResult, w io.Writer, action string) error {
	s := res.Spec.Summary
	_, _ = fmt.Fprintf(w, "agentmoat %s: %d steps total\n", action, s.Total)
	_, _ = fmt.Fprintf(w, "  applied: %d   already-applied: %d   skipped: %d   failed: %d\n",
		s.Applied, s.AlreadyApplied, s.Skipped, s.Failed)
	_, _ = fmt.Fprintf(w, "  plan-hash: %s   dry-run: %v\n\n",
		res.Metadata.PlanHash, res.Metadata.DryRun)

	if len(res.Spec.Steps) == 0 {
		_, _ = fmt.Fprintln(w, "(no steps)")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	_, _ = fmt.Fprintln(tw, "#\tNAMESPACE\tKIND\tNAME\tSTATUS\tNOTE")
	for _, st := range res.Spec.Steps {
		note := st.Error
		if note == "" && res.Metadata.DryRun && st.Patch != "" {
			note = compactPatchPreview(st.Patch)
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n",
			st.Order,
			st.Target.Namespace,
			st.Target.Kind,
			st.Target.Name,
			string(st.Status),
			note,
		)
	}
	return nil
}

// renderRollbackResultTable rides on renderApplyResultTable with the action
// header swapped out.
func renderRollbackResultTable(res *schema.RollbackResult, w io.Writer) error {
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
	return renderApplyResultTable(view, w, "rollback")
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
	s := strings.Join(ids, "; ")
	if len(reasons) > maxReasonsInline {
		s += fmt.Sprintf(" (+%d more)", len(reasons)-maxReasonsInline)
	}
	return s
}

// renderVerifyReportTable is the VerifyReport-specific view: per-step verdict
// against the live cluster. One row per step; the Probe column tells the
// operator at a glance whether the in-pod gVisor probe ran and what it found.
func renderVerifyReportTable(report *schema.VerifyReport, w io.Writer) error {
	s := report.Spec.Summary
	_, _ = fmt.Fprintf(w, "agentmoat verify: %d steps verified\n", s.Total)
	_, _ = fmt.Fprintf(w, "  ok: %d   mismatch: %d   error: %d\n",
		s.OK, s.Mismatch, s.Error)
	_, _ = fmt.Fprintf(w, "  plan-hash: %s   in-pod-probe: %v\n\n",
		report.Metadata.PlanHash, report.Metadata.InPodProbe)

	if len(report.Spec.Results) == 0 {
		_, _ = fmt.Fprintln(w, "(no results)")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	defer func() { _ = tw.Flush() }()
	_, _ = fmt.Fprintln(tw, "#\tSTATUS\tKIND/NS/NAME\tEXPECTED\tACTUAL\tPROBE\tMESSAGE")
	for _, r := range report.Spec.Results {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s/%s/%s\t%s\t%s\t%s\t%s\n",
			r.Order,
			string(r.Status),
			r.Target.Kind, r.Target.Namespace, r.Target.Name,
			r.Expected,
			displayActual(r.Actual),
			summarizeProbe(r.Probe),
			r.Message,
		)
	}
	return nil
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

// renderExplainDocumentTable prints the topic content as raw markdown when
// Spec.Content is set, or the topic list (one per line) when only Spec.Topics
// is populated. Both shapes are produced by the agentmoat.Explain orchestrator.
func renderExplainDocumentTable(doc *schema.ExplainDocument, w io.Writer) error {
	if doc.Spec.Content != "" {
		// Topic mode: print the raw markdown so terminal pagers (less, etc.)
		// render headings naturally. Trailing newline ensures the shell
		// prompt lands on its own line.
		content := doc.Spec.Content
		if !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		_, _ = fmt.Fprint(w, content)
		return nil
	}

	// List mode: a short header plus one topic name per line. The header
	// names the binary so an operator who pipes `agentmoat explain` into
	// less/grep still has the context.
	_, _ = fmt.Fprintln(w, "agentmoat explain: available topics")
	for _, t := range doc.Spec.Topics {
		_, _ = fmt.Fprintf(w, "  %s\n", t)
	}
	_, _ = fmt.Fprintln(w, "")
	_, _ = fmt.Fprintln(w, "Run `agentmoat explain <topic>` to print the topic content.")
	return nil
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
	s := string(compact)
	const maxLen = 60
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}
