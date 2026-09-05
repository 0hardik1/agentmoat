// Package preflight: cluster inventory.
//
// Collect turns two API reads (one RuntimeClass, the node list) into
// schema.ClusterFacts. Everything derived here is a count or a sorted list,
// so the same cluster state always yields the same facts and the same
// findings. The GPU inventory (gpu.go) rides on the same node list.
package preflight

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Collect reads the RuntimeClass and the node list and returns the facts.
//
// A missing RuntimeClass is a fact (Found=false), not an error: the
// preflight's job is to report it. Any other API error is returned wrapped
// with %w so callers can test apierrors.IsForbidden and degrade (scan does;
// the preflight verb and the apply gate do not, because the whole point of
// those is to read the nodes).
func Collect(ctx context.Context, client kubernetes.Interface, runtimeClassName string) (*schema.ClusterFacts, error) {
	if client == nil {
		return nil, fmt.Errorf("preflight: client is nil")
	}
	if runtimeClassName == "" {
		runtimeClassName = schema.DefaultRuntimeClassName
	}

	facts := &schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{Name: runtimeClassName},
	}

	var tolerations []corev1.Toleration
	rc, err := client.NodeV1().RuntimeClasses().Get(ctx, runtimeClassName, metav1.GetOptions{})
	switch {
	case err == nil:
		facts.RuntimeClass = runtimeClassFacts(rc)
		if rc.Scheduling != nil {
			tolerations = rc.Scheduling.Tolerations
		}
	case apierrors.IsNotFound(err):
		// Found stays false; Evaluate turns that into runtimeclass-missing.
	default:
		return nil, fmt.Errorf("preflight: getting RuntimeClass %q: %w", runtimeClassName, err)
	}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("preflight: listing nodes: %w", err)
	}
	fillNodeFacts(facts, nodes.Items, tolerations)
	facts.GPU = collectGPU(nodes.Items, selectorOf(facts))
	RefreshGPUSupport(facts)
	return facts, nil
}

// selectorOf turns the RuntimeClass nodeSelector into a label selector,
// nil when the RuntimeClass has none (nothing matches then; Evaluate
// reports the missing selector as the error).
func selectorOf(facts *schema.ClusterFacts) labels.Selector {
	if len(facts.RuntimeClass.NodeSelector) == 0 {
		return nil
	}
	return labels.SelectorFromSet(labels.Set(facts.RuntimeClass.NodeSelector))
}

// MatchesRuntimeClass reports whether the node satisfies the RuntimeClass
// nodeSelector recorded in facts. False when there is no selector.
func MatchesRuntimeClass(facts *schema.ClusterFacts, n *corev1.Node) bool {
	sel := selectorOf(facts)
	return sel != nil && sel.Matches(labels.Set(n.Labels))
}

// runtimeClassFacts projects a RuntimeClass object down to the wire shape.
func runtimeClassFacts(rc *nodev1.RuntimeClass) schema.RuntimeClassFacts {
	f := schema.RuntimeClassFacts{
		Name:    rc.Name,
		Found:   true,
		Handler: rc.Handler,
	}
	if rc.Scheduling != nil {
		if len(rc.Scheduling.NodeSelector) > 0 {
			f.NodeSelector = make(map[string]string, len(rc.Scheduling.NodeSelector))
			for k, v := range rc.Scheduling.NodeSelector {
				f.NodeSelector[k] = v
			}
		}
		f.Tolerations = len(rc.Scheduling.Tolerations)
	}
	if rc.Overhead != nil && len(rc.Overhead.PodFixed) > 0 {
		f.Overhead = make(map[string]string, len(rc.Overhead.PodFixed))
		for name, qty := range rc.Overhead.PodFixed {
			f.Overhead[string(name)] = qty.String()
		}
	}
	return f
}

// fillNodeFacts walks the node list once and fills Nodes and Platform.
// "Matching" is evaluated with the same label-selector semantics the
// scheduler applies to the merged nodeSelector; with no selector nothing
// matches, and Evaluate reports the missing selector as the error.
func fillNodeFacts(facts *schema.ClusterFacts, nodes []corev1.Node, rcTolerations []corev1.Toleration) {
	sel := selectorOf(facts)

	nf := &facts.Nodes
	for i := range nodes {
		n := &nodes[i]
		nf.Total++
		ready := NodeIsReady(n)
		if ready {
			nf.Ready++
		}
		matches := sel != nil && sel.Matches(labels.Set(n.Labels))
		countPlatform(&facts.Platform, n, matches)
		if !matches {
			continue
		}
		nf.MatchingSelector++
		nf.MatchingNames = append(nf.MatchingNames, n.Name)
		if ready {
			nf.MatchingAndReady++
		}
		if taintedWithoutToleration(n, rcTolerations) {
			nf.MatchingTaintedWithoutToleration++
		}
	}
	sort.Strings(nf.MatchingNames)
}

// countPlatform updates the platform counters for one node. Auto Mode
// nodes also run Bottlerocket; they are counted once, under Auto Mode,
// because the remediation differs (you cannot add a node group inside
// Auto Mode's managed pool, you add a separate one).
func countPlatform(pf *schema.PlatformFacts, n *corev1.Node, matches bool) {
	auto := isEKSAutoMode(n)
	bottlerocket := !auto && isBottlerocket(n)
	if auto {
		pf.EKSAutoModeNodes++
		if matches {
			pf.EKSAutoModeMatchingNodes++
		}
	}
	if bottlerocket {
		pf.BottlerocketNodes++
		if matches {
			pf.BottlerocketMatchingNodes++
		}
	}
	if _, ok := n.Labels[schema.KarpenterNodePoolLabel]; ok {
		pf.KarpenterNodes++
	}
}

// NodeIsReady reports whether the node's Ready condition is True.
func NodeIsReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// taintedWithoutToleration reports whether the node carries a NoSchedule
// or NoExecute taint that none of the RuntimeClass tolerations tolerate.
// PreferNoSchedule is ignored: it only lowers scheduling priority.
func taintedWithoutToleration(n *corev1.Node, tolerations []corev1.Toleration) bool {
	for i := range n.Spec.Taints {
		taint := &n.Spec.Taints[i]
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for j := range tolerations {
			if tolerates(&tolerations[j], taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return true
		}
	}
	return false
}

// tolerates mirrors the scheduler's Toleration.ToleratesTaint for the two
// GA operators: an empty effect or key on the toleration matches any
// taint; Equal (the default) needs the same value; Exists ignores the
// value. The upstream method also handles the alpha numeric comparison
// operators and takes a logger for them; RuntimeClass tolerations in
// practice never use those, so this local copy keeps the dependency out.
func tolerates(t *corev1.Toleration, taint *corev1.Taint) bool {
	if t.Effect != "" && t.Effect != taint.Effect {
		return false
	}
	if t.Key != "" && t.Key != taint.Key {
		return false
	}
	switch t.Operator {
	case "", corev1.TolerationOpEqual:
		return t.Value == taint.Value
	case corev1.TolerationOpExists:
		return true
	default:
		return false
	}
}

// isEKSAutoMode: EKS Auto Mode stamps eks.amazonaws.com/compute-type=auto
// on every managed instance.
func isEKSAutoMode(n *corev1.Node) bool {
	return n.Labels[schema.EKSAutoModeLabel] == schema.EKSAutoModeLabelValue
}

// isBottlerocket: Bottlerocket reports "Bottlerocket OS <version> (<variant>)"
// as the node OS image.
func isBottlerocket(n *corev1.Node) bool {
	return strings.HasPrefix(n.Status.NodeInfo.OSImage, schema.BottlerocketOSImagePrefix)
}
