// Tests for pkg/preflight.
//
// Collect is exercised against a fake clientset so the label, taint, and
// platform detection run on real corev1 / nodev1 objects. Evaluate is pure,
// so every severity branch is a table row over hand-built ClusterFacts.
package preflight

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// --- fixtures -------------------------------------------------------------

type nodeOpt func(*corev1.Node)

func withLabels(kv ...string) nodeOpt {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		for i := 0; i+1 < len(kv); i += 2 {
			n.Labels[kv[i]] = kv[i+1]
		}
	}
}

func notReady() nodeOpt {
	return func(n *corev1.Node) {
		n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	}
}

func withTaint(key, value string, effect corev1.TaintEffect) nodeOpt {
	return func(n *corev1.Node) {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: key, Value: value, Effect: effect})
	}
}

func withOSImage(img string) nodeOpt {
	return func(n *corev1.Node) { n.Status.NodeInfo.OSImage = img }
}

// node builds a Ready node running a plain Linux image unless options say
// otherwise.
func node(name string, opts ...nodeOpt) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:   corev1.NodeSystemInfo{OSImage: "Amazon Linux 2023.7.20250818"},
		},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// gvisorRC returns the shipped RuntimeClass shape: handler gvisor, selector
// runtime=gvisor, toleration for runtime=gvisor:NoSchedule, overhead set.
func gvisorRC() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gvisor"},
		Handler:    "gvisor",
		Scheduling: &nodev1.Scheduling{
			NodeSelector: map[string]string{"runtime": "gvisor"},
			Tolerations: []corev1.Toleration{{
				Key: "runtime", Operator: corev1.TolerationOpEqual, Value: "gvisor", Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Overhead: &nodev1.Overhead{PodFixed: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("140Mi"),
			corev1.ResourceCPU:    resource.MustParse("250m"),
		}},
	}
}

func findingIDs(fs []schema.PreflightFinding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.ID+":"+string(f.Severity))
	}
	return out
}

func joined(ids []string) string { return strings.Join(ids, " ") }

// --- Collect ---------------------------------------------------------------

func TestCollect_HappyPath(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		gvisorRC(),
		node("cp", withTaint("node-role.kubernetes.io/control-plane", "", corev1.TaintEffectNoSchedule)),
		node("gv-a", withLabels("runtime", "gvisor", schema.KarpenterNodePoolLabel, "gvisor-pool"),
			withTaint("runtime", "gvisor", corev1.TaintEffectNoSchedule)),
		node("gv-b", withLabels("runtime", "gvisor"), notReady()),
		node("runc-1"),
	)

	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	want := schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{
			Name: "gvisor", Found: true, Handler: "gvisor",
			NodeSelector: map[string]string{"runtime": "gvisor"}, Tolerations: 1,
			Overhead: map[string]string{"memory": "140Mi", "cpu": "250m"},
		},
		// gv-a is tainted runtime=gvisor:NoSchedule, which the RuntimeClass
		// tolerates, so it does not count as tainted-without-toleration.
		Nodes: schema.NodeFacts{
			Total: 4, Ready: 3, MatchingSelector: 2, MatchingAndReady: 1,
			MatchingTaintedWithoutToleration: 0, MatchingNames: []string{"gv-a", "gv-b"},
		},
		Platform: schema.PlatformFacts{KarpenterNodes: 1},
	}
	if !reflect.DeepEqual(*facts, want) {
		t.Fatalf("facts mismatch:\n got: %+v\nwant: %+v", *facts, want)
	}
}

func TestCollect_RuntimeClassMissingIsAFactNotAnError(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(node("a", withLabels("runtime", "gvisor")))
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.RuntimeClass.Found {
		t.Fatal("Found = true for a missing RuntimeClass")
	}
	if facts.RuntimeClass.Name != "gvisor" {
		t.Fatalf("Name = %q", facts.RuntimeClass.Name)
	}
	// No selector -> nothing matches, even though a node carries the label.
	if facts.Nodes.MatchingSelector != 0 || facts.Nodes.Total != 1 {
		t.Fatalf("Nodes = %+v", facts.Nodes)
	}
}

