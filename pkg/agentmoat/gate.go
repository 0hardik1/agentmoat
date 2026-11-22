// Package agentmoat: the scan gate.
//
// A scan report is information. A gate turns it into a yes or a no that a
// pipeline can stop on: the exit code of `agentmoat scan`. The decision is
// made here, in the library, and not in cmd/, so every surface that wants a
// gate (the CLI today, a CI action that calls the CLI) gets the same one.
//
// There are three settings:
//
//   - incompatible (the default): fail when any workload is incompatible.
//     This is what scan has always done. It suits a terminal and an
//     inventory. It does not suit a pull-request check: nearly every real
//     repository has a CNI DaemonSet or a privileged agent that will never
//     move to gVisor, and a check that is red from the first day is a check
//     that people turn off.
//   - sandboxed: fail only when a workload that asks for the sandbox
//     RuntimeClass is incompatible. That is a statement about a decision
//     somebody already made ("this runs in gVisor"), so a failure is always
//     a real problem: a change is about to break a sandboxed workload, or
//     to put one in the sandbox that cannot run there.
//   - none: never fail. For a job that only publishes the report.
package agentmoat

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// FailOn selects when a scan fails. See the package comment in this file.
type FailOn string

// The FailOn values. They are the text of the --fail-on flag.
const (
	FailOnIncompatible FailOn = "incompatible"
	FailOnSandboxed    FailOn = "sandboxed"
	FailOnNone         FailOn = "none"
)

// ParseFailOn converts flag text to a FailOn. The empty string is the
// default, FailOnIncompatible.
func ParseFailOn(s string) (FailOn, error) {
	switch FailOn(s) {
	case "", FailOnIncompatible:
		return FailOnIncompatible, nil
	case FailOnSandboxed:
		return FailOnSandboxed, nil
	case FailOnNone:
		return FailOnNone, nil
	}
	return "", fmt.Errorf("unknown --fail-on value %q (valid: %s, %s, %s)",
		s, FailOnIncompatible, FailOnSandboxed, FailOnNone)
}

// GateResult is the answer of EvaluateGate.
type GateResult struct {
	// Failed is true when the scan must fail (exit code 2).
	Failed bool

	// Messages are lines for a person or a job log: which workloads
	// failed the gate, or why the gate had nothing to check. FailOnSandboxed
	// always gives one or more; the other two settings give none, so the
	// default scan prints what it always printed.
	Messages []string
}

// ErrUnrenderedInput is wrapped by the error EvaluateGate returns when
// FailOnSandboxed is asked about input that is not rendered.
var ErrUnrenderedInput = errors.New("the input is not rendered")

// EvaluateGate decides whether a scan fails under failOn.
//
// The error is non-nil in one case: FailOnSandboxed on a report whose source
// lists signs of unrendered input (a kustomization file, a partial object).
// The gate then refuses to answer. An overlay can add runtimeClassName to a
// base, or change the pod spec of a workload that has it, and a file scan
// applies no overlay; a "pass" would describe objects that are never
// deployed in that form. A refusal is exit code 1 (an error), not 2: the
// scan did not find a problem, it could not look.
func EvaluateGate(report *schema.ScanReport, failOn FailOn) (GateResult, error) {
	switch failOn {
	case FailOnNone:
		return GateResult{}, nil
	case FailOnSandboxed:
		return sandboxedGate(report)
	case FailOnIncompatible, "":
		for _, w := range report.Spec.Workloads {
			if w.Compatibility == schema.CompatibilityIncompatible {
				return GateResult{Failed: true}, nil
			}
		}
		return GateResult{}, nil
	}
	return GateResult{}, fmt.Errorf("unknown --fail-on value %q", failOn)
}

// identity is what makes two report rows "the same workload".
type identity struct{ kind, namespace, name string }

