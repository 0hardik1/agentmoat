// Package preflight: findings.
//
// Evaluate is a pure function from ClusterFacts to a sorted list of
// findings. Each finding has a stable ID (below), a severity, a message
// that quotes the numbers behind the verdict, and a remediation. Only
// error-severity findings block apply.
package preflight

import (
	"fmt"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// Finding IDs. These are a public surface (JSON output, docs/preflight.md,
// scripts that branch on them); do not rename without a deprecation path.
const (
	// FindingRuntimeClassMissing: no RuntimeClass with the requested name.
	FindingRuntimeClassMissing = "runtimeclass-missing"

	// FindingRuntimeClassNoNodeSelector: the RuntimeClass exists but has no
	// scheduling.nodeSelector, so admission cannot steer pods to runsc
	// nodes; a patched pod may land on a runc node with a spec that lies.
	FindingRuntimeClassNoNodeSelector = "runtimeclass-no-node-selector"

	// FindingRuntimeClassNoMatchingNodes: the selector matches no node.
	FindingRuntimeClassNoMatchingNodes = "runtimeclass-no-matching-nodes"

	// FindingRuntimeClassNoReadyMatchingNodes: nodes match but none is Ready.
	FindingRuntimeClassNoReadyMatchingNodes = "runtimeclass-no-ready-matching-nodes"

	// FindingRuntimeClassTaintWithoutToleration: matching nodes carry a
	// NoSchedule/NoExecute taint the RuntimeClass does not tolerate. Error
	// when every matching node is affected, warn when only some are.
	FindingRuntimeClassTaintWithoutToleration = "runtimeclass-taint-without-toleration"

	// FindingEKSAutoModeNodes: candidate nodes are EKS Auto Mode managed
	// instances. Error when all candidates are, warn when some are.
	FindingEKSAutoModeNodes = "eks-auto-mode-nodes"

	// FindingBottlerocketNodes: candidate nodes run Bottlerocket (outside
	// Auto Mode). Same error/warn split.
	FindingBottlerocketNodes = "bottlerocket-nodes"

	// FindingRuntimeClassNoOverhead: informational; overhead.podFixed is
	// unset so the scheduler does not account for the Sentry's footprint.
	FindingRuntimeClassNoOverhead = "runtimeclass-no-overhead"
)

// Evaluate derives findings from facts. Pure and deterministic: the output
// is sorted error -> warn -> info, then by ID. A nil facts value yields an
// empty (non-nil) slice so callers never nil-check.
func Evaluate(facts *schema.ClusterFacts) []schema.PreflightFinding {
	out := []schema.PreflightFinding{}
	if facts == nil {
		return out
	}
	out = append(out, runtimeClassFindings(facts)...)
	out = append(out, taintFindings(facts)...)
	out = append(out, platformFindings(facts)...)
	sortFindings(out)
	return out
}

// IsBlocking reports whether any finding has error severity.
func IsBlocking(findings []schema.PreflightFinding) bool {
	for _, f := range findings {
		if f.Severity == schema.SeverityError {
			return true
		}
	}
	return false
}

// Summarize buckets findings by severity. Ready is "no errors".
func Summarize(findings []schema.PreflightFinding) schema.PreflightSummary {
	s := schema.PreflightSummary{Total: len(findings)}
	for _, f := range findings {
		switch f.Severity {
		case schema.SeverityError:
			s.Error++
		case schema.SeverityWarn:
			s.Warn++
		case schema.SeverityInfo:
			s.Info++
		}
	}
	s.Ready = s.Error == 0
	return s
}

// runtimeClassFindings covers the RuntimeClass object and whether its
// selector lands on a Ready node. The cases are mutually exclusive and
// ordered from "nothing to check" to "almost there".
func runtimeClassFindings(f *schema.ClusterFacts) []schema.PreflightFinding {
	rc := f.RuntimeClass
	n := f.Nodes
	var out []schema.PreflightFinding

	switch {
	case !rc.Found:
		out = append(out, schema.PreflightFinding{
			ID:       FindingRuntimeClassMissing,
			Severity: schema.SeverityError,
			Message: fmt.Sprintf("RuntimeClass %q does not exist; pods requesting it are rejected at admission",
				rc.Name),
			Remediation: "kubectl apply -f deploy/runtimeclass.yaml (edit metadata.name to match), or pass --runtime-class <name>",
		})
	case len(rc.NodeSelector) == 0:
		out = append(out, schema.PreflightFinding{
			ID:       FindingRuntimeClassNoNodeSelector,
			Severity: schema.SeverityError,
			Message: fmt.Sprintf("RuntimeClass %q has no scheduling.nodeSelector; admission cannot steer its pods onto runsc nodes, "+
				"so a pod may run on a runc node while its spec says %q", rc.Name, rc.Name),
			Remediation: "add scheduling.nodeSelector (for example runtime: gvisor) to the RuntimeClass and label the gVisor nodes to match; see docs/runtimeclass-101.md",
		})
	case n.MatchingSelector == 0:
		out = append(out, schema.PreflightFinding{
			ID:       FindingRuntimeClassNoMatchingNodes,
			Severity: schema.SeverityError,
			Message: fmt.Sprintf("0 of %d nodes match RuntimeClass %q nodeSelector %s",
				n.Total, rc.Name, formatSelector(rc.NodeSelector)),
			Remediation: "label the nodes that ship runsc to match the selector, or add a node group built from the agentmoat AMI (packer/); see docs/eks-deployment.md",
		})
	case n.MatchingAndReady == 0:
		out = append(out, schema.PreflightFinding{
			ID:       FindingRuntimeClassNoReadyMatchingNodes,
			Severity: schema.SeverityError,
			Message: fmt.Sprintf("%d node(s) match RuntimeClass %q nodeSelector %s but none is Ready",
				n.MatchingSelector, rc.Name, formatSelector(rc.NodeSelector)),
			Remediation: "wait for or repair the matching nodes (kubectl describe node " + strings.Join(n.MatchingNames, " ") + ")",
		})
	}

	if rc.Found && len(rc.Overhead) == 0 {
		out = append(out, schema.PreflightFinding{
			ID:       FindingRuntimeClassNoOverhead,
			Severity: schema.SeverityInfo,
			Message: fmt.Sprintf("RuntimeClass %q sets no overhead.podFixed; the scheduler will not account for the gVisor Sentry's resident footprint",
				rc.Name),
			Remediation: "consider overhead.podFixed (deploy/runtimeclass.yaml uses memory 140Mi, cpu 250m)",
		})
	}
	return out
}

// taintFindings covers matching nodes whose taints the RuntimeClass does
// not tolerate. Nothing to say when no node matches (the selector finding
// already covers that).
func taintFindings(f *schema.ClusterFacts) []schema.PreflightFinding {
	n := f.Nodes
	if n.MatchingSelector == 0 || n.MatchingTaintedWithoutToleration == 0 {
		return nil
	}
	sev := schema.SeverityWarn
	if n.MatchingTaintedWithoutToleration == n.MatchingSelector {
		sev = schema.SeverityError
	}
	return []schema.PreflightFinding{{
		ID:       FindingRuntimeClassTaintWithoutToleration,
		Severity: sev,
		Message: fmt.Sprintf("%d of %d node(s) matching RuntimeClass %q carry a NoSchedule/NoExecute taint that its %d scheduling.tolerations do not tolerate",
			n.MatchingTaintedWithoutToleration, n.MatchingSelector, f.RuntimeClass.Name, f.RuntimeClass.Tolerations),
		Remediation: "add the taint to RuntimeClass scheduling.tolerations (preferred; admission merges it into every pod), " +
			"or re-plan with --add-toleration when the taint is exactly runtime=gvisor:NoSchedule",
	}}
}

// platformFindings covers nodes that can never run runsc regardless of
// labels. The candidate set is the matching nodes when the selector
// matches anything, else the whole cluster (so a RuntimeClass-less Auto
// Mode cluster still gets the loud message).
func platformFindings(f *schema.ClusterFacts) []schema.PreflightFinding {
	n, p := f.Nodes, f.Platform
	candidates := n.MatchingSelector
	autoCount, brCount := p.EKSAutoModeMatchingNodes, p.BottlerocketMatchingNodes
	scope := fmt.Sprintf("node(s) matching RuntimeClass %q", f.RuntimeClass.Name)
	if candidates == 0 {
		candidates = n.Total
		autoCount, brCount = p.EKSAutoModeNodes, p.BottlerocketNodes
		scope = "node(s) in the cluster"
	}
	if candidates == 0 {
		return nil
	}

	var out []schema.PreflightFinding
	if autoCount > 0 {
		out = append(out, schema.PreflightFinding{
			ID:       FindingEKSAutoModeNodes,
			Severity: allOrSome(autoCount, candidates),
			Message: fmt.Sprintf("%d of %d %s are EKS Auto Mode managed instances (%s=%s); AWS owns their Bottlerocket image and container runtime, "+
				"runsc cannot be installed, so pods requesting RuntimeClass %q can never start there",
				autoCount, candidates, scope, schema.EKSAutoModeLabel, schema.EKSAutoModeLabelValue, f.RuntimeClass.Name),
			Remediation: "add a self-managed or Karpenter node group built from the agentmoat AL2023 AMI (packer/), label it to match the RuntimeClass nodeSelector, " +
				"and keep Auto Mode for runc workloads; see docs/eks-deployment.md",
		})
	}
	if brCount > 0 {
		out = append(out, schema.PreflightFinding{
			ID:       FindingBottlerocketNodes,
			Severity: allOrSome(brCount, candidates),
			Message: fmt.Sprintf("%d of %d %s run Bottlerocket, which ships no runsc and has an immutable root filesystem; "+
				"pods requesting RuntimeClass %q cannot start there",
				brCount, candidates, scope, f.RuntimeClass.Name),
			Remediation: "use the agentmoat AL2023 AMI (packer/) for the gVisor node group and label those nodes to match the RuntimeClass nodeSelector; see docs/eks-deployment.md",
		})
	}
	return out
}

// allOrSome is error when every candidate is affected (nothing can
// schedule) and warn otherwise (some nodes still can).
func allOrSome(affected, candidates int) schema.Severity {
	if affected >= candidates {
		return schema.SeverityError
	}
	return schema.SeverityWarn
}

// formatSelector renders a nodeSelector as k=v pairs in key order so
// messages are deterministic.
func formatSelector(sel map[string]string) string {
	keys := make([]string, 0, len(sel))
	for k := range sel {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+sel[k])
	}
	return strings.Join(parts, ",")
}

// severityRank orders severities for sorting: error first.
func severityRank(s schema.Severity) int {
	switch s {
	case schema.SeverityError:
		return 0
	case schema.SeverityWarn:
		return 1
	default:
		return 2
	}
}

// sortFindings sorts by severity rank, then ID, in place.
func sortFindings(findings []schema.PreflightFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri, rj := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return findings[i].ID < findings[j].ID
	})
}
