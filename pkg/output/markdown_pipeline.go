// Package output: Markdown views of the documents that the pipeline verbs
// write (plan, apply, rollback, verify, preflight).
//
// Each view is for the job summary of one pipeline job: the approver reads
// the plan summary before the approval, the apply summary says what was
// changed, the verify summary says whether the pods run in the sandbox.
//
// The layout rule is the same in all of them: the headline first, then the
// rows that need a person (failed steps, mismatches, error findings), then
// the rest in a collapsed block. A capped table can then never hide a
// failure behind fifty rows that are fine.
//
// The escaping rules are in markdown.go. Every string from a document goes
// through mdCode, mdCodeCell, mdText, or a fenced block.

package output

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// mdChips writes one line of "label: value" facts under the title.
func mdChips(d *mdDoc, chips ...string) {
	var kept []string
	for _, c := range chips {
		if c != "" {
			kept = append(kept, c)
		}
	}
	d.linef("%s", strings.Join(kept, " · "))
	d.blank()
}

// mdChip is one "label: `value`" fact, or "" when the value is empty.
func mdChip(label, value string) string {
	if value == "" {
		return ""
	}
	return label + ": " + mdCode(value)
}

// targetCells renders a workload reference as three table cells.
func targetCells(t schema.WorkloadRef) []string {
	return []string{mdCodeCell(t.Namespace), mdCodeCell(t.Kind), mdCodeCell(t.Name)}
}

// ---------------------------------------------------------------------------
// MigrationPlan.
// ---------------------------------------------------------------------------

// writeMigrationPlanMarkdown is the MigrationPlan view: the hash that the
// apply job must use, the warnings, the ordered steps, and what the plan
// leaves out.
func writeMigrationPlanMarkdown(d *mdDoc, plan *schema.MigrationPlan) {
	sum := plan.Spec.Summary
	d.heading(2, "agentmoat plan")
	mdChips(d,
		mdChip("Plan hash", plan.Metadata.PlanHash),
		mdChip("RuntimeClass", plan.Spec.Options.RuntimeClassName),
		mdChip("Cluster", plan.Metadata.Cluster),
	)
	d.table(
		[]string{"Workloads", "Included", "Excluded"},
		map[int]bool{0: true, 1: true, 2: true},
		[][]string{{fmt.Sprint(sum.Total), fmt.Sprint(sum.Included), fmt.Sprint(sum.Excluded)}},
	)

	if len(plan.Spec.Warnings) > 0 {
		d.heading(3, fmt.Sprintf("Warnings (%d)", len(plan.Spec.Warnings)))
		shown := plan.Spec.Warnings
		if len(shown) > mdMaxRows {
			shown = shown[:mdMaxRows]
		}
		for _, w := range shown {
			d.linef("- ⚠️ %s: %s", mdCode(w.ID), mdText(w.Message))
		}
		d.blank()
		d.leftOut(len(plan.Spec.Warnings) - len(shown))
	}

	d.heading(3, fmt.Sprintf("Steps (%d)", len(plan.Spec.Steps)))
	if len(plan.Spec.Steps) == 0 {
		d.linef("The plan has no steps.")
		d.blank()
	} else {
		rows := make([][]string, 0, len(plan.Spec.Steps))
		for _, st := range plan.Spec.Steps {
			row := []string{fmt.Sprint(st.Order)}
			row = append(row, targetCells(st.Target)...)
			row = append(row, fmt.Sprint(st.RiskScore), mdCodeCell(st.WaitFor), mdText(st.Notes))
			rows = append(rows, row)
		}
		d.table([]string{"#", "Namespace", "Kind", "Name", "Risk", "Wait for", "Notes"},
			map[int]bool{0: true, 4: true}, rows)
	}

	if len(plan.Spec.Excluded) > 0 {
		d.details(fmt.Sprintf("Excluded (%d)", len(plan.Spec.Excluded)))
		rows := make([][]string, 0, len(plan.Spec.Excluded))
		for _, e := range plan.Spec.Excluded {
			row := targetCells(e.Target)
			row = append(row, mdLabel(string(e.Compatibility)), mdText(e.Reason))
			rows = append(rows, row)
		}
		d.table([]string{"Namespace", "Kind", "Name", "Compatibility", "Reason"}, nil, rows)
		d.endDetails()
	}
}

