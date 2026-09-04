// Package agentmoat: Explain orchestrator.
//
// Explain has two modes, both producing the same versioned
// ExplainDocument envelope so the renderer and the MCP server can switch
// on the populated fields without learning a new shape:
//
//   - Static-topic / list mode (offline): reads from the embedded
//     docs/*.md and fills Spec.Topic, Spec.Content, and Spec.Topics. No
//     Kubernetes client work; safe to invoke from anywhere.
//
//   - Deep mode (online, ctx-aware): runs the same scanner + classifier
//     pipeline that Scan() uses, then walks each (workload, verdict)
//     pair through pkg/explainer.ExplainWorkload to produce a
//     NamespaceExplanation. This is the path that powers
//     `agentmoat explain namespace <ns>` and
//     `agentmoat explain workload <ns>/<name>`.
//
// The two paths share the metadata envelope (GeneratedAt + agentmoat
// version) and the topic list, so a deep-mode document still carries the
// catalog of available static topics. JSON consumers see one envelope
// shape across both modes.
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
	"github.com/0hardik1/agentmoat/pkg/explainer"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// Explain returns an ExplainDocument describing either a static topic
// (offline mode) or a deeply-explained namespace / single workload
// (online mode). The active mode is selected by opts:
//
//   - opts.Namespace == "" -> static-topic / list mode (no cluster work).
//     ctx is unused but kept on the signature so deep mode and topic mode
//     share an interface.
//   - opts.Namespace != "" -> deep mode. ctx is honored on every K8s API
//     call. opts.Workload, when non-nil, narrows the output to one entry.
//
// Topic lookup in static mode is case-insensitive; an unknown topic
// returns an error whose message lists every valid topic so the operator
// can self-correct.
func Explain(ctx context.Context, opts ExplainOptions) (*schema.ExplainDocument, error) {
	if opts.Namespace == "" {
		// Static-topic / list mode. ctx is ignored: there is no cluster
		// work. Kept on the signature so the deep-mode path can share
		// it with no extra plumbing at the call sites.
		return explainTopic(opts)
	}
	return explainNamespace(ctx, opts)
}

// explainTopic is the original (offline) Explain body. Pulled out so the
// deep-mode branch is clearly separate and so the topic path stays a pure
// function over the embedded docs.
//
// Empty Topic -> list mode (Spec.Topics populated, Spec.Content empty).
// Non-empty Topic -> topic mode (Spec.Topic + Spec.Content + Spec.Topics).
func explainTopic(opts ExplainOptions) (*schema.ExplainDocument, error) {
	doc := newExplainDoc()
	doc.Spec.Topics = explainer.Topics()

	if opts.Topic == "" {
		// List mode: just the available topics.
		return doc, nil
	}

	content, err := explainer.Explain(opts.Topic)
	if err != nil {
		// Pass the explainer's error through unchanged: it already
		// names the valid topics for self-correction.
		return nil, fmt.Errorf("explain: %w", err)
	}
	doc.Spec.Topic = opts.Topic
	doc.Spec.Content = content
	return doc, nil
}