func TestCollect_DefaultsRuntimeClassName(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorRC())
	facts, err := Collect(context.Background(), client, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !facts.RuntimeClass.Found || facts.RuntimeClass.Name != schema.DefaultRuntimeClassName {
		t.Fatalf("RuntimeClass = %+v", facts.RuntimeClass)
	}
}

func TestCollect_TaintDetection(t *testing.T) {
	t.Parallel()
	rc := gvisorRC()
	rc.Scheduling.Tolerations = nil // RuntimeClass tolerates nothing.
	client := fake.NewSimpleClientset(
		rc,
		node("tainted", withLabels("runtime", "gvisor"), withTaint("runtime", "gvisor", corev1.TaintEffectNoSchedule)),
		node("prefer", withLabels("runtime", "gvisor"), withTaint("x", "y", corev1.TaintEffectPreferNoSchedule)),
		node("clean", withLabels("runtime", "gvisor")),
	)
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// PreferNoSchedule does not count; the hard taint does.
	if facts.Nodes.MatchingTaintedWithoutToleration != 1 {
		t.Fatalf("MatchingTaintedWithoutToleration = %d, want 1", facts.Nodes.MatchingTaintedWithoutToleration)
	}
}

func TestCollect_PlatformDetection(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		gvisorRC(),
		// Auto Mode node: also Bottlerocket, must be counted once (as Auto Mode).
		node("auto-1", withLabels(schema.EKSAutoModeLabel, schema.EKSAutoModeLabelValue, schema.KarpenterNodePoolLabel, "general-purpose"),
			withOSImage("Bottlerocket OS 1.40.0 (aws-k8s-1.33)")),
		node("br-1", withLabels("runtime", "gvisor"), withOSImage("Bottlerocket OS 1.40.0 (aws-k8s-1.33)")),
		node("al-1", withLabels("runtime", "gvisor")),
	)
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	p := facts.Platform
	if p.EKSAutoModeNodes != 1 || p.EKSAutoModeMatchingNodes != 0 {
		t.Fatalf("Auto Mode counts = %d/%d", p.EKSAutoModeNodes, p.EKSAutoModeMatchingNodes)
	}
	if p.BottlerocketNodes != 1 || p.BottlerocketMatchingNodes != 1 {
		t.Fatalf("Bottlerocket counts = %d/%d", p.BottlerocketNodes, p.BottlerocketMatchingNodes)
	}
	if p.KarpenterNodes != 1 {
		t.Fatalf("KarpenterNodes = %d", p.KarpenterNodes)
	}
}

func TestCollect_ForbiddenIsWrappedAndDetectable(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorRC())
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(k8sschema.GroupResource{Resource: "nodes"}, "", errors.New("denied"))
	})
	_, err := Collect(context.Background(), client, "gvisor")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !apierrors.IsForbidden(err) {
		t.Fatalf("IsForbidden(%v) = false; scan relies on this to degrade", err)
	}
	if !strings.Contains(err.Error(), "listing nodes") {
		t.Fatalf("error lacks context: %v", err)
	}
}

func TestCollect_NilClient(t *testing.T) {
	t.Parallel()
	var client kubernetes.Interface
	if _, err := Collect(context.Background(), client, "gvisor"); err == nil {
		t.Fatal("expected error for nil client")
	}
}

// --- Evaluate -------------------------------------------------------------

