// Package agentmoat: the manifest source for Scan.
//
// A cluster scan answers "what is running, and can it move?". A manifest
// scan answers the same question for workloads that are still files: the
// YAML in a repository, or the output of `helm template` or `kustomize
// build`. It exists for the pipeline. A pull request has no cluster to
// scan, and by the time a change is in one, the review that could have
// stopped it is over.
//
// The function here is the "source" half of Scan for that case: it gets the
// workloads from pkg/scanner (ReadManifestPaths, FromManifests) and hands
// them to the same buildReport as the cluster path, so a pod spec gets the
// same verdict whichever way it arrived.
//
// Invariant: nothing in this file builds a Kubernetes client, loads a
// kubeconfig, or opens a connection. Not even a fake client: the report of a
// file scan must not carry a cluster name it never talked to. A test keeps
// this honest with a kubeconfig that points at a dead server.
//
// What a manifest scan cannot know, and says so:
//
//   - What admission adds in the cluster (an injected sidecar).
//   - What an overlay changes. Each file is read as a complete object. A
//     directory with a kustomization file is bases plus patches, not the
//     objects that get deployed. Those inputs are listed in
//     ReportSource.Unrendered. The way to scan the real thing is
//     `kustomize build <dir> | agentmoat scan -f -`.
//   - Whether a field name is spelled right. This is not a schema validator.
package agentmoat

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// scanManifests is Scan for opts.ManifestPaths.
func scanManifests(opts ScanOptions, stderr io.Writer) (*schema.ScanReport, error) {
	set, err := readManifestSet(opts, stderr)
	if err != nil {
		return nil, err
	}

	report := buildReport(scanInput{
		Workloads:        set.found.Workloads,
		Origins:          set.found.Origins,
		Facts:            set.facts,
		RuntimeClassName: opts.RuntimeClassName,
		Source:           set.source,
		// Cluster and Namespaces stay empty: no cluster was read, and the
		// one namespace value is a default, not a filter.
	}, set.registry)

	describeManifestScan(stderr, set.files, set.found, report)
	return report, nil
}

// manifestSet is manifest input that was read and decoded, with what a
// verdict needs. It is the first half of a file scan. `scan -f` and `patch`
// share it, so the two read the same paths in the same way: the same files
// in a directory, the same documents, the same default namespace, and the
// same errors for input that cannot be read.
type manifestSet struct {
	files    *scanner.ManifestFiles
	found    *scanner.ManifestResult
	registry *classifier.Registry
	facts    *schema.ClusterFacts // nil with no --facts
	source   *schema.ReportSource
}

// readManifestSet reads opts.ManifestPaths. Of the options it uses the
// paths, standard input, the one namespace, the label selector, the rules
// file, and the facts file.
func readManifestSet(opts ScanOptions, stderr io.Writer) (*manifestSet, error) {
	// In this mode a namespace is a default, not a filter (see
	// ScanOptions.ManifestPaths), and there is only one default.
	if len(opts.Namespaces) > 1 {
		return nil, fmt.Errorf("manifest input takes one namespace (the default for objects that name none), not %d: %s",
			len(opts.Namespaces), strings.Join(opts.Namespaces, ", "))
	}
	defaultNamespace := ""
	if len(opts.Namespaces) == 1 {
		defaultNamespace = opts.Namespaces[0]
	}

	registry, err := buildRegistry(opts.RulesYAMLPath, stderr)
	if err != nil {
		return nil, err
	}

	files, err := scanner.ReadManifestPaths(opts.ManifestPaths, opts.Stdin)
	if err != nil {
		return nil, fmt.Errorf("reading manifests: %w", err)
	}
	if len(files.Inputs) == 0 {
		return nil, noManifestsError(opts.ManifestPaths, files)
	}

	found, err := scanner.FromManifests(files.Inputs, scanner.ManifestOptions{
		DefaultNamespace: defaultNamespace,
		LabelSelector:    opts.LabelSelector,
	})
	if err != nil {
		// A *scanner.ManifestError lists every unreadable document with
		// its file and line. It reads well as it is; a prefix would only
		// push the first line to the right.
		return nil, err
	}
	if found.Objects == 0 {
		return nil, fmt.Errorf("no Kubernetes objects in %s: the input has no document with content",
			describePaths(opts.ManifestPaths))
	}

	// Facts come from a saved report or not at all. There is no cluster to
	// ask, and that is the point of this mode.
	var facts *schema.ClusterFacts
	if opts.FactsPath != "" {
		facts, err = loadClusterFacts(opts.FactsPath)
		if err != nil {
			return nil, fmt.Errorf("loading --facts: %w", err)
		}
		_, _ = fmt.Fprintf(stderr, "cluster facts: loaded from %s (%s)\n", opts.FactsPath, describeFacts(facts))
	}

	source := &schema.ReportSource{
		Type:       schema.SourceManifests,
		Inputs:     len(files.Inputs),
		Skipped:    found.Skipped,
		Duplicates: found.Duplicates,
		Unrendered: unrenderedSigns(files, found),
	}
	if len(source.Skipped) == 0 {
		source.Skipped = nil // keep the key out of the report when empty
	}
	return &manifestSet{files: files, found: found, registry: registry, facts: facts, source: source}, nil
}

