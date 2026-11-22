// Tests for the sandbox co-location check (sandbox.go).
//
// BuildSandboxFacts and the findings are pure, so most cases are tables
// over hand-built pods. Collect is exercised once end to end against a fake
// clientset, plus the RBAC-denied path, which must degrade to an info
// finding and never fail the preflight.
package preflight

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// --- pod fixtures ------------------------------------------------------------

type podOpt func(*corev1.Pod)

func onNode(name string) podOpt { return func(p *corev1.Pod) { p.Spec.NodeName = name } }

func withRuntimeClass(rc string) podOpt {
	return func(p *corev1.Pod) { p.Spec.RuntimeClassName = &rc }
}

func ownedByDS(name string) podOpt {
	return func(p *corev1.Pod) {
		ctrl := true
		p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{
			APIVersion: "apps/v1", Kind: "DaemonSet", Name: name, Controller: &ctrl,
		})
	}
}

func withEnv(name, value string) podOpt {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Env = append(p.Spec.Containers[0].Env, corev1.EnvVar{Name: name, Value: value})
	}
}

func withInitEnv(name, value string) podOpt {
	return func(p *corev1.Pod) {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
			Name: "init", Image: "busybox", Env: []corev1.EnvVar{{Name: name, Value: value}},
		})
	}
}

func inPhase(ph corev1.PodPhase) podOpt { return func(p *corev1.Pod) { p.Status.Phase = ph } }

// podIdentity mimics what the EKS Pod Identity webhook injects.
func podIdentity() podOpt {
	return func(p *corev1.Pod) {
		withEnv(envPodIdentityURI, "http://169.254.170.23/v1/credentials")(p)
		withEnv(envPodIdentityToken, "/var/run/secrets/pods.eks.amazonaws.com/serviceaccount/eks-pod-identity-token")(p)
	}
}

// irsa mimics what the IRSA (pod identity webhook) mutation injects.
func irsa() podOpt {
	return func(p *corev1.Pod) {
		withEnv(envIRSARoleARN, "arn:aws:iam::111122223333:role/example")(p)
		withEnv(envIRSATokenFile, "/var/run/secrets/eks.amazonaws.com/serviceaccount/token")(p)
	}
}

// pod builds a Running pod with one container in namespace ns.
func pod(ns, name string, opts ...podOpt) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: "app:1"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}

// --- BuildSandboxFacts -------------------------------------------------------