func TestEvaluate(t *testing.T) {
	t.Parallel()
	ready := func() *schema.ClusterFacts {
		return &schema.ClusterFacts{
			RuntimeClass: schema.RuntimeClassFacts{
				Name: "gvisor", Found: true, Handler: "gvisor",
				NodeSelector: map[string]string{"runtime": "gvisor"}, Tolerations: 1,
				Overhead: map[string]string{"memory": "140Mi"},
			},
			Nodes: schema.NodeFacts{Total: 3, Ready: 3, MatchingSelector: 2, MatchingAndReady: 2, MatchingNames: []string{"a", "b"}},
		}
	}

	tests := []struct {
		name     string
		mutate   func(*schema.ClusterFacts)
		want     string // space-joined id:severity, in the sorted order Evaluate promises
		blocking bool
	}{
		{
			name:   "ready cluster has no findings",
			mutate: func(*schema.ClusterFacts) {},
			want:   "",
		},
		{
			name:   "missing runtimeclass",
			mutate: func(f *schema.ClusterFacts) { f.RuntimeClass = schema.RuntimeClassFacts{Name: "gvisor"} },
			want:   "runtimeclass-missing:error", blocking: true,
		},
		{
			name: "no node selector",
			mutate: func(f *schema.ClusterFacts) {
				f.RuntimeClass.NodeSelector = nil
				f.Nodes.MatchingSelector, f.Nodes.MatchingAndReady = 0, 0
			},
			want: "runtimeclass-no-node-selector:error", blocking: true,
		},
		{
			name:   "no matching nodes",
			mutate: func(f *schema.ClusterFacts) { f.Nodes.MatchingSelector, f.Nodes.MatchingAndReady = 0, 0 },
			want:   "runtimeclass-no-matching-nodes:error", blocking: true,
		},
		{
			name:   "matching nodes but none ready",
			mutate: func(f *schema.ClusterFacts) { f.Nodes.MatchingAndReady = 0 },
			want:   "runtimeclass-no-ready-matching-nodes:error", blocking: true,
		},
		{
			name:   "every matching node tainted without toleration",
			mutate: func(f *schema.ClusterFacts) { f.Nodes.MatchingTaintedWithoutToleration = 2 },
			want:   "runtimeclass-taint-without-toleration:error", blocking: true,
		},
		{
			name:   "some matching nodes tainted without toleration",
			mutate: func(f *schema.ClusterFacts) { f.Nodes.MatchingTaintedWithoutToleration = 1 },
			want:   "runtimeclass-taint-without-toleration:warn",
		},
		{
			name:   "all matching nodes are EKS Auto Mode",
			mutate: func(f *schema.ClusterFacts) { f.Platform.EKSAutoModeMatchingNodes = 2; f.Platform.EKSAutoModeNodes = 3 },
			want:   "eks-auto-mode-nodes:error", blocking: true,
		},
		{
			name:   "some matching nodes are EKS Auto Mode",
			mutate: func(f *schema.ClusterFacts) { f.Platform.EKSAutoModeMatchingNodes = 1; f.Platform.EKSAutoModeNodes = 1 },
			want:   "eks-auto-mode-nodes:warn",
		},
		{
			name: "auto mode cluster without runtimeclass reports both, errors first",
			mutate: func(f *schema.ClusterFacts) {
				f.RuntimeClass = schema.RuntimeClassFacts{Name: "gvisor"}
				f.Nodes = schema.NodeFacts{Total: 3, Ready: 3}
				f.Platform.EKSAutoModeNodes = 3
			},
			want: "eks-auto-mode-nodes:error runtimeclass-missing:error", blocking: true,
		},
		{
			name:   "all matching nodes are Bottlerocket",
			mutate: func(f *schema.ClusterFacts) { f.Platform.BottlerocketMatchingNodes = 2 },
			want:   "bottlerocket-nodes:error", blocking: true,
		},
		{
			name:   "some matching nodes are Bottlerocket",
			mutate: func(f *schema.ClusterFacts) { f.Platform.BottlerocketMatchingNodes = 1 },
			want:   "bottlerocket-nodes:warn",
		},
		{
			name:   "no overhead is info only",
			mutate: func(f *schema.ClusterFacts) { f.RuntimeClass.Overhead = nil },
			want:   "runtimeclass-no-overhead:info",
		},
		{
			name: "sorted error, warn, info then id",
			mutate: func(f *schema.ClusterFacts) {
				f.RuntimeClass.Overhead = nil
				f.Nodes.MatchingTaintedWithoutToleration = 1
				f.Platform.EKSAutoModeMatchingNodes = 2
			},
			want: "eks-auto-mode-nodes:error runtimeclass-taint-without-toleration:warn runtimeclass-no-overhead:info", blocking: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := ready()
			tc.mutate(facts)
			got := Evaluate(facts)
			if got == nil {
				t.Fatal("Evaluate returned nil; must be a non-nil slice")
			}
			if joined(findingIDs(got)) != tc.want {
				t.Fatalf("findings = %q, want %q", joined(findingIDs(got)), tc.want)
			}
			if IsBlocking(got) != tc.blocking {
				t.Fatalf("IsBlocking = %v, want %v", IsBlocking(got), tc.blocking)
			}
			for _, f := range got {
				if f.Message == "" {
					t.Errorf("finding %s has an empty message", f.ID)
				}
				if f.Severity != schema.SeverityInfo && f.Remediation == "" {
					t.Errorf("finding %s has no remediation", f.ID)
				}
			}
			sum := Summarize(got)
			if sum.Ready == tc.blocking || sum.Total != len(got) || sum.Error+sum.Warn+sum.Info != sum.Total {
				t.Fatalf("Summarize = %+v for %d findings (blocking=%v)", sum, len(got), tc.blocking)
			}
		})
	}
}

