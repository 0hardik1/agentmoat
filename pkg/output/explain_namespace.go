// Package output: deep namespace-explanation renderer.
//
// This file holds the table-mode renderer for the deep document produced
// by `agentmoat explain namespace <ns>` and `agentmoat explain workload
// <ns>/<name>`. Unlike the static-topic renderer (explain.go) which
// prints one markdown document, this renderer walks a structured
// schema.NamespaceExplanation and prints a header, a SUMMARY bar chart,
// and a per-workload section with structured evidence, glamour-rendered
// prose, and a hardcoded "what was checked" summary for the compatible
// case.
//
// The layout intentionally mirrors the SUMMARY pattern used by the scan
// renderer in table.go so an operator who has read a scan output already
// knows how to read this one. Section dividers and finding bullets give
// the long-form view structure that the table renderers don't need.

package output

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/glamour"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// dividerWidth is the cell width of the per-workload section divider.
// Chosen by eye: wide enough to be visually distinctive at typical
// terminal widths (80-120 cols), narrow enough not to wrap when an
// 80-col terminal is in use.
const dividerWidth = 66

// renderNamespaceExplanation renders the deep, per-workload explanation
// view for `agentmoat explain namespace <ns>` and `agentmoat explain
// workload <ns>/<name>`.
//
// Layout:
//   - Title line: "agentmoat explain namespace <name>"
//   - SUMMARY block: same horizontal bar chart pattern as scan
//   - Per-workload section: divider, badge + identity header,
//     recommendation, per-finding bullets with evidence + prose, then for
//     Compatible verdicts a "what was checked" summary.
//
// The function returns the first writer error it encounters; partial
// output may have already been flushed. That matches the convention
// used by the other renderers in this package.
func renderNamespaceExplanation(ns *schema.NamespaceExplanation, w io.Writer, useColor bool) error {
	s := NewStyles(useColor)

	// Title line. We always say "namespace <name>" even when the source
	// command was `explain workload <ns>/<name>`: the document still
	// carries a NamespaceExplanation with a single entry in Workloads,
	// and a uniform title keeps the renderer simple.
	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat explain namespace "+ns.Name)); err != nil {
		return err
	}

	// SUMMARY block. Same chart shape as scan: total headline and three
	// per-category rows. We reuse the scan symbol/color choices so an
	// operator who has read a scan summary already knows the legend.
	sum := ns.Summary
	if _, err := fmt.Fprintf(w, "%s  %d workloads\n", s.Bold.Render("SUMMARY"), sum.Total); err != nil {
		return err
	}
	if sum.Total == 0 {
		// Empty namespace short-circuit: no chart, no per-workload
		// section, just a muted placeholder. The CLI's exit code is
		// handled upstream; we just emit a useful line so a piped
		// caller (or a confused operator) sees something.
		_, err := fmt.Fprintln(w, s.Muted.Render("(no workloads in this namespace)"))
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

	// Per-workload sections, in the order the orchestrator built them
	// (the schema is responsible for deterministic order). For each
	// workload we print:
	//   1. Divider (66 cells of '─', muted).
	//   2. Identity header line with the verdict badge and an
	//      upper-cased compatibility tag.
	//   3. Recommendation (if non-empty) and Overhead (if non-empty)
	//      lines below the header.
	//   4. Either Findings (each with evidence + prose) or, for
	//      Compatible, a "what was checked" summary.
	for i := range ns.Workloads {
		if err := renderWorkloadSection(s, &ns.Workloads[i], w, useColor); err != nil {
			return err
		}
	}
	return nil
}