func TestBuildSandboxFacts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		pods []corev1.Pod
		want *schema.SandboxFacts
	}{
		{
			name: "no pods",
			want: nil,
		},
		{
			name: "only runc pods: nothing to report",
			pods: []corev1.Pod{pod("a", "web", onNode("n1")), pod("a", "db", onNode("n2"))},
			want: nil,
		},
		{
			name: "dedicated gVisor node: sandboxed pods and DaemonSets only",
			pods: []corev1.Pod{
				pod("agents", "a1", onNode("gv-1"), withRuntimeClass("gvisor")),
				pod("agents", "a2", onNode("gv-1"), withRuntimeClass("gvisor"), podIdentity()),
				pod("kube-system", "aws-node-x", onNode("gv-1"), ownedByDS("aws-node")),
				pod("kube-system", "eks-pod-identity-agent-x", onNode("gv-1"), ownedByDS("eks-pod-identity-agent")),
			},
			want: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name:                            "gv-1",
				RuntimeClasses:                  []string{"gvisor"},
				SandboxedPods:                   2,
				SandboxedPodsWithAWSCredentials: []string{"agents/a2"},
				RuncDaemonSetPods:               2,
			}}},
		},
		{
			name: "Nitro Sandbox node shared with runc workloads, mixed classes",
			pods: []corev1.Pod{
				pod("agents", "conf", onNode("ns-1"), withRuntimeClass(schema.NitroConfidentialSandboxRuntimeClass), irsa()),
				pod("agents", "iso", onNode("ns-1"), withRuntimeClass(schema.NitroIsolatedSandboxRuntimeClass), podIdentity()),
				pod("web", "frontend", onNode("ns-1")),
				pod("batch", "etl", onNode("ns-1"), withRuntimeClass("runc-custom")),
				pod("kube-system", "kube-proxy-x", onNode("ns-1"), ownedByDS("kube-proxy")),
				// A plain node with no sandboxed pod is left out entirely.
				pod("web", "other", onNode("plain-1")),
			},
			want: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name:                            "ns-1",
				RuntimeClasses:                  []string{"confidential-sandbox", "isolated-sandbox"},
				SandboxedPods:                   2,
				SandboxedPodsWithAWSCredentials: []string{"agents/conf", "agents/iso"},
				RuncWorkloadPods:                []string{"batch/etl", "web/frontend"},
				RuncDaemonSetPods:               1,
			}}},
		},
		{
			name: "finished and unscheduled pods are ignored",
			pods: []corev1.Pod{
				pod("agents", "a1", onNode("gv-1"), withRuntimeClass("gvisor")),
				pod("jobs", "done", onNode("gv-1"), inPhase(corev1.PodSucceeded)),
				pod("jobs", "crashed", onNode("gv-1"), inPhase(corev1.PodFailed)),
				pod("web", "pending", withRuntimeClass("gvisor")), // no nodeName yet
			},
			want: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name: "gv-1", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 1,
			}}},
		},
		{
			name: "credentials found in an init container count",
			pods: []corev1.Pod{
				pod("agents", "a1", onNode("gv-1"), withRuntimeClass("gvisor"), withInitEnv(envIRSARoleARN, "arn:aws:iam::111122223333:role/x")),
			},
			want: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name: "gv-1", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 1,
				SandboxedPodsWithAWSCredentials: []string{"agents/a1"},
			}}},
		},
		{
			name: "nodes sorted by name",
			pods: []corev1.Pod{
				pod("x", "b", onNode("node-b"), withRuntimeClass("gvisor")),
				pod("x", "a", onNode("node-a"), withRuntimeClass("gvisor")),
			},
			want: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{
				{Name: "node-a", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 1},
				{Name: "node-b", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 1},
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := BuildSandboxFacts("gvisor", tc.pods)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("facts mismatch:\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

func TestBuildSandboxFacts_CustomTargetRuntimeClass(t *testing.T) {
	t.Parallel()
	// With --runtime-class runsc-kvm, a pod naming "gvisor" is no longer the
	// target and counts as runc; the Nitro classes still count as sandboxed.
	pods := []corev1.Pod{
		pod("a", "kvm", onNode("n1"), withRuntimeClass("runsc-kvm")),
		pod("a", "old", onNode("n1"), withRuntimeClass("gvisor")),
		pod("a", "nitro", onNode("n1"), withRuntimeClass(schema.NitroIsolatedSandboxRuntimeClass)),
	}
	got := BuildSandboxFacts("runsc-kvm", pods)
	if got == nil || len(got.Nodes) != 1 {
		t.Fatalf("got %+v", got)
	}
	n := got.Nodes[0]
	if n.SandboxedPods != 2 || !reflect.DeepEqual(n.RuncWorkloadPods, []string{"a/old"}) {
		t.Fatalf("node = %+v", n)
	}
}

func TestBuildSandboxFacts_Deterministic(t *testing.T) {
	t.Parallel()
	pods := []corev1.Pod{
		pod("z", "r2", onNode("n1")),
		pod("a", "s1", onNode("n1"), withRuntimeClass("gvisor"), podIdentity()),
		pod("m", "r1", onNode("n1")),
		pod("b", "s2", onNode("n1"), withRuntimeClass(schema.NitroIsolatedSandboxRuntimeClass), podIdentity()),
	}
	reversed := make([]corev1.Pod, len(pods))
	for i := range pods {
		reversed[len(pods)-1-i] = pods[i]
	}
	if a, b := BuildSandboxFacts("gvisor", pods), BuildSandboxFacts("gvisor", reversed); !reflect.DeepEqual(a, b) {
		t.Fatalf("input order changed the facts:\n%+v\n%+v", a, b)
	}
}

// --- findings ----------------------------------------------------------------

func TestEvaluate_SandboxFindings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		sandbox *schema.SandboxFacts
		want    []string
	}{
		{name: "nil facts: no findings", sandbox: nil, want: nil},
		{
			name:    "unchecked",
			sandbox: &schema.SandboxFacts{Checked: false, Reason: "forbidden"},
			want:    []string{"sandbox-colocation-unchecked:info"},
		},
		{
			name: "dedicated node without credentials: quiet",
			sandbox: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name: "gv-1", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 3, RuncDaemonSetPods: 4,
			}}},
			want: nil,
		},
		{
			name: "dedicated node with credentials: info only",
			sandbox: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name: "gv-1", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 1,
				SandboxedPodsWithAWSCredentials: []string{"a/x"},
			}}},
			want: []string{"sandbox-pod-aws-credentials:info"},
		},
		{
			name: "shared node with credentials: warn and info",
			sandbox: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name: "ns-1", RuntimeClasses: []string{"isolated-sandbox"}, SandboxedPods: 1,
				SandboxedPodsWithAWSCredentials: []string{"a/x"}, RuncWorkloadPods: []string{"web/frontend"},
			}}},
			want: []string{"sandbox-node-runc-colocation:warn", "sandbox-pod-aws-credentials:info"},
		},
		{
			name: "shared node without credentials: warn only",
			sandbox: &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{{
				Name: "gv-1", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 1, RuncWorkloadPods: []string{"web/frontend"},
			}}},
			want: []string{"sandbox-node-runc-colocation:warn"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := sandboxFindings(&schema.ClusterFacts{Sandbox: tc.sandbox})
			if ids := findingIDs(got); joined(ids) != joined(tc.want) {
				t.Fatalf("findings = %v, want %v", ids, tc.want)
			}
			for _, f := range got {
				if f.Severity == schema.SeverityError {
					t.Fatalf("sandbox finding %s must never block", f.ID)
				}
			}
		})
	}
}