// noManifestsError explains a scan that found no manifest text at all.
func noManifestsError(paths []string, files *scanner.ManifestFiles) error {
	where := describePaths(paths)
	switch {
	case len(files.Kustomizations) > 0:
		return fmt.Errorf("no manifests in %s: only a kustomization file is there; "+
			"scan what it builds: kustomize build <dir> | agentmoat scan -f -", where)
	case files.Ignored > 0:
		return fmt.Errorf("no manifests in %s: %d YAML or JSON file(s) are there, but none has an apiVersion", where, files.Ignored)
	default:
		return fmt.Errorf("no manifests in %s: a directory is searched for *.yaml, *.yml, and *.json files", where)
	}
}

// describePaths renders the manifest paths for an error message. "-" alone
// reads like a typing error, so it is spelled out.
func describePaths(paths []string) string {
	shown := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == scanner.StdinPath {
			p = "standard input"
		}
		shown = append(shown, p)
	}
	return strings.Join(shown, ", ")
}

// unrenderedSigns lists what tells us the input is not the rendered form of
// the objects: kustomization files (found by name on disk, or by kind in the
// text) and partial objects. See schema.ReportSource.Unrendered.
func unrenderedSigns(files *scanner.ManifestFiles, found *scanner.ManifestResult) []string {
	kustomizations := map[string]struct{}{}
	for _, name := range files.Kustomizations {
		kustomizations[name] = struct{}{}
	}
	for _, name := range found.Kustomizations {
		kustomizations[name] = struct{}{}
	}
	names := make([]string, 0, len(kustomizations))
	for name := range kustomizations {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]string, 0, len(names)+len(found.Partial))
	for _, name := range names {
		out = append(out, "kustomization file: "+name)
	}
	for _, p := range found.Partial {
		line := fmt.Sprintf("partial object: %s %s at %s:%d has no containers",
			p.Kind, displayName(p.Namespace, p.Name), p.Origin.File, p.Origin.Line)
		if p.RuntimeClassName != "" {
			line += fmt.Sprintf(" and sets runtimeClassName %q", p.RuntimeClassName)
		}
		out = append(out, line)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// displayName renders a namespace/name pair for a message, with "-" for an
// empty part.
func displayName(namespace, name string) string {
	if namespace == "" {
		namespace = "-"
	}
	if name == "" {
		name = "-"
	}
	return namespace + "/" + name
}

// originOf projects the scanner's origin onto the wire type.
//
// For standard input there is no file a reader could open. When the document
// came from `helm template` it carries a `# Source:` comment that names the
// template, and that path IS a file in the repository, so it is used. The
// line is then left out: it counts in the rendered stream, and a CI
// annotation on that line of the template would point at the wrong text.
func originOf(o scanner.ManifestOrigin) *schema.Origin {
	if o.File == scanner.StdinName && o.TemplateSource != "" {
		return &schema.Origin{File: o.TemplateSource}
	}
	return &schema.Origin{File: o.File, Line: o.Line}
}

// describeManifestScan writes the progress lines for a manifest scan. They
// say what was read and, with more care, what was not classified: a quiet
// "0 incompatible" from a scan that looked at nothing is the failure this
// mode must not have.
func describeManifestScan(stderr io.Writer, files *scanner.ManifestFiles, found *scanner.ManifestResult, report *schema.ScanReport) {
	sum := report.Spec.Summary
	_, _ = fmt.Fprintf(stderr, "scanned %d workloads from %d manifest input(s) (compatible: %d, review: %d, incompatible: %d)\n",
		sum.Total, len(files.Inputs), sum.Compatible, sum.NeedsReview, sum.Incompatible)

	// The objects that were read but are not workloads (found.Skipped) are
	// not listed here: they are in the report, and every renderer shows
	// them. The lines below are what the report does not carry.
	if files.Ignored > 0 {
		_, _ = fmt.Fprintf(stderr, "not read: %d file(s) with no apiVersion\n", files.Ignored)
	}
	if found.OwnedDropped > 0 {
		_, _ = fmt.Fprintf(stderr, "left out: %d workload(s) whose controller is also in the input\n", found.OwnedDropped)
	}
	if found.Filtered > 0 {
		_, _ = fmt.Fprintf(stderr, "left out: %d workload(s) that do not match the label selector\n", found.Filtered)
	}
	if found.NoNamespace > 0 {
		_, _ = fmt.Fprintf(stderr, "note: %d workload(s) name no namespace; pass one with -n to fill it in\n", found.NoNamespace)
	}
	for _, d := range found.Duplicates {
		_, _ = fmt.Fprintf(stderr, "warning: %s\n", d)
	}
	if signs := report.Metadata.Source.Unrendered; len(signs) > 0 {
		// One line here. The list is in the report (source.unrendered),
		// and the table prints it; twice on one screen is noise.
		_, _ = fmt.Fprintf(stderr, "warning: this input is not rendered (%d sign(s): kustomization files or partial objects), "+
			"so the verdicts describe the files and not what gets deployed; "+
			"scan the rendered objects: kustomize build <dir> | agentmoat scan -f -\n", len(signs))
	}
	if sum.Total == 0 {
		_, _ = fmt.Fprintln(stderr, "note: the input has no workloads (no object with a pod template)")
	}
}

// formatCounts renders a count map as "2 ConfigMap, 1 Service", sorted by
// key so the line is the same on every run.
func formatCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
	}
	return strings.Join(parts, ", ")
}
