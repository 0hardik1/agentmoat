// Package output: PreflightReport table renderer, plus the small preflight
// block the apply/rollback and plan tables reuse.
//
// Findings are rendered as a list, not a table: the message and the
// remediation are full sentences that would either be truncated into
// uselessness or wrap badly in a column. The list keeps each finding as
// three lines (badge + id, message, fix) that read top to bottom.
package output

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// renderPreflightReportTable is the PreflightReport-specific view: a
// readiness headline, the counts as a bar, the collected facts, and the
// findings list.
func renderPreflightReportTable(report *schema.PreflightReport, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)
	sum := report.Spec.Summary

	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat preflight")); err != nil {
		return err
	}
	chips := []string{
		"runtime-class: " + report.Metadata.RuntimeClassName,
		"cluster: " + displayCluster(report.Metadata.Cluster),
	}
	if pm := report.Metadata.Probe; pm != nil {
		chips = append(chips, "probe: "+describeProbe(pm))
	}
	if _, err := fmt.Fprintln(w, s.Muted.Render(strings.Join(chips, "   "))); err != nil {
		return err
	}

	if err := renderPreflightSummary(s, sum, w, useColor); err != nil {
		return err
	}
	if err := renderClusterFacts(s, report.Spec.Facts, w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return renderFindingsList(s, report.Spec.Findings, w,
		fmt.Sprintf("(no findings: pods requesting RuntimeClass %q can schedule)", report.Metadata.RuntimeClassName))
}