func TestSandboxFindings_MessagesQuoteNumbersAndCapNames(t *testing.T) {
	t.Parallel()
	var nodes []schema.SandboxNode
	for _, name := range []string{"n1", "n2", "n3", "n4", "n5", "n6", "n7"} {
		nodes = append(nodes, schema.SandboxNode{
			Name: name, RuntimeClasses: []string{"gvisor"}, SandboxedPods: 2,
			SandboxedPodsWithAWSCredentials: []string{"agents/" + name},
			RuncWorkloadPods:                []string{"web/" + name},
		})
	}
	got := sandboxFindings(&schema.ClusterFacts{Sandbox: &schema.SandboxFacts{Checked: true, Nodes: nodes}})
	if len(got) != 2 {
		t.Fatalf("findings = %v", findingIDs(got))
	}
	colo := got[0].Message
	for _, want := range []string{"7 of 7 nodes", "7 runc workload pods", "n1 (1 runc, 2 sandboxed, 1 with AWS credentials)", "and 2 more", "7 sandboxed pods there"} {
		if !strings.Contains(colo, want) {
			t.Errorf("colocation message lacks %q: %s", want, colo)
		}
	}
	if strings.Contains(colo, "n6 (") {
		t.Errorf("colocation message should cap node names at %d: %s", maxNamesInMessage, colo)
	}
	creds := got[1].Message
	for _, want := range []string{"7 sandboxed pods get AWS credentials", "agents/n1", "and 2 more", "does not read IAM"} {
		if !strings.Contains(creds, want) {
			t.Errorf("credentials message lacks %q: %s", want, creds)
		}
	}
}