// ---------------------------------------------------------------------------
// ApplyResult and RollbackResult.
// ---------------------------------------------------------------------------

// writeStepsMarkdown is the view of an ApplyResult or a RollbackResult
// (action is "apply" or "rollback"): whether anything was changed, the
// counts, the steps that failed or were skipped, then the rest.
func writeStepsMarkdown(d *mdDoc, action string, meta schema.ApplyMetadata, sum schema.ApplySummary,
	steps []schema.StepResult, preflight []schema.PreflightFinding) {
	title := "agentmoat " + action
	if meta.DryRun {
		title += " (dry run)"
	}
	d.heading(2, title)

	// Say first whether the cluster was changed. It is the question a
	// person has when they open the summary of an apply job.
	switch {
	case meta.DryRun:
		d.linef("➖ **Dry run.** Nothing was changed. Pass `--dry-run=false` to %s.", action)
	case sum.Failed > 0:
		d.linef("❌ **%d of %d step(s) failed.**", sum.Failed, sum.Total)
	case sum.Skipped > 0:
		d.linef("⚠️ **%d of %d step(s) were skipped.**", sum.Skipped, sum.Total)
	default:
		d.linef("✅ **All %d step(s) are done.**", sum.Total)
	}
	d.blank()
	mdChips(d, mdChip("Plan hash", meta.PlanHash), mdChip("Cluster", meta.Cluster))

	d.table(
		[]string{"Steps", "Applied", "Already applied", "Skipped", "Failed"},
		map[int]bool{0: true, 1: true, 2: true, 3: true, 4: true},
		[][]string{{
			fmt.Sprint(sum.Total), fmt.Sprint(sum.Applied), fmt.Sprint(sum.AlreadyApplied),
			fmt.Sprint(sum.Skipped), fmt.Sprint(sum.Failed),
		}},
	)

	if len(preflight) > 0 {
		d.heading(3, fmt.Sprintf("Preflight findings (%d)", len(preflight)))
		writeFindingsTable(d, preflight)
	}

	var attention, done []schema.StepResult
	for _, st := range steps {
		if st.Status == schema.StepStatusFailed || st.Status == schema.StepStatusSkipped {
			attention = append(attention, st)
		} else {
			done = append(done, st)
		}
	}
	if len(attention) > 0 {
		d.heading(3, fmt.Sprintf("Failed or skipped (%d)", len(attention)))
		writeStepTable(d, attention, meta.DryRun)
	}
	if len(done) > 0 {
		d.details(fmt.Sprintf("Other steps (%d)", len(done)))
		writeStepTable(d, done, meta.DryRun)
		d.endDetails()
	}
}

// writeStepTable writes one row for each step result. The note is the
// error, or for a dry run the patch that would be sent.
func writeStepTable(d *mdDoc, steps []schema.StepResult, dryRun bool) {
	rows := make([][]string, 0, len(steps))
	for _, st := range steps {
		note := mdText(st.Error)
		if st.Error == "" && dryRun && st.Patch != "" {
			note = mdCodeCell(compactPatchPreview(st.Patch))
		}
		row := []string{fmt.Sprint(st.Order)}
		row = append(row, targetCells(st.Target)...)
		row = append(row, mdLabel(string(st.Status)), note)
		rows = append(rows, row)
	}
	d.table([]string{"#", "Namespace", "Kind", "Name", "Status", "Note"}, map[int]bool{0: true}, rows)
}

// ---------------------------------------------------------------------------
// VerifyReport.
// ---------------------------------------------------------------------------

