// Package agentmoat: orchestration entry points.
//
// Scan() is the single function that wires scanner -> classifier ->
// schema.ScanReport. The CLI and the future MCP server both call this so
// the two surfaces cannot drift.
//
// Phase 1 ships only Scan. Subsequent phases will add Plan, Apply, Verify,
// and Rollback to this package, each following the same pattern: a public
// function that returns a versioned schema type plus an error.
package agentmoat

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/0hardik1/agentmoat/internal/kube"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// Version is set at build time via -ldflags. Defaults to "dev" for local
// builds. Reported in ScanReport.Metadata.AgentmoatVersion for provenance.
var Version = "dev"

// Scan performs a full read-only scan and classification of the target
// Kubernetes cluster. It does not mutate any cluster state.
//
// Pipeline:
//  1. Build a Kubernetes client from opts (kubeconfig / in-cluster).
//  2. Build the classifier registry (built-in rules + optional YAML overrides).
//  3. Enumerate workloads via pkg/scanner.
//  4. Classify each workload via pkg/classifier.
//  5. Assemble a versioned schema.ScanReport and return it.
//
// The caller is responsible for rendering the report (CLI uses pkg/output).
func Scan(ctx context.Context, opts ScanOptions) (*schema.ScanReport, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// 1. Build the Kubernetes client. We pass --kubeconfig / --context
	// through to the loader; everything else falls back to env / defaults.
	// Tests (currently only the MCP server's unit tests) can preempt this
	// step by setting opts.KubeClient to a fake; production callers leave
	// it nil so the standard three-tier loader runs.
	client := opts.KubeClient
	if client == nil {
		cs, _, err := kube.NewClient(opts.KubeconfigPath, opts.Context)
		if err != nil {
			return nil, fmt.Errorf("building kubernetes client: %w", err)
		}
		client = cs
	}

	// 2. Build the rule registry. Always register the built-ins, then
	// apply YAML overrides on top if provided.
	registry := classifier.NewRegistry()
	classifier.RegisterBuiltins(registry)
	if opts.RulesYAMLPath != "" {
		data, err := os.ReadFile(opts.RulesYAMLPath)
		if err != nil {
			return nil, fmt.Errorf("reading rules override %s: %w", opts.RulesYAMLPath, err)
		}
		warns, err := registry.LoadYAML(data)
		if err != nil {
			return nil, fmt.Errorf("parsing rules override %s: %w", opts.RulesYAMLPath, err)
		}
		// Surface override warnings (unknown IDs) on stderr but do not fail
		// the run; the operator may be intentionally future-proofing.
		for _, w := range warns {
			_, _ = fmt.Fprintf(stderr, "warning: %s\n", w)
		}
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

	// 4. Classify and collect results. Sorting is already done by the
	// scanner, so iterating in order produces a deterministic report.
	report := schema.NewScanReport()
	report.Metadata = schema.ReportMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          kube.CurrentContext(opts.KubeconfigPath),
		Namespaces:       opts.Namespaces,
		AgentmoatVersion: Version,
	}

	results := make([]schema.WorkloadResult, 0, len(workloads))
	for _, w := range workloads {
		v := classifier.Classify(w, registry)
		results = append(results, schema.WorkloadResult{
			Kind:           w.Kind,
			Namespace:      w.Namespace,
			Name:           w.Name,
			Compatibility:  v.Compatibility,
			Reasons:        v.Reasons,
			Recommendation: v.Recommendation,
			Overhead:       v.Overhead,
		})
	}

	// Defensive: keep results sorted even if scanner.Enumerate's order
	// changes in the future. Stable key: (Namespace, Kind, Name).
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
		Summary:   summarize(results),
		Workloads: results,
	}

	_, _ = fmt.Fprintf(stderr, "scanned %d workloads (compatible: %d, review: %d, incompatible: %d)\n",
		report.Spec.Summary.Total,
		report.Spec.Summary.Compatible,
		report.Spec.Summary.NeedsReview,
		report.Spec.Summary.Incompatible,
	)

	return report, nil
}

// summarize counts each compatibility bucket. Kept as a small helper so
// the Scan() body stays readable.
func summarize(results []schema.WorkloadResult) schema.Summary {
	s := schema.Summary{Total: len(results)}
	for _, r := range results {
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

// _ keeps the io import live for callers that pass opts.Stderr.
var _ io.Writer = (io.Writer)(nil)