// --- Collect -----------------------------------------------------------------

func podObj(p corev1.Pod) *corev1.Pod { return &p }

func TestCollect_SandboxColocation(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		gvisorRC(),
		node("gv-1", withLabels("runtime", "gvisor"), withTaint("runtime", "gvisor", corev1.TaintEffectNoSchedule)),
		node("ns-1", withLabels(schema.NitroSandboxNodeLabel, schema.NitroSandboxNodeLabelValue)),
		node("plain-1"),
		podObj(pod("agents", "gv-agent", onNode("gv-1"), withRuntimeClass("gvisor"))),
		podObj(pod("agents", "nitro-agent", onNode("ns-1"), withRuntimeClass(schema.NitroIsolatedSandboxRuntimeClass), podIdentity())),
		podObj(pod("web", "frontend", onNode("ns-1"))),
		// A runc pod on a node that is not a candidate is never looked at.
		podObj(pod("web", "elsewhere", onNode("plain-1"))),
	)
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	want := &schema.SandboxFacts{Checked: true, Nodes: []schema.SandboxNode{
		{Name: "gv-1", RuntimeClasses: []string{"gvisor"}, SandboxedPods: 1},
		{
			Name: "ns-1", RuntimeClasses: []string{"isolated-sandbox"}, SandboxedPods: 1,
			SandboxedPodsWithAWSCredentials: []string{"agents/nitro-agent"},
			RuncWorkloadPods:                []string{"web/frontend"},
		},
	}}
	if !reflect.DeepEqual(facts.Sandbox, want) {
		t.Fatalf("Sandbox mismatch:\n got: %+v\nwant: %+v", facts.Sandbox, want)
	}
	ids := findingIDs(Evaluate(facts))
	for _, id := range []string{"sandbox-node-runc-colocation:warn", "sandbox-pod-aws-credentials:info"} {
		if !strings.Contains(joined(ids), id) {
			t.Errorf("Evaluate lacks %s: %v", id, ids)
		}
	}
}

func TestCollect_NoSandboxedPodsKeepsFactsAbsent(t *testing.T) {
	t.Parallel()
	// gVisor nodes exist but nothing has migrated yet: the facts must look
	// exactly as they did before this check existed.
	client := fake.NewSimpleClientset(
		gvisorRC(),
		node("gv-1", withLabels("runtime", "gvisor")),
		podObj(pod("web", "frontend", onNode("gv-1"))),
	)
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if facts.Sandbox != nil {
		t.Fatalf("Sandbox = %+v, want nil", facts.Sandbox)
	}
}

func TestCollect_PodListForbiddenDegrades(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorRC(), node("gv-1", withLabels("runtime", "gvisor")))
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(k8sschema.GroupResource{Resource: "pods"}, "", nil)
	})
	facts, err := Collect(context.Background(), client, "gvisor")
	if err != nil {
		t.Fatalf("Collect must not fail on a pod-list error: %v", err)
	}
	if facts.Sandbox == nil || facts.Sandbox.Checked || !strings.Contains(facts.Sandbox.Reason, "gv-1") {
		t.Fatalf("Sandbox = %+v", facts.Sandbox)
	}
	findings := Evaluate(facts)
	if IsBlocking(findings) {
		t.Fatalf("a pod-list error must not block: %v", findingIDs(findings))
	}
	if !strings.Contains(joined(findingIDs(findings)), "sandbox-colocation-unchecked:info") {
		t.Fatalf("findings = %v", findingIDs(findings))
	}
}

func TestCollect_NoCandidateNodesListsNoPods(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorRC(), node("plain-1"))
	listed := false
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		listed = true
		return false, nil, nil
	})
	if _, err := Collect(context.Background(), client, "gvisor"); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if listed {
		t.Fatal("pods were listed although no node can host sandboxed pods")
	}
}
