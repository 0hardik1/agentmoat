// Package agentmoat: orchestration entry points.
//
// Scan() is the single function that wires scanner -> classifier ->
// schema.ScanReport. The CLI and the MCP server (cmd/agentmoat-mcp) both
// call this so the two surfaces cannot drift.
//
// The package's other entry points (Plan, Apply, Rollback, Verify,
// Explain, AssessWorkload) live in their own files and follow the same
// pattern: a public function that returns a versioned schema type plus an
// error.
package agentmoat

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// Version is the agentmoat version that every report carries in
// Metadata.AgentmoatVersion for provenance. Both binaries set it at start-up
// through ResolveBuildInfo (version.go): -ldflags first, then the Go build
// record. "dev" is what a program sees when it imports this package and
// never calls ResolveBuildInfo.
var Version = "dev"

// Scan performs a full read-only scan and classification of the target
// Kubernetes cluster. It does not mutate any cluster state.
//
// Pipeline:
//  1. Build a Kubernetes client from opts (kubeconfig / in-cluster).
//  2. Build the classifier registry (built-in rules + optional YAML overrides).
//  3. Enumerate workloads via pkg/scanner.
//  4. Collect cluster facts (RuntimeClass + nodes) via pkg/preflight so
//     the report records whether any node can host the plan. Optional:
//     skipped on request, degraded to nil when RBAC denies node reads.
//  5. Classify each workload via pkg/classifier and assemble a versioned
//     schema.ScanReport (buildReport).
//
// Steps 1, 3, and 4 are the "source": where the workloads and the facts
// come from. Steps 2 and 5 do not know or care. That split is deliberate:
// a second source feeds the same buildReport and gets the same verdicts.
//
// The second source is manifest files. When opts.ManifestPaths is set, Scan
// reads those and nothing else (scan_manifests.go): it builds no Kubernetes
// client, reads no kubeconfig, and makes no network call. The check comes
// first in this function so that no line of the cluster path can run.
//
// The caller is responsible for rendering the report (CLI uses pkg/output).
func Scan(ctx context.Context, opts ScanOptions) (*schema.ScanReport, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	if len(opts.ManifestPaths) > 0 {
		return scanManifests(opts, stderr)
	}

	// 1. Build the Kubernetes client. We pass --kubeconfig / --context
	// through to the loader; everything else falls back to env / defaults.
	// Tests can preempt this step by setting opts.KubeClient to a fake;
	// production callers leave it nil so the standard loader runs.
	client, err := newKubeClient(opts.KubeClient, opts.KubeconfigPath, opts.Context)
	if err != nil {
		return nil, err
	}

	// 2. Build the rule registry: the built-ins, then the YAML overrides.
	registry, err := buildRegistry(opts.RulesYAMLPath, stderr)
	if err != nil {
		return nil, err
	}

	// 3. Enumerate workloads. Translate ScanOptions -> EnumerateOptions.
	// If AllNamespaces is true, ignore the Namespaces filter.
	enumOpts := scanner.EnumerateOptions{
		LabelSelector: opts.LabelSelector,
		IncludeSystem: opts.IncludeSystem,
	}
	if !opts.AllNamespaces {
		enumOpts.Namespaces = opts.Namespaces
	}

	_, _ = fmt.Fprintln(stderr, "scanning cluster...")
	workloads, err := scanner.Enumerate(ctx, client, enumOpts)
	if err != nil {
		return nil, fmt.Errorf("enumerating workloads: %w", err)
	}

	// 4. Cluster facts. Two cheap cluster-scoped reads (one RuntimeClass,
	// the node list), or a saved report when --facts was given. They ride
	// along in the report so `plan --scan` can warn about a cluster with
	// no gVisor node without touching the API itself, and they feed the
	// classifier (gpu-passthrough refines against the GPU nodes and the
	// probed nvproxy driver list). Cluster read failures never fail the
	// scan: see facts.go.
	facts, err := resolveClusterFacts(ctx, client, factsSource{
		RuntimeClassName: opts.RuntimeClassName, SkipClusterFacts: opts.SkipClusterFacts, FactsPath: opts.FactsPath,
	}, stderr)
	if err != nil {
		return nil, err
	}

	// 5. Classify and assemble.
	report := buildReport(scanInput{
		Workloads:        workloads,
		Facts:            facts,
		Cluster:          kube.CurrentContext(opts.KubeconfigPath, opts.Context),
		Namespaces:       enumOpts.Namespaces, // nil when the scan was cluster-wide
		RuntimeClassName: opts.RuntimeClassName,
	}, registry)

	_, _ = fmt.Fprintf(stderr, "scanned %d workloads (compatible: %d, review: %d, incompatible: %d)\n",
		report.Spec.Summary.Total,
		report.Spec.Summary.Compatible,
		report.Spec.Summary.NeedsReview,
		report.Spec.Summary.Incompatible,
	)

	return report, nil
}

