// Package agentmoat: cluster facts for the scan, assess, and explain.
//
// Scan attaches schema.ClusterFacts to ReportMetadata so a plan built from
// the stored report can warn "no node can host this" without a cluster
// round-trip, and hands the same facts to the classifier, which uses the
// GPU inventory to refine gpu-passthrough. Facts come from the cluster
// (best-effort) or, with --facts, from a saved PreflightReport or
// ScanReport, which is how the nvproxy probe's driver list reaches a scan
// without the scan creating anything. The collection is best-effort by design: scan is promised to
// work from a read-only kubeconfig whose RBAC may well stop at workloads
// and namespaces. Denied node reads therefore degrade to "no facts" plus a
// stderr warning instead of failing the scan. The strict path is
// `agentmoat preflight`, which does fail on RBAC errors because reading
// the nodes is its whole job.
package agentmoat

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
)

// factsSource is the subset of ScanOptions / AssessWorkloadOptions that
// decides where the facts come from, so both orchestrators share one
// resolver.
type factsSource struct {
	RuntimeClassName string
	SkipClusterFacts bool
	FactsPath        string
}

// resolveClusterFacts returns the ClusterFacts the classifier and the
// report should use: from FactsPath when set (an unreadable file is an
// error), nil when SkipClusterFacts, else best-effort from the cluster.
func resolveClusterFacts(ctx context.Context, client kubernetes.Interface, src factsSource, stderr io.Writer) (*schema.ClusterFacts, error) {
	if src.FactsPath != "" {
		facts, err := loadClusterFacts(src.FactsPath)
		if err != nil {
			return nil, fmt.Errorf("loading --facts: %w", err)
		}
		_, _ = fmt.Fprintf(stderr, "cluster facts: loaded from %s (%s)\n", src.FactsPath, describeFacts(facts))
		return facts, nil
	}
	if src.SkipClusterFacts {
		return nil, nil
	}
	return collectClusterFacts(ctx, client, src.RuntimeClassName, stderr), nil
}

// loadClusterFacts reads ClusterFacts from a saved report. Two kinds are
// accepted, because both carry the same block: a PreflightReport (from
// `preflight` or `probe nvproxy`) at spec.facts, and a ScanReport at
// metadata.clusterFacts.
func loadClusterFacts(path string) (*schema.ClusterFacts, error) {
	// #nosec G304 -- path is an operator-supplied flag naming a report
	// this binary wrote earlier; reading it is the feature.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	switch envelope.Kind {
	case schema.KindPreflightReport:
		var r schema.PreflightReport
		if err := yaml.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		facts := r.Spec.Facts
		return &facts, nil
	case schema.KindScanReport:
		var r schema.ScanReport
		if err := yaml.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if r.Metadata.ClusterFacts == nil {
			return nil, fmt.Errorf("%s has no metadata.clusterFacts (scanned with --no-cluster-facts or without node RBAC)", path)
		}
		return r.Metadata.ClusterFacts, nil
	default:
		return nil, fmt.Errorf("file %s has kind %q, want %q or %q", path, envelope.Kind, schema.KindPreflightReport, schema.KindScanReport)
	}
}

// describeFacts is the one-line stderr summary of loaded facts.
func describeFacts(f *schema.ClusterFacts) string {
	s := fmt.Sprintf("RuntimeClass %q, %d node(s)", f.RuntimeClass.Name, f.Nodes.Total)
	if f.GPU != nil {
		s += fmt.Sprintf(", %d GPU node(s)", f.GPU.Nodes)
		if f.GPU.Nvproxy != nil {
			s += fmt.Sprintf(", nvproxy probed on runsc %s", f.GPU.Nvproxy.RunscVersion)
		} else {
			s += ", nvproxy not probed"
		}
	}
	return s
}

// collectClusterFacts wraps preflight.Collect with the degrade-on-error
// policy described above. Returns nil (never a partial value) on any
// error so consumers have one nil-check, not a set of zero-value traps.
func collectClusterFacts(ctx context.Context, client kubernetes.Interface, runtimeClassName string, stderr io.Writer) *schema.ClusterFacts {
	facts, err := preflight.Collect(ctx, client, runtimeClassName)
	if err == nil {
		return facts
	}
	if apierrors.IsForbidden(err) {
		_, _ = fmt.Fprintf(stderr, "warning: cluster facts skipped: %v "+
			"(grant get/list on nodes and node.k8s.io/runtimeclasses; see deploy/clusterrole-readonly.yaml)\n", err)
		return nil
	}
	_, _ = fmt.Fprintf(stderr, "warning: cluster facts skipped: %v\n", err)
	return nil
}