// writeVerifyReportMarkdown is the VerifyReport view: whether every
// workload runs with the expected RuntimeClass, the ones that do not, then
// the rest.
func writeVerifyReportMarkdown(d *mdDoc, report *schema.VerifyReport) {
	sum := report.Spec.Summary
	wait := report.Metadata.Wait
	d.heading(2, "agentmoat verify")
	switch {
	case sum.Mismatch+sum.Error > 0:
		// The headline says when the result is after a wait. A job
		// summary shows no stderr: "not verified" after one look means
		// "look again", and after a wait that timed out it means "roll
		// back". The reader must not have to guess which one this is.
		d.linef("❌ **%d of %d workload(s) are not verified%s.**", sum.Mismatch+sum.Error, sum.Total, verifyWaitClause(wait))
	case sum.Total == 0:
		d.linef("⚠️ **Nothing was verified.** The plan has no steps.")
	case wait != nil:
		d.linef("✅ **All %d workload(s) are verified, and their pods stayed Ready.**", sum.Total)
	default:
		d.linef("✅ **All %d workload(s) are verified.**", sum.Total)
	}
	d.blank()
	probe := "no"
	if report.Metadata.InPodProbe {
		probe = "yes"
	}
	chips := []string{
		mdChip("Plan hash", report.Metadata.PlanHash),
		mdChip("Cluster", report.Metadata.Cluster),
		"In-pod probe: " + probe,
	}
	if wait != nil {
		chips = append(chips, "Wait: "+describeWait(wait))
	}
	mdChips(d, chips...)
	d.table(
		[]string{"Workloads", "OK", "Mismatch", "Error"},
		map[int]bool{0: true, 1: true, 2: true, 3: true},
		[][]string{{fmt.Sprint(sum.Total), fmt.Sprint(sum.OK), fmt.Sprint(sum.Mismatch), fmt.Sprint(sum.Error)}},
	)

	var attention, ok []schema.VerifyResult
	for _, r := range report.Spec.Results {
		if r.Status == schema.VerifyStatusOK {
			ok = append(ok, r)
		} else {
			attention = append(attention, r)
		}
	}
	if len(attention) > 0 {
		d.heading(3, fmt.Sprintf("Mismatch or error (%d)", len(attention)))
		writeVerifyTable(d, attention, wait != nil)
	}
	if len(ok) > 0 {
		d.details(fmt.Sprintf("OK (%d)", len(ok)))
		writeVerifyTable(d, ok, wait != nil)
		d.endDetails()
	}
}

// verifyWaitClause is the end of the "not verified" headline for a verify
// with a wait time: how the wait ended. It is empty with no wait.
func verifyWaitClause(w *schema.VerifyWait) string {
	if w == nil {
		return ""
	}
	requested := time.Duration(w.RequestedSeconds) * time.Second
	if w.Interrupted {
		return fmt.Sprintf(". The wait of %s was interrupted after %s",
			requested, time.Duration(w.WaitedSeconds)*time.Second)
	}
	return fmt.Sprintf(" after a wait of %s", requested)
}

// writeVerifyTable writes one row for each verify result. The Ready column
// is there only after a wait: without one the verifier does not read
// readiness.
func writeVerifyTable(d *mdDoc, results []schema.VerifyResult, waited bool) {
	headers := []string{"#", "Namespace", "Kind", "Name", "Status", "Expected", "Actual", "Probe", "Node"}
	if waited {
		headers = append(headers, "Ready")
	}
	headers = append(headers, "Message")
	rows := make([][]string, 0, len(results))
	for _, r := range results {
		row := []string{fmt.Sprint(r.Order)}
		row = append(row, targetCells(r.Target)...)
		row = append(row,
			mdLabel(string(r.Status)),
			mdCodeCell(r.Expected),
			mdCodeCell(r.Actual),
			mdText(summarizeProbe(r.Probe)),
			mdText(summarizeNodePlacement(r.NodePlacement)),
		)
		if waited {
			row = append(row, mdText(summarizeReadiness(r)))
		}
		// The message of a step that is not Ready has text from the
		// cluster in it: a pod name, a container name, a scheduler
		// message. mdText makes it inert.
		row = append(row, mdText(r.Message))
		rows = append(rows, row)
	}
	d.table(headers, map[int]bool{0: true}, rows)
}

// ---------------------------------------------------------------------------
// PreflightReport.
// ---------------------------------------------------------------------------