// renderPreflightSummary prints the SUMMARY headline (with the readiness
// badge) and the error/warn/info bar.
func renderPreflightSummary(s *Styles, sum schema.PreflightSummary, w io.Writer, useColor bool) error {
	if _, err := fmt.Fprintf(w, "%s  %d findings   %s\n",
		s.Bold.Render("SUMMARY"), sum.Total, StatusBadge(s, readinessWord(sum.Ready))); err != nil {
		return err
	}
	items := []barItem{
		{Symbol: symbolFail, Label: "error", Count: sum.Error, Style: s.Danger, NoColorFill: '▒'},
		{Symbol: symbolWarn, Label: "warn", Count: sum.Warn, Style: s.Warn, NoColorFill: '▓'},
		{Symbol: symbolDot, Label: "info", Count: sum.Info, Style: s.Info, NoColorFill: '░'},
	}
	if chart := renderSummaryBar(items, summaryBarWidth(w), useColor); chart != "" {
		if _, err := fmt.Fprintln(w, chart); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderClusterFacts prints the FACTS block: what the preflight saw, so
// the operator can cross-check the findings against `kubectl get nodes`.
func renderClusterFacts(s *Styles, facts schema.ClusterFacts, w io.Writer) error {
	if _, err := fmt.Fprintln(w, s.Bold.Render("FACTS")); err != nil {
		return err
	}
	rc := facts.RuntimeClass
	rcLine := fmt.Sprintf("RuntimeClass %q: not found", rc.Name)
	if rc.Found {
		parts := []string{"handler=" + rc.Handler}
		if len(rc.NodeSelector) > 0 {
			parts = append(parts, "nodeSelector="+formatKV(rc.NodeSelector))
		} else {
			parts = append(parts, "nodeSelector=(none)")
		}
		parts = append(parts, fmt.Sprintf("tolerations=%d", rc.Tolerations))
		if len(rc.Overhead) > 0 {
			parts = append(parts, "overhead="+formatKV(rc.Overhead))
		} else {
			parts = append(parts, "overhead=(none)")
		}
		rcLine = fmt.Sprintf("RuntimeClass %q: %s", rc.Name, strings.Join(parts, "  "))
	}
	if _, err := fmt.Fprintln(w, "  "+rcLine); err != nil {
		return err
	}

	n := facts.Nodes
	nodeLine := fmt.Sprintf("Nodes: total=%d  ready=%d  matching-selector=%d  matching-and-ready=%d  tainted-without-toleration=%d",
		n.Total, n.Ready, n.MatchingSelector, n.MatchingAndReady, n.MatchingTaintedWithoutToleration)
	if _, err := fmt.Fprintln(w, "  "+nodeLine); err != nil {
		return err
	}
	if len(n.MatchingNames) > 0 {
		if _, err := fmt.Fprintln(w, "  "+s.Muted.Render("matching: "+strings.Join(n.MatchingNames, ", "))); err != nil {
			return err
		}
	}

	p := facts.Platform
	if p.EKSAutoModeNodes+p.BottlerocketNodes+p.KarpenterNodes > 0 {
		platLine := fmt.Sprintf("Platform: eks-auto-mode=%d (matching %d)  bottlerocket=%d (matching %d)  karpenter=%d",
			p.EKSAutoModeNodes, p.EKSAutoModeMatchingNodes, p.BottlerocketNodes, p.BottlerocketMatchingNodes, p.KarpenterNodes)
		if _, err := fmt.Fprintln(w, "  "+platLine); err != nil {
			return err
		}
	}
	return renderGPUFacts(s, facts.GPU, w)
}

// renderGPUFacts prints the GPU line and one muted line per (card, driver,
// MIG) group with its support verdicts. Silent on a CPU-only cluster.
func renderGPUFacts(s *Styles, g *schema.GPUFacts, w io.Writer) error {
	if g == nil {
		return nil
	}
	nv := "nvproxy=(not probed; run 'agentmoat probe nvproxy')"
	if g.Nvproxy != nil {
		nv = fmt.Sprintf("nvproxy=runsc %s (%d supported drivers)", g.Nvproxy.RunscVersion, len(g.Nvproxy.SupportedDrivers))
	}
	if _, err := fmt.Fprintf(w, "  GPU: nodes=%d  matching-selector=%d  mig=%d  %s\n", g.Nodes, g.MatchingNodes, g.MIGNodes, nv); err != nil {
		return err
	}
	for _, grp := range g.Groups {
		product, driver := grp.Product, grp.Driver
		if product == "" {
			product = "(no GFD labels)"
		}
		if driver == "" {
			driver = "unknown"
		}
		mig := ""
		if grp.MIG {
			mig = "  mig=yes"
		}
		line := fmt.Sprintf("%s  driver=%s  nodes=%d (matching %d)%s  card=%s  driver-support=%s",
			product, driver, grp.Nodes, grp.MatchingNodes, mig, grp.ProductSupport, grp.DriverSupport)
		if _, err := fmt.Fprintln(w, "    "+s.Muted.Render(line)); err != nil {
			return err
		}
	}
	return nil
}

// describeProbe condenses ProbeMetadata into the header chip.
func describeProbe(pm *schema.ProbeMetadata) string {
	switch {
	case pm.DryRun && pm.Node != "":
		return fmt.Sprintf("dry-run (would create %s/%s on node %s)", pm.Namespace, pm.PodName, pm.Node)
	case pm.DryRun:
		return "dry-run (not scheduled: preflight not ready)"
	case pm.Succeeded:
		return fmt.Sprintf("%s/%s ran on node %s", pm.Namespace, pm.PodName, pm.Node)
	case pm.Node == "":
		return "skipped (preflight not ready)"
	default:
		return fmt.Sprintf("failed on node %s (see findings)", pm.Node)
	}
}

// renderFindingsList prints FINDINGS as badge + id, message, and fix
// lines. emptyText is printed (muted) when there is nothing to show.
func renderFindingsList(s *Styles, findings []schema.PreflightFinding, w io.Writer, emptyText string) error {
	if _, err := fmt.Fprintln(w, s.Bold.Render("FINDINGS")); err != nil {
		return err
	}
	if len(findings) == 0 {
		_, err := fmt.Fprintln(w, "  "+s.Muted.Render(emptyText))
		return err
	}
	for _, f := range findings {
		if _, err := fmt.Fprintf(w, "  %s  %s\n", StatusBadge(s, string(f.Severity)), s.Bold.Render(f.ID)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "           "+f.Message); err != nil {
			return err
		}
		if f.Remediation != "" {
			if _, err := fmt.Fprintln(w, "           "+s.Muted.Render("fix: "+f.Remediation)); err != nil {
				return err
			}
		}
	}
	return nil
}

// renderApplyPreflightBlock is the PREFLIGHT section of the apply table:
// one headline when the preflight passed, headline plus the error findings
// when it blocked the apply. Nothing is printed when the preflight was
// skipped (summary nil), so rollback output is unchanged.
func renderApplyPreflightBlock(s *Styles, summary *schema.PreflightSummary, findings []schema.PreflightFinding, w io.Writer) error {
	if summary == nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "%s  %s (%d error, %d warn, %d info)\n",
		s.Bold.Render("PREFLIGHT"), StatusBadge(s, readinessWord(summary.Ready)),
		summary.Error, summary.Warn, summary.Info); err != nil {
		return err
	}
	if summary.Ready {
		_, err := fmt.Fprintln(w)
		return err
	}
	for _, f := range findings {
		if f.Severity != schema.SeverityError {
			continue
		}
		if _, err := fmt.Fprintf(w, "  %s  %s\n", StatusBadge(s, string(f.Severity)), s.Bold.Render(f.ID)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "           "+f.Message); err != nil {
			return err
		}
		if f.Remediation != "" {
			if _, err := fmt.Fprintln(w, "           "+s.Muted.Render("fix: "+f.Remediation)); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintln(w, "  "+s.Muted.Render("nothing was mutated; run 'agentmoat preflight' for the full picture or pass --skip-preflight to override"))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w)
	return err
}

// renderPlanWarnings prints the "Cluster warnings:" section of the plan
// table. Silent when there are none.
func renderPlanWarnings(s *Styles, warnings []schema.PlanWarning, w io.Writer) error {
	if len(warnings) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, s.Bold.Render("Cluster warnings:")); err != nil {
		return err
	}
	for _, wn := range warnings {
		if _, err := fmt.Fprintf(w, "  %s  %s\n", StatusBadge(s, "warn"), s.Bold.Render(wn.ID)); err != nil {
			return err
		}
		if _, err := fmt.Fprintln(w, "          "+wn.Message); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w, "  "+s.Muted.Render("warnings do not change the steps or the plan hash; 'agentmoat apply' re-checks the live cluster"))
	return err
}

// summarizeNodePlacement condenses a NodePlacement into one table cell.
// "-" when not evaluated (spec check failed first), "unchecked" when the
// verifier could not read the RuntimeClass or the nodes, "ok" when every
// hosting node matches the selector, "N mismatched" otherwise.
func summarizeNodePlacement(np *schema.NodePlacement) string {
	if np == nil {
		return "-"
	}
	if !np.Checked {
		return "unchecked"
	}
	if len(np.Mismatched) == 0 {
		return "ok"
	}
	return strconv.Itoa(len(np.Mismatched)) + " mismatched"
}

// readinessWord maps the Ready bit to the badge word.
func readinessWord(ready bool) string {
	if ready {
		return "ready"
	}
	return "blocked"
}

// displayCluster returns "(unknown)" for an empty cluster name.
func displayCluster(c string) string {
	if c == "" {
		return "(unknown)"
	}
	return c
}

// formatKV renders a string map as k=v pairs in key order.
func formatKV(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}