// scanInput is what a workload source hands to buildReport: the workloads,
// the facts to classify them against, and the provenance that goes into the
// report metadata. It carries no client and no file handle, so buildReport
// is a pure function of its arguments (apart from the timestamp).
type scanInput struct {
	// Workloads is the canonical workload list from pkg/scanner.
	Workloads []scanner.Workload

	// Facts is the cluster inventory the classifier refines against. Nil
	// means "no facts": the two rules with a Refine hook keep their base
	// severity.
	Facts *schema.ClusterFacts

	// Cluster is the kubeconfig context name, for ReportMetadata.Cluster.
	// Empty when the workloads did not come from a cluster.
	Cluster string

	// Namespaces is the namespace filter that was applied, for
	// ReportMetadata.Namespaces. Nil means cluster-wide.
	Namespaces []string

	// RuntimeClassName is the RuntimeClass the scan treats as the sandbox.
	// Empty means schema.DefaultRuntimeClassName.
	RuntimeClassName string

	// Origins, when non-nil, is parallel to Workloads: Origins[i] is where
	// Workloads[i] was defined. Nil for a cluster scan.
	Origins []scanner.ManifestOrigin

	// Source goes to ReportMetadata.Source. Nil for a cluster scan.
	Source *schema.ReportSource
}

// buildReport classifies every workload in the input and assembles the
// ScanReport. It is the half of a scan that is the same for every workload
// source.
func buildReport(in scanInput, registry *classifier.Registry) *schema.ScanReport {
	runtimeClass := sandboxClassName(in.RuntimeClassName)

	report := schema.NewScanReport()
	report.Metadata = schema.ReportMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          in.Cluster,
		Namespaces:       in.Namespaces,
		AgentmoatVersion: Version,
		ClusterFacts:     in.Facts,
		RuntimeClassName: runtimeClass,
		Source:           in.Source,
	}

	results := make([]schema.WorkloadResult, 0, len(in.Workloads))
	for i, w := range in.Workloads {
		result := workloadResult(w, classifier.ClassifyWithFacts(w, registry, in.Facts))
		// Attach the origin here, by index, before the sort below moves
		// the results: Origins is parallel to Workloads, not to results.
		if in.Origins != nil {
			result.Origin = originOf(in.Origins[i])
		}
		results = append(results, result)
	}

	// Defensive: keep results sorted even if the source's order changes in
	// the future. Stable key: (Namespace, Kind, Name).
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Namespace != results[j].Namespace {
			return results[i].Namespace < results[j].Namespace
		}
		if results[i].Kind != results[j].Kind {
			return results[i].Kind < results[j].Kind
		}
		return results[i].Name < results[j].Name
	})

	report.Spec = schema.ReportSpec{
		Summary:   summarize(results, runtimeClass),
		Workloads: results,
	}
	return report
}

// sandboxClassName resolves the RuntimeClass a scan treats as the sandbox:
// the given name, or the default when it is empty. Everything that asks "is
// this workload sandboxed?" (the summary count here, the gate in gate.go)
// goes through this one function, so the two cannot disagree.
func sandboxClassName(name string) string {
	if name == "" {
		return schema.DefaultRuntimeClassName
	}
	return name
}

// workloadResult projects a scanner.Workload and its verdict onto the wire
// type. Scan and AssessWorkload both call it, so a single-workload verdict
// and the cluster-wide one for the same object have the same shape.
func workloadResult(w scanner.Workload, v classifier.Verdict) schema.WorkloadResult {
	result := schema.WorkloadResult{
		Kind:           w.Kind,
		Namespace:      w.Namespace,
		Name:           w.Name,
		Compatibility:  v.Compatibility,
		Reasons:        v.Reasons,
		Recommendation: v.Recommendation,
		Overhead:       v.Overhead,
	}
	// The runtime class the pod spec asks for today. No rule reads it (a
	// rule says whether a workload CAN run under gVisor, not whether it
	// does), but the report needs it to tell "already sandboxed" from "a
	// candidate".
	if rc := w.PodSpec.RuntimeClassName; rc != nil {
		result.RuntimeClassName = *rc
	}
	return result
}

// summarize counts each compatibility bucket, and the workloads that ask
// for runtimeClass (already resolved by sandboxClassName).
func summarize(results []schema.WorkloadResult, runtimeClass string) schema.Summary {
	s := schema.Summary{Total: len(results)}
	for _, r := range results {
		if r.RuntimeClassName == runtimeClass {
			s.Sandboxed++
		}
		switch r.Compatibility {
		case schema.CompatibilityCompatible:
			s.Compatible++
		case schema.CompatibilityReview:
			s.NeedsReview++
		case schema.CompatibilityIncompatible:
			s.Incompatible++
		}
	}
	return s
}