// writePreflightReportMarkdown is the PreflightReport view (also the output
// of `probe nvproxy`): whether the cluster can host the RuntimeClass, the
// counts, the findings, and the two facts a person checks first.
func writePreflightReportMarkdown(d *mdDoc, report *schema.PreflightReport) {
	sum := report.Spec.Summary
	d.heading(2, "agentmoat preflight")
	if sum.Ready {
		d.linef("✅ **Ready.** Pods that ask for RuntimeClass %s can be scheduled.", mdCode(report.Metadata.RuntimeClassName))
	} else {
		d.linef("❌ **Blocked.** %d error finding(s). `agentmoat apply` refuses to run on this cluster.", sum.Error)
	}
	d.blank()
	chips := []string{
		mdChip("RuntimeClass", report.Metadata.RuntimeClassName),
		mdChip("Cluster", report.Metadata.Cluster),
	}
	if pm := report.Metadata.Probe; pm != nil {
		chips = append(chips, "Probe: "+mdText(describeProbe(pm)))
	}
	mdChips(d, chips...)
	d.table(
		[]string{"Findings", "Error", "Warn", "Info"},
		map[int]bool{0: true, 1: true, 2: true, 3: true},
		[][]string{{fmt.Sprint(sum.Total), fmt.Sprint(sum.Error), fmt.Sprint(sum.Warn), fmt.Sprint(sum.Info)}},
	)

	if len(report.Spec.Findings) > 0 {
		d.heading(3, fmt.Sprintf("Findings (%d)", len(report.Spec.Findings)))
		writeFindingsTable(d, report.Spec.Findings)
	}

	facts := report.Spec.Facts
	d.heading(3, "Facts")
	rc := facts.RuntimeClass
	if rc.Found {
		selector := "none"
		if len(rc.NodeSelector) > 0 {
			selector = mdCode(formatKV(rc.NodeSelector))
		}
		d.linef("- RuntimeClass %s: handler %s, nodeSelector %s.", mdCode(rc.Name), mdCode(rc.Handler), selector)
	} else {
		d.linef("- RuntimeClass %s: not found.", mdCode(rc.Name))
	}
	n := facts.Nodes
	d.linef("- Nodes: %d total, %d Ready, %d match the nodeSelector, %d match and are Ready.",
		n.Total, n.Ready, n.MatchingSelector, n.MatchingAndReady)
	writeGPUFactsMarkdown(d, facts.GPU)
	d.blank()
}

// writeGPUFactsMarkdown writes the GPU facts: the node counts, the result of
// the nvproxy probe when there is one, and one line for each group of nodes
// with the same card and driver. It writes nothing for a cluster with no GPU
// nodes.
//
// This is the result of `probe nvproxy`, so it must be in the Markdown form
// of that command. The product and driver names are node labels: text from
// the cluster, so they go in code spans like all other values.
func writeGPUFactsMarkdown(d *mdDoc, g *schema.GPUFacts) {
	if g == nil {
		return
	}
	nvproxy := "nvproxy is not probed (run `agentmoat probe nvproxy`)"
	if g.Nvproxy != nil {
		nvproxy = fmt.Sprintf("nvproxy: runsc %s, %d supported driver(s)", mdCode(g.Nvproxy.RunscVersion), len(g.Nvproxy.SupportedDrivers))
	}
	d.linef("- GPU: %d node(s), %d match the nodeSelector, %d with MIG; %s.", g.Nodes, g.MatchingNodes, g.MIGNodes, nvproxy)

	groups := g.Groups
	leftOut := 0
	if len(groups) > mdMaxRows {
		leftOut = len(groups) - mdMaxRows
		groups = groups[:mdMaxRows]
	}
	for _, grp := range groups {
		product := "no GFD labels"
		if grp.Product != "" {
			product = mdCode(grp.Product)
		}
		mig := ""
		if grp.MIG {
			mig = ", MIG"
		}
		d.linef("  - %s, driver %s%s: %d node(s) (%d match), card support %s, driver support %s",
			product, mdCode(grp.Driver), mig, grp.Nodes, grp.MatchingNodes,
			mdCode(string(grp.ProductSupport)), mdCode(string(grp.DriverSupport)))
	}
	if leftOut > 0 {
		d.linef("  - _%d more not shown. The JSON output has all of them._", leftOut)
	}
}

// writeFindingsTable writes preflight findings, error findings first.
func writeFindingsTable(d *mdDoc, findings []schema.PreflightFinding) {
	// A stable sort by severity keeps the document order inside each
	// severity, and puts the findings that block an apply at the top of a
	// table that may be capped.
	sorted := make([]schema.PreflightFinding, len(findings))
	copy(sorted, findings)
	sort.SliceStable(sorted, func(i, j int) bool {
		return severityRank(sorted[i].Severity) < severityRank(sorted[j].Severity)
	})
	rows := make([][]string, 0, len(sorted))
	for _, f := range sorted {
		rows = append(rows, []string{
			mdLabel(string(f.Severity)),
			mdCodeCell(f.ID),
			mdText(f.Message),
			mdText(f.Remediation),
		})
	}
	d.table([]string{"Severity", "ID", "Message", "What to do"}, nil, rows)
}