// sandboxedGate is EvaluateGate for FailOnSandboxed.
func sandboxedGate(report *schema.ScanReport) (GateResult, error) {
	runtimeClass := sandboxClassName(report.Metadata.RuntimeClassName)

	if src := report.Metadata.Source; src != nil && len(src.Unrendered) > 0 {
		return GateResult{}, fmt.Errorf("--fail-on %s cannot answer: %w (%s). "+
			"An overlay can add runtimeClassName to a workload or change its pod spec, and a file scan applies no overlay. "+
			"Scan the rendered objects: kustomize build <dir> | agentmoat scan -f - --fail-on %s",
			FailOnSandboxed, ErrUnrenderedInput, summarizeSigns(src.Unrendered), FailOnSandboxed)
	}

	// Work by identity, not by row. A manifest scan keeps every copy of a
	// workload that is defined twice (two overlays that each carry a full
	// Deployment). Which copy gets deployed is not knowable from the files,
	// so the safe reading is the union: the workload is sandboxed if any
	// copy asks for the class, and incompatible if any copy is.
	type state struct {
		sandboxed    bool
		incompatible []schema.WorkloadResult
	}
	states := map[identity]*state{}
	var order []identity
	otherClasses := map[string]int{}
	for _, w := range report.Spec.Workloads {
		id := identity{w.Kind, w.Namespace, w.Name}
		st := states[id]
		if st == nil {
			st = &state{}
			states[id] = st
			order = append(order, id)
		}
		switch {
		case w.RuntimeClassName == runtimeClass:
			st.sandboxed = true
		case w.RuntimeClassName != "":
			otherClasses[w.RuntimeClassName]++
		}
		if w.Compatibility == schema.CompatibilityIncompatible {
			st.incompatible = append(st.incompatible, w)
		}
	}

	var res GateResult
	sandboxed := 0
	for _, id := range order {
		st := states[id]
		if !st.sandboxed {
			continue
		}
		sandboxed++
		for _, w := range st.incompatible {
			res.Failed = true
			res.Messages = append(res.Messages, "  "+describeBlocked(w))
		}
	}

	switch {
	case res.Failed:
		head := fmt.Sprintf("gate failed: these workloads ask for RuntimeClass %q and cannot run in it:", runtimeClass)
		res.Messages = append([]string{head}, res.Messages...)
	case sandboxed == 0:
		// The dangerous quiet case: a cluster whose class is named
		// "gvisor-runsc" would pass this gate forever. Say that nothing
		// was checked, and show the names that are in use.
		msg := fmt.Sprintf("gate: no workload asks for RuntimeClass %q, so --fail-on %s checked nothing.", runtimeClass, FailOnSandboxed)
		if len(otherClasses) > 0 {
			msg += fmt.Sprintf(" RuntimeClass names in this scan: %s. If one of them is your sandbox, pass it with --runtime-class.",
				formatCounts(otherClasses))
		}
		res.Messages = []string{msg}
	default:
		res.Messages = []string{fmt.Sprintf("gate passed: %d workload(s) ask for RuntimeClass %q and none is incompatible.", sandboxed, runtimeClass)}
	}
	return res, nil
}

// describeBlocked renders one workload that failed the gate: identity, the
// rules with error severity, and where it is defined when that is known.
func describeBlocked(w schema.WorkloadResult) string {
	var rules []string
	for _, r := range w.Reasons {
		if r.Severity == schema.SeverityError {
			rules = append(rules, r.RuleID)
		}
	}
	sort.Strings(rules)
	line := fmt.Sprintf("%s %s", w.Kind, displayName(w.Namespace, w.Name))
	if len(rules) > 0 {
		line += ": " + strings.Join(rules, ", ")
	}
	if o := w.Origin; o != nil {
		if o.Line > 0 {
			line += fmt.Sprintf(" (%s:%d)", o.File, o.Line)
		} else {
			line += fmt.Sprintf(" (%s)", o.File)
		}
	}
	return line
}

// summarizeSigns shortens the Unrendered list for an error message: the
// first two entries and a count of the rest. The full list is in the report
// and was printed with the scan.
func summarizeSigns(signs []string) string {
	const show = 2
	if len(signs) <= show {
		return strings.Join(signs, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(signs[:show], "; "), len(signs)-show)
}
