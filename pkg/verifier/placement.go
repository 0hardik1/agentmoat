// Package verifier: node placement check.
//
// A pod can carry `runtimeClassName: gvisor` and still be running on a
// runc node. That happens when the RuntimeClass has no
// scheduling.nodeSelector (nothing steers the pod), or when the pod's own
// nodeSelector / affinity pinned it somewhere else before the RuntimeClass
// existed. The spec-level check passes in both cases; only the in-pod
// probe (opt-in, needs pods/exec) would notice. This file adds a cheaper,
// API-only signal: read the RuntimeClass nodeSelector, read the nodes the
// step's pods run on, and report every hosting node whose labels do not
// satisfy the selector. A mismatch demotes the step exactly like a failed
// probe does, since the verdict is the same: the workload is not on gVisor.
//
// The check is best-effort about permissions. Nodes and RuntimeClasses are
// cluster-scoped; an identity with the older, workload-only RBAC cannot
// read them. In that case NodePlacement.Checked stays false, Message says
// why, and the step keeps its spec-level status: verify must not start
// failing for people who never granted node reads.
package verifier

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// stepDeps bundles what verifyStep needs beyond the step itself, so the
// per-step function keeps a short signature as checks are added.
type stepDeps struct {
	client    kubernetes.Interface
	doProbe   bool
	exec      ExecRunner
	placement *placementChecker
	stderr    io.Writer
}

// placementChecker memoizes RuntimeClass and Node reads across steps. A
// plan with 40 Deployments on the same 3 nodes should cost 1 RuntimeClass
// GET and 3 Node GETs, not 43 and 120.
type placementChecker struct {
	client kubernetes.Interface
	rcs    map[string]rcLookup
	nodes  map[string]nodeLookup
}

// rcLookup is a cached RuntimeClass read. problem is non-empty when the
// object cannot be used for the check (missing, unreadable, no selector).
type rcLookup struct {
	selector map[string]string
	problem  string
}

// nodeLookup is a cached Node read.
type nodeLookup struct {
	labels  map[string]string
	problem string
}

func newPlacementChecker(client kubernetes.Interface) *placementChecker {
	return &placementChecker{
		client: client,
		rcs:    map[string]rcLookup{},
		nodes:  map[string]nodeLookup{},
	}
}

// check evaluates the nodes hosting `pods` against the nodeSelector of
// the named RuntimeClass. Always returns a non-nil NodePlacement; the
// caller decides what a mismatch means for the step status.
func (p *placementChecker) check(ctx context.Context, runtimeClassName string, pods []corev1.Pod) *schema.NodePlacement {
	np := &schema.NodePlacement{}

	rc := p.runtimeClass(ctx, runtimeClassName)
	if rc.problem != "" {
		np.Message = rc.problem + "; node placement not checked (run 'agentmoat preflight')"
		return np
	}
	np.Selector = rc.selector

	names := distinctNodeNames(pods)
	if len(names) == 0 {
		np.Message = "no pod has been scheduled to a node yet; node placement not checked"
		return np
	}
	np.Nodes = names

	sel := labels.SelectorFromSet(labels.Set(rc.selector))
	for _, name := range names {
		n := p.node(ctx, name)
		if n.problem != "" {
			np.Message = n.problem + "; node placement not checked"
			return np
		}
		if !sel.Matches(labels.Set(n.labels)) {
			np.Mismatched = append(np.Mismatched, name)
		}
	}

	np.Checked = true
	if len(np.Mismatched) > 0 {
		np.Message = fmt.Sprintf("%d of %d node(s) hosting these pods do not match RuntimeClass %q nodeSelector %s: %s",
			len(np.Mismatched), len(names), runtimeClassName, formatSelector(rc.selector), strings.Join(np.Mismatched, ", "))
	}
	return np
}

// runtimeClass reads (once) the RuntimeClass and extracts its selector.
func (p *placementChecker) runtimeClass(ctx context.Context, name string) rcLookup {
	if cached, ok := p.rcs[name]; ok {
		return cached
	}
	var out rcLookup
	rc, err := p.client.NodeV1().RuntimeClasses().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		out.problem = fmt.Sprintf("RuntimeClass %q not found", name)
	case err != nil:
		out.problem = fmt.Sprintf("RuntimeClass %q unreadable: %v", name, err)
	case rc.Scheduling == nil || len(rc.Scheduling.NodeSelector) == 0:
		out.problem = fmt.Sprintf("RuntimeClass %q has no scheduling.nodeSelector", name)
	default:
		out.selector = rc.Scheduling.NodeSelector
	}
	p.rcs[name] = out
	return out
}

// node reads (once) a Node and keeps its labels.
func (p *placementChecker) node(ctx context.Context, name string) nodeLookup {
	if cached, ok := p.nodes[name]; ok {
		return cached
	}
	var out nodeLookup
	n, err := p.client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		out.problem = fmt.Sprintf("node %q not found", name)
	case err != nil:
		out.problem = fmt.Sprintf("node %q unreadable: %v", name, err)
	default:
		out.labels = n.Labels
	}
	p.nodes[name] = out
	return out
}

// distinctNodeNames returns the sorted set of non-empty spec.nodeName
// values across pods. Pending pods have no node yet and are skipped.
func distinctNodeNames(pods []corev1.Pod) []string {
	seen := map[string]bool{}
	for i := range pods {
		if n := pods[i].Spec.NodeName; n != "" {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
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