// renderWorkloadSection prints one workload's complete deep-explanation
// block: divider + header, findings (each with evidence + prose), the
// per-verdict tail (Checked summary for Compatible, brief "other checks"
// line for Review/Incompatible), and the optional Overhead line. Pulled
// out of renderNamespaceExplanation so the outer function reads as a
// straight section walker.
func renderWorkloadSection(s *Styles, wl *schema.WorkloadExplanation, w io.Writer, useColor bool) error {
	if err := renderWorkloadHeader(s, wl, w); err != nil {
		return err
	}
	for j := range wl.Findings {
		if err := renderWorkloadFinding(s, &wl.Findings[j], w, useColor); err != nil {
			return err
		}
	}
	if err := renderWorkloadVerdictTail(s, wl, w); err != nil {
		return err
	}
	if wl.Overhead != "" {
		if _, err := fmt.Fprintf(w, "  %s %s\n", s.Bold.Render("Expected overhead:"), wl.Overhead); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintln(w)
	return err
}

// renderWorkloadVerdictTail prints the per-verdict closing block: a full
// "what was checked" summary for Compatible, or a one-line "other
// checks (did not fire)" recap for Review and Incompatible.
func renderWorkloadVerdictTail(s *Styles, wl *schema.WorkloadExplanation, w io.Writer) error {
	if wl.Compatibility == schema.CompatibilityCompatible {
		return renderWorkloadChecked(s, wl.Checked, w)
	}
	if len(wl.Checked) == 0 {
		return nil
	}
	line := fmt.Sprintf("Other checks (did not fire): %d rules - see JSON for details", len(wl.Checked))
	_, err := fmt.Fprintln(w, "  "+s.Muted.Render(line))
	return err
}

// renderWorkloadHeader prints the divider, the identity line (badge +
// kind + namespace/name + right-aligned compatibility tag), and the
// optional Recommendation line. Extracted as its own function so the
// main loop in renderNamespaceExplanation reads as the section walker
// it is, not a 50-line per-iteration block.
func renderWorkloadHeader(s *Styles, wl *schema.WorkloadExplanation, w io.Writer) error {
	div := strings.Repeat("─", dividerWidth)
	if _, err := fmt.Fprintln(w, s.Muted.Render(div)); err != nil {
		return err
	}
	badge := StatusBadge(s, string(wl.Compatibility))
	tag := strings.ToUpper(string(wl.Compatibility))
	// Identity line: "<badge> <Kind> <ns>/<name>" left, tag right. We
	// pad the tag in a 12-cell column so the tag column lines up
	// regardless of workload kind. The body of the identity line goes
	// raw (no ANSI from the right-align) because lipgloss is
	// width-aware and the badge already has styling.
	left := fmt.Sprintf("%s %s %s/%s", badge, wl.Kind, wl.Namespace, wl.Name)
	if _, err := fmt.Fprintf(w, "%s  %s\n", left, s.Bold.Render(fmt.Sprintf("%12s", tag))); err != nil {
		return err
	}
	if wl.Recommendation != "" {
		if _, err := fmt.Fprintf(w, "  %s %s\n", s.Bold.Render("Recommendation:"), wl.Recommendation); err != nil {
			return err
		}
	}
	return nil
}

// renderWorkloadFinding renders one RuleFinding: bullet, title, brief,
// evidence block, glamour-rendered prose, remediation link.
//
// The output for one finding looks like:
//
//	▸ <ruleID> (<severity>)  <Title>
//	  Evidence:
//	    <evidence lines>
//
//	  <prose body, indented two spaces>
//
//	  Remediation: <URL>
//
// All indentation is two spaces so the finding visually nests under the
// per-workload header above it.
func renderWorkloadFinding(s *Styles, f *schema.RuleFinding, w io.Writer, useColor bool) error {
	// Bullet line. We color the severity tag by mapping to the same
	// palette StatusBadge uses, so "(error)" reads as red. The rule ID
	// is left plain (no style) to keep grep-ability intact.
	sev := string(f.Severity)
	var sevStyled string
	switch f.Severity {
	case schema.SeverityError:
		sevStyled = s.Danger.Render(sev)
	case schema.SeverityWarn:
		sevStyled = s.Warn.Render(sev)
	default:
		sevStyled = s.Info.Render(sev)
	}
	bullet := s.Bold.Render(symbolFinding)
	if _, err := fmt.Fprintf(w, "%s %s (%s)  %s\n", bullet, f.RuleID, sevStyled, s.Bold.Render(f.Title)); err != nil {
		return err
	}

	// Evidence block. Skipped when every field of f.Evidence is empty:
	// renderEvidence does the empty check itself and returns nil
	// without writing.
	if err := renderEvidence(s, &f.Evidence, w); err != nil {
		return err
	}

	// Prose body. When useColor is true we run the markdown through
	// glamour and indent each line of the result; otherwise we print
	// the raw markdown indented verbatim. Glamour failures degrade to
	// raw markdown rather than erroring, matching writeTopicContent.
	if f.WhyMarkdown != "" {
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
		prose := f.WhyMarkdown
		if useColor {
			if rendered, err := glamour.Render(prose, "auto"); err == nil {
				prose = rendered
			}
		}
		if err := writeIndented(w, prose, "  "); err != nil {
			return err
		}
	}

	if f.RemediationURL != "" {
		if _, err := fmt.Fprintf(w, "  %s %s\n", s.Bold.Render("Remediation:"), f.RemediationURL); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w); err != nil {
		return err
	}
	return nil
}

// renderEvidence formats the Evidence struct as a multi-line block
// indented under "Evidence:". Returns nil without writing when Evidence
// is empty across every field so a rule with prose-only output (no
// concrete pod evidence) does not get a blank "Evidence:" header.
//
// Each field of Evidence corresponds to a different concrete fact shape
// (host namespaces, container/capability pairs, hostPath mounts, etc.).
// We emit the lines in a fixed order so the same finding renders the
// same way every time.
func renderEvidence(s *Styles, e *schema.Evidence, w io.Writer) error {
	if e == nil || evidenceEmpty(e) {
		return nil
	}
	// Build the whole block into a strings.Builder (which never errors)
	// so we only have one real I/O write at the end. Keeps per-section
	// loops branch-free.
	var b strings.Builder
	fmt.Fprintf(&b, "  %s\n", s.Bold.Render("Evidence:"))
	// Host namespaces: "pod.spec.hostNetwork = true" etc. The schema
	// stores the bare value (e.g. "hostNetwork"); we map it to the
	// pod-spec dotted form so the output reads as a literal pod field.
	// If the orchestrator pre-formatted a value with "=" we trust it.
	for _, hn := range e.HostNamespaces {
		line := hn
		if !strings.Contains(line, "=") {
			line = fmt.Sprintf("pod.spec.%s = true", hn)
		}
		fmt.Fprintln(&b, "    "+line)
	}
	for _, c := range e.PrivilegedContainers {
		fmt.Fprintf(&b, "    Container %q runs privileged\n", c)
	}
	for _, h := range e.Capabilities {
		fmt.Fprintf(&b, "    Container %q has capability %s\n", h.Container, h.Capability)
	}
	for _, h := range e.HostPaths {
		mounts := ""
		if len(h.Containers) > 0 {
			quoted := make([]string, 0, len(h.Containers))
			for _, c := range h.Containers {
				quoted = append(quoted, fmt.Sprintf("%q", c))
			}
			mounts = fmt.Sprintf("  (mounted by container %s)", strings.Join(quoted, ", "))
		}
		fmt.Fprintf(&b, "    Volume %q -> %s%s\n", h.Volume, h.Path, mounts)
	}
	for _, h := range e.ImageMatches {
		fmt.Fprintf(&b, "    Container %q image %q matches hint %q\n", h.Container, h.Image, h.HintPattern)
	}
	for _, h := range e.GPURequests {
		fmt.Fprintf(&b, "    Container %q requests %s = %s\n", h.Container, h.Resource, h.Quantity)
	}
	for _, h := range e.EnvVars {
		fmt.Fprintf(&b, "    Container %q env %s=%s\n", h.Container, h.Name, h.Value)
	}
	for _, h := range e.Annotations {
		fmt.Fprintf(&b, "    Annotation %s=%s\n", h.Key, h.Value)
	}
	for _, h := range e.CSIDrivers {
		fmt.Fprintf(&b, "    Volume %q uses CSI driver %q\n", h.Volume, h.Driver)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// evidenceEmpty reports whether every slice in e is empty. Used by
// renderEvidence to skip the block header when no concrete facts are
// available.
func evidenceEmpty(e *schema.Evidence) bool {
	return len(e.HostNamespaces) == 0 &&
		len(e.PrivilegedContainers) == 0 &&
		len(e.Capabilities) == 0 &&
		len(e.HostPaths) == 0 &&
		len(e.ImageMatches) == 0 &&
		len(e.GPURequests) == 0 &&
		len(e.EnvVars) == 0 &&
		len(e.Annotations) == 0 &&
		len(e.CSIDrivers) == 0
}

// checkedBucket is one row in the "what was checked" summary printed
// for compatible workloads. Each bucket collapses several rule IDs
// (e.g. host-network, host-pid, host-ipc) into one human-readable line
// so the operator sees the categories that did not fire, not a wall of
// rule IDs.
type checkedBucket struct {
	Label string
	Line  string
}

// checkedBuckets is the fixed list of categories the renderer prints in
// the compatible-verdict summary. Hardcoding the list (rather than
// deriving it from the rule registry) is deliberate: the renderer
// output is a UX statement, not a 1:1 rule audit, and the JSON view
// carries the full Checked slice for callers that need the audit
// shape.
var checkedBuckets = []checkedBucket{
	{Label: "host namespaces", Line: "host namespaces: none used"},
	{Label: "capabilities", Line: "capabilities: none risky (no CAP_NET_RAW, CAP_BPF, CAP_PERFMON, CAP_SYS_ADMIN)"},
	{Label: "privileged", Line: "privileged: no privileged containers"},
	{Label: "volumes", Line: "volumes: no hostPath, no FUSE, no /dev/kvm"},
	{Label: "images", Line: "images: no risky hints (no eBPF/cilium/tetragon/falco, no GPU)"},
	{Label: "annotations", Line: "annotations: none flagged (no io_uring, no raw-socket override)"},
}

// renderWorkloadChecked renders the "what was checked" summary for a
// compatible workload. The checked slice from the schema is currently
// unused by this renderer: presence of the workload in the Compatible
// bucket already implies all rules didn't fire, and a fixed-text
// summary reads more cleanly than a dynamic list. JSON consumers can
// still see the full Checked array.
func renderWorkloadChecked(s *Styles, _ []schema.RuleCheck, w io.Writer) error {
	if _, err := fmt.Fprintf(w, "  %s\n", s.Bold.Render("What was checked (no blocking rules fired):")); err != nil {
		return err
	}
	for _, b := range checkedBuckets {
		if _, err := fmt.Fprintf(w, "    %s %s\n", symbolDot, b.Line); err != nil {
			return err
		}
	}
	return nil
}

// writeIndented writes content to w with prefix prepended to every
// non-empty line. Empty lines (after trimming the trailing newline)
// pass through unchanged so paragraph breaks survive. Used to nest
// glamour output under the finding bullet.
//
// The function is deliberately simple: it splits on '\n' and rejoins
// rather than streaming, because glamour output is small (one finding
// at a time) and the simpler form is easier to reason about.
func writeIndented(w io.Writer, content, prefix string) error {
	// strip exactly one trailing newline (glamour and our raw
	// markdown both end with one) so we don't add a phantom indented
	// blank line at the bottom.
	content = strings.TrimRight(content, "\n")
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if line == "" {
			if _, err := fmt.Fprintln(w); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintln(w, prefix+line); err != nil {
			return err
		}
	}
	return nil
}