func TestEvaluate_NilFacts(t *testing.T) {
	t.Parallel()
	got := Evaluate(nil)
	if got == nil || len(got) != 0 {
		t.Fatalf("Evaluate(nil) = %v, want empty non-nil", got)
	}
}

func TestEvaluate_MessagesQuoteNumbers(t *testing.T) {
	t.Parallel()
	facts := &schema.ClusterFacts{
		RuntimeClass: schema.RuntimeClassFacts{Name: "gvisor", Found: true, NodeSelector: map[string]string{"b": "2", "a": "1"}},
		Nodes:        schema.NodeFacts{Total: 12},
	}
	got := Evaluate(facts)
	var msg string
	for _, f := range got {
		if f.ID == FindingRuntimeClassNoMatchingNodes {
			msg = f.Message
		}
	}
	// Selector rendered in key order so the message is deterministic.
	if !strings.Contains(msg, "0 of 12 nodes") || !strings.Contains(msg, "a=1,b=2") {
		t.Fatalf("message = %q", msg)
	}
}

// --- Run ------------------------------------------------------------------

func TestRun_EnvelopeAndSummary(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorRC(), node("gv-a", withLabels("runtime", "gvisor")))
	report, err := Run(context.Background(), Options{Client: client, Cluster: "kind-agentmoat", AgentmoatVersion: "test"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.APIVersion != schema.APIVersion || report.Kind != schema.KindPreflightReport {
		t.Fatalf("envelope = %s/%s", report.APIVersion, report.Kind)
	}
	if report.Metadata.RuntimeClassName != "gvisor" || report.Metadata.Cluster != "kind-agentmoat" || report.Metadata.GeneratedAt == "" {
		t.Fatalf("metadata = %+v", report.Metadata)
	}
	if !report.Spec.Summary.Ready || report.Spec.Summary.Total != 0 {
		t.Fatalf("summary = %+v findings = %v", report.Spec.Summary, report.Spec.Findings)
	}
	if report.Spec.Findings == nil {
		t.Fatal("Findings must be non-nil")
	}
}

func TestRun_NotReadyIsAReportNotAnError(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(node("runc-only"))
	report, err := Run(context.Background(), Options{Client: client})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if report.Spec.Summary.Ready || report.Spec.Summary.Error != 1 {
		t.Fatalf("summary = %+v", report.Spec.Summary)
	}
	if report.Spec.Findings[0].ID != FindingRuntimeClassMissing {
		t.Fatalf("first finding = %s", report.Spec.Findings[0].ID)
	}
}

func TestRun_NilClient(t *testing.T) {
	t.Parallel()
	if _, err := Run(context.Background(), Options{}); err == nil {
		t.Fatal("expected error")
	}
}