// explainNamespace is the deep path. It mirrors what Scan() does, then
// hands each (workload, verdict) to pkg/explainer.ExplainWorkload to
// build the structured evidence + prose. We deliberately re-build the
// pipeline inline here rather than calling Scan() because we need the
// raw scanner.Workload (which carries PodSpec) and Scan() only returns
// the lossy schema.WorkloadResult.
//
// When opts.Workload is set, we filter after enumerate so the operator
// sees the classifier verdict the same way they would for the whole
// namespace: the registry is unchanged, only the output is narrowed.
func explainNamespace(ctx context.Context, opts ExplainOptions) (*schema.ExplainDocument, error) {
	// Force the scan to the named namespace. opts.Scan.Namespaces /
	// AllNamespaces are deliberately overwritten: in this mode the
	// positional argument (or filter) is the source of truth.
	scanOpts := opts.Scan
	scanOpts.Namespaces = []string{opts.Namespace}
	scanOpts.AllNamespaces = false

	stderr := scanOpts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Build the kube client and the classifier registry the same way
	// Scan() does. Keeping these inline (rather than factoring a shared
	// helper) keeps the call graph easy to read; if a third command
	// needs the same plumbing we can extract then.
	client, _, err := kube.NewClient(scanOpts.KubeconfigPath, scanOpts.Context)
	if err != nil {
		return nil, fmt.Errorf("building kubernetes client: %w", err)
	}

	registry := classifier.NewRegistry()
	classifier.RegisterBuiltins(registry)
	if scanOpts.RulesYAMLPath != "" {
		data, err := os.ReadFile(scanOpts.RulesYAMLPath)
		if err != nil {
			return nil, fmt.Errorf("reading rules override %s: %w", scanOpts.RulesYAMLPath, err)
		}
		warns, err := registry.LoadYAML(data)
		if err != nil {
			return nil, fmt.Errorf("parsing rules override %s: %w", scanOpts.RulesYAMLPath, err)
		}
		for _, msg := range warns {
			_, _ = fmt.Fprintf(stderr, "warning: %s\n", msg)
		}
	}

	enumOpts := scanner.EnumerateOptions{
		Namespaces:    []string{opts.Namespace},
		LabelSelector: scanOpts.LabelSelector,
		IncludeSystem: scanOpts.IncludeSystem,
	}

	_, _ = fmt.Fprintf(stderr, "scanning namespace %q...\n", opts.Namespace)
	workloads, err := scanner.Enumerate(ctx, client, enumOpts)
	if err != nil {
		return nil, fmt.Errorf("enumerating workloads: %w", err)
	}

	// Apply the optional single-workload filter. Done after classify so
	// the summary counts below could in principle reflect the whole
	// namespace; but operator expectation for `explain workload` is "I
	// want a focused view of this one thing", so we filter the source
	// list first and the summary then reports "1 workload". That keeps
	// the renderer simple: every chart/badge it draws is exactly what
	// the JSON view also reports.
	if opts.Workload != nil {
		filtered, err := filterWorkloads(workloads, opts.Namespace, opts.Workload)
		if err != nil {
			return nil, err
		}
		workloads = filtered
	}

	// Same cluster facts as scan, so the deep explanation of a GPU
	// workload carries the same refined verdict the scan table shows.
	facts, err := resolveClusterFacts(ctx, client, factsSource{
		RuntimeClassName: scanOpts.RuntimeClassName, SkipClusterFacts: scanOpts.SkipClusterFacts, FactsPath: scanOpts.FactsPath,
	}, stderr)
	if err != nil {
		return nil, err
	}

	// Classify and explain each surviving workload in scanner order.
	// scanner.Enumerate already sorts by (namespace, kind, name) so the
	// output is deterministic without a re-sort.
	rules := registry.Rules()
	explanations := make([]schema.WorkloadExplanation, 0, len(workloads))
	for _, w := range workloads {
		v := classifier.ClassifyWithFacts(w, registry, facts)
		expl, err := explainer.ExplainWorkload(w, v, rules)
		if err != nil {
			// ExplainWorkload returns nil for missing prose; an actual
			// error here means something structural broke (reserved for
			// a future revision). Surface it with workload identity so
			// the operator can pinpoint the problem.
			return nil, fmt.Errorf("explaining %s/%s/%s: %w", w.Kind, w.Namespace, w.Name, err)
		}
		explanations = append(explanations, expl)
	}

	// Defensive resort: scanner already sorts but we want to guarantee
	// stable output even if a future scanner change reorders within a
	// namespace.
	sort.SliceStable(explanations, func(i, j int) bool {
		if explanations[i].Kind != explanations[j].Kind {
			return explanations[i].Kind < explanations[j].Kind
		}
		return explanations[i].Name < explanations[j].Name
	})

	doc := newExplainDoc()
	doc.Spec.Topics = explainer.Topics()
	doc.Spec.Namespace = &schema.NamespaceExplanation{
		Name:      opts.Namespace,
		Summary:   summarizeExplanations(explanations),
		Workloads: explanations,
	}

	_, _ = fmt.Fprintf(stderr, "explained %d workloads (compatible: %d, review: %d, incompatible: %d)\n",
		doc.Spec.Namespace.Summary.Total,
		doc.Spec.Namespace.Summary.Compatible,
		doc.Spec.Namespace.Summary.NeedsReview,
		doc.Spec.Namespace.Summary.Incompatible,
	)

	return doc, nil
}

// filterWorkloads narrows a scanner result list to the entries matching
// the WorkloadFilter. Returns a descriptive error when no entry matches,
// listing the available names so the operator can correct a typo.
//
// Matching rules:
//   - Filter.Name is required and matches Workload.Name exactly.
//   - Filter.Kind, if non-empty, must equal Workload.Kind exactly.
//     Kind values come from the scanner ("Deployment", "StatefulSet",
//     etc.); a wrong-case filter ("deployment") will NOT match. The CLI
//     side is responsible for normalizing input if we ever want case
//     insensitivity.
func filterWorkloads(workloads []scanner.Workload, ns string, filter *WorkloadFilter) ([]scanner.Workload, error) {
	out := make([]scanner.Workload, 0, 1)
	for _, w := range workloads {
		if w.Name != filter.Name {
			continue
		}
		if filter.Kind != "" && w.Kind != filter.Kind {
			continue
		}
		out = append(out, w)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf(
			"no workload found in namespace %q matching kind=%q name=%q",
			ns, filter.Kind, filter.Name,
		)
	}
	return out, nil
}

// summarizeExplanations builds a Summary the renderer can chart. We
// reuse schema.Summary (the same shape that scan uses) so the
// NamespaceExplanation header chart can lean on the existing bar
// renderer with no extra adapter.
func summarizeExplanations(explanations []schema.WorkloadExplanation) schema.Summary {
	s := schema.Summary{Total: len(explanations)}
	for _, e := range explanations {
		switch e.Compatibility {
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

// newExplainDoc returns an envelope with Metadata pre-filled. Used by
// both modes so the GeneratedAt + Version fields are identical for any
// output format.
func newExplainDoc() *schema.ExplainDocument {
	doc := schema.NewExplainDocument()
	doc.Metadata = schema.ExplainMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		AgentmoatVersion: Version,
	}
	return doc
}
