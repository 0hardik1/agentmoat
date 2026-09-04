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

	// GPU findings (gpu.go). None of them blocks: the RuntimeClass can
	// still host CPU workloads. They feed the classifier's gpu-passthrough
	// refinement and the plan warnings.

	// FindingGPUProductUnsupported (warn): GPU nodes carry a card nvproxy
	// does not support.
	FindingGPUProductUnsupported = "gpu-product-unsupported"

	// FindingGPUProductUnknown (info): GPU nodes carry no GFD product
	// label, so the card cannot be checked.
	FindingGPUProductUnknown = "gpu-product-unknown"

	// FindingGPUMIGEnabled (warn): GPU nodes slice their GPUs with MIG,
	// which nvproxy does not support.
	FindingGPUMIGEnabled = "gpu-mig-enabled"

	// FindingGPUDriverUnsupported (warn): the probed runsc does not list
	// the host driver on nodes whose card is otherwise supported.
	FindingGPUDriverUnsupported = "gpu-driver-unsupported"

	// FindingGPUDriverUnconfirmed (info): the card is supported but the
	// driver list has not been probed yet.
	FindingGPUDriverUnconfirmed = "gpu-driver-unconfirmed"

	// FindingGPUNvproxyReady (info): card and driver both supported by the
	// probed runsc.
	FindingGPUNvproxyReady = "gpu-nvproxy-ready"

	// Probe findings, produced by pkg/probe around the Evaluate output.

	// FindingNvproxyProbeDryRun (info): the probe pod was described, not
	// created.
	FindingNvproxyProbeDryRun = "nvproxy-probe-dry-run"

	// FindingNvproxyProbeSkipped (warn): the preflight has an error
	// finding, so there is no node to run the probe pod on.
	FindingNvproxyProbeSkipped = "nvproxy-probe-skipped"

	// FindingNvproxyProbeFailed (warn): the pod did not complete or its
	// output did not parse; the message carries the reason.
	FindingNvproxyProbeFailed = "nvproxy-probe-failed"
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
	out = append(out, gpuFindings(facts)...)
	SortFindings(out)
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

// gpuBuckets sorts GPU node groups by what stands between them and a
// working nvproxy. Each bucket becomes at most one finding, listing every
// group in it, so a cluster with three T4 driver versions yields one
// gpu-driver-unsupported finding, not three.
type gpuBuckets struct {
	mig, productBad, productUnknown, driverBad, driverUnknown, ready []string
}

func bucketGPUGroups(gf *schema.GPUFacts) gpuBuckets {
	var b gpuBuckets
	for _, g := range gf.Groups {
		desc := DescribeGPUGroup(g)
		switch {
		case g.MIG:
			b.mig = append(b.mig, desc)
		case g.ProductSupport == schema.SupportUnknown:
			b.productUnknown = append(b.productUnknown, desc)
		case g.ProductSupport == schema.SupportUnsupported:
			b.productBad = append(b.productBad, desc)
		case g.DriverSupport == schema.SupportSupported:
			b.ready = append(b.ready, desc)
		case g.DriverSupport == schema.SupportUnsupported:
			b.driverBad = append(b.driverBad, desc)
		default:
			b.driverUnknown = append(b.driverUnknown, desc)
		}
	}
	return b
}

// gpuFindings covers the GPU nodes. Nothing to say on a CPU-only cluster.
// Severities stop at warn: a GPU problem never blocks the RuntimeClass for
// everything else; it changes the gpu-passthrough verdict instead.
func gpuFindings(f *schema.ClusterFacts) []schema.PreflightFinding {
	if f.GPU == nil || f.GPU.Nodes == 0 {
		return nil
	}
	b := bucketGPUGroups(f.GPU)
	runsc := "runsc"
	if f.GPU.Nvproxy != nil && f.GPU.Nvproxy.RunscVersion != "" {
		runsc = "runsc " + f.GPU.Nvproxy.RunscVersion
	}
	var out []schema.PreflightFinding
	if len(b.mig) > 0 {
		out = append(out, schema.PreflightFinding{
			ID: FindingGPUMIGEnabled, Severity: schema.SeverityWarn,
			Message: "GPU nodes slice their GPUs with MIG (" + schema.GFDMIGStrategyLabel + " is not \"none\"); gVisor nvproxy does not support MIG: " +
				strings.Join(b.mig, "; "),
			Remediation: "keep MIG workloads on runc, or give the gVisor node group whole GPUs (mig.strategy=none)",
		})
	}
	if len(b.productBad) > 0 {
		out = append(out, schema.PreflightFinding{
			ID: FindingGPUProductUnsupported, Severity: schema.SeverityWarn,
			Message: "gVisor nvproxy does not support these GPU cards (supported: " + strings.Join(NvproxySupportedProducts, ", ") + "): " +
				strings.Join(b.productBad, "; "),
			Remediation: "keep workloads that need these cards on runc, or add gVisor GPU nodes with a supported card; see docs/gpu-nvproxy.md",
		})
	}
	if len(b.driverBad) > 0 {
		out = append(out, schema.PreflightFinding{
			ID: FindingGPUDriverUnsupported, Severity: schema.SeverityWarn,
			Message: runsc + " nvproxy does not list the host driver on: " + strings.Join(b.driverBad, "; ") +
				"; supported drivers: " + strings.Join(f.GPU.Nvproxy.SupportedDrivers, ", "),
			Remediation: "install one of the listed driver versions on the GPU nodes, or move to a runsc release that lists yours (docs/gvisor-version.md)",
		})
	}
	if len(b.productUnknown) > 0 {
		out = append(out, schema.PreflightFinding{
			ID: FindingGPUProductUnknown, Severity: schema.SeverityInfo,
			Message: "GPU nodes carry no GPU Feature Discovery labels (" + schema.GFDProductLabel + "), so the card and driver cannot be checked: " +
				strings.Join(b.productUnknown, "; "),
			Remediation: "install GPU Feature Discovery (part of the NVIDIA GPU Operator, or gfd.enabled=true in the device plugin chart)",
		})
	}
	if len(b.driverUnknown) > 0 {
		out = append(out, schema.PreflightFinding{
			ID: FindingGPUDriverUnconfirmed, Severity: schema.SeverityInfo,
			Message: "nvproxy supports the card, but the host driver has not been checked against the installed runsc: " +
				strings.Join(b.driverUnknown, "; "),
			Remediation: "run 'agentmoat probe nvproxy --dry-run=false' to read the supported driver list from runsc on a gVisor node",
		})
	}
	if len(b.ready) > 0 {
		out = append(out, schema.PreflightFinding{
			ID: FindingGPUNvproxyReady, Severity: schema.SeverityInfo,
			Message: runsc + " nvproxy supports the card and the host driver on: " + strings.Join(b.ready, "; "),
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

// SortFindings sorts by severity rank, then ID, in place. Exported so
// pkg/probe can append its own findings and keep the report order.
func SortFindings(findings []schema.PreflightFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		ri, rj := severityRank(findings[i].Severity), severityRank(findings[j].Severity)
		if ri != rj {
			return ri < rj
		}
		return findings[i].ID < findings[j].ID
	})
}
