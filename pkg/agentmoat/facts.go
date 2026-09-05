// Package agentmoat: cluster facts for the scan.
//
// Scan attaches schema.ClusterFacts to ReportMetadata so a plan built from
// the stored report can warn "no node can host this" without a cluster
// round-trip. The collection is best-effort by design: scan is promised to
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

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
)

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
