// Tests for the node placement check (placement.go). The spec-level and
// probe behaviors are covered in verifier_test.go; here every case is
// about where the pods run relative to the RuntimeClass nodeSelector.
package verifier

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func placementRC(name string, selector map[string]string) *nodev1.RuntimeClass {
	rc := &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Handler: name}
	if selector != nil {
		rc.Scheduling = &nodev1.Scheduling{NodeSelector: selector}
	}
	return rc
}

func placementNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}

// podOnNode is runningPod with runtimeClassName=gvisor scheduled to node.
func podOnNode(ns, name string, labels map[string]string, node string) *corev1.Pod {
	p := runningPod(ns, name, labels, strPtr("gvisor"))
	p.Spec.NodeName = node
	return p
}

var (
	gvSel  = map[string]string{"runtime": "gvisor"}
	webSel = map[string]string{"app": "web"}
)

func runPlacementVerify(t *testing.T, client *fake.Clientset) schema.VerifyResult {
	t.Helper()
	report, err := Verify(context.Background(), Options{
		Client: client,
		Plan:   makePlan(stepRef("Deployment", "ns", "web", "gvisor")),
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(report.Spec.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(report.Spec.Results))
	}
	return report.Spec.Results[0]
}

func TestVerify_NodePlacement_AllMatchingStaysOK(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		placementRC("gvisor", gvSel),
		placementNode("gv-1", gvSel),
		deployment("ns", "web", webSel),
		podOnNode("ns", "web-0", webSel, "gv-1"),
		podOnNode("ns", "web-1", webSel, "gv-1"),
	)
	r := runPlacementVerify(t, client)
	if r.Status != schema.VerifyStatusOK {
		t.Fatalf("status = %s (%s)", r.Status, r.Message)
	}
	np := r.NodePlacement
	if np == nil || !np.Checked || len(np.Mismatched) != 0 || strings.Join(np.Nodes, ",") != "gv-1" {
		t.Fatalf("NodePlacement = %+v", np)
	}
	if np.Selector["runtime"] != "gvisor" {
		t.Fatalf("Selector = %v", np.Selector)
	}
}

func TestVerify_NodePlacement_MismatchDemotes(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		placementRC("gvisor", gvSel),
		placementNode("gv-1", gvSel),
		placementNode("runc-1", map[string]string{"kubernetes.io/os": "linux"}),
		deployment("ns", "web", webSel),
		podOnNode("ns", "web-0", webSel, "gv-1"),
		podOnNode("ns", "web-1", webSel, "runc-1"),
	)
	r := runPlacementVerify(t, client)
	if r.Status != schema.VerifyStatusMismatch {
		t.Fatalf("status = %s, want mismatch (%s)", r.Status, r.Message)
	}
	np := r.NodePlacement
	if np == nil || !np.Checked || strings.Join(np.Mismatched, ",") != "runc-1" {
		t.Fatalf("NodePlacement = %+v", np)
	}
	if !strings.Contains(r.Message, "runc-1") || !strings.Contains(r.Message, "runtime=gvisor") {
		t.Fatalf("message = %q", r.Message)
	}
	// The spec-level fields still describe a spec that looked right.
	if r.Actual != "gvisor" || r.Expected != "gvisor" {
		t.Fatalf("expected/actual = %q/%q", r.Expected, r.Actual)
	}
}

func TestVerify_NodePlacement_UncheckedCases(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		objects []runtime.Object
		wantMsg string
	}{
		{
			name: "runtimeclass missing",
			objects: []runtime.Object{
				placementNode("gv-1", gvSel),
				deployment("ns", "web", webSel),
				podOnNode("ns", "web-0", webSel, "gv-1"),
			},
			wantMsg: `RuntimeClass "gvisor" not found`,
		},
		{
			name: "runtimeclass without selector",
			objects: []runtime.Object{
				placementRC("gvisor", nil),
				placementNode("gv-1", gvSel),
				deployment("ns", "web", webSel),
				podOnNode("ns", "web-0", webSel, "gv-1"),
			},
			wantMsg: "no scheduling.nodeSelector",
		},
		{
			name: "pods not scheduled yet",
			objects: []runtime.Object{
				placementRC("gvisor", gvSel),
				deployment("ns", "web", webSel),
				podOnNode("ns", "web-0", webSel, ""),
			},
			wantMsg: "no pod has been scheduled",
		},
		{
			name: "hosting node not readable",
			objects: []runtime.Object{
				placementRC("gvisor", gvSel),
				deployment("ns", "web", webSel),
				podOnNode("ns", "web-0", webSel, "ghost"),
			},
			wantMsg: `node "ghost" not found`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := runPlacementVerify(t, fake.NewSimpleClientset(tc.objects...))
			if r.Status != schema.VerifyStatusOK {
				t.Fatalf("status = %s, want ok: an unchecked placement must not fail verify (%s)", r.Status, r.Message)
			}
			np := r.NodePlacement
			if np == nil || np.Checked || len(np.Mismatched) != 0 {
				t.Fatalf("NodePlacement = %+v, want unchecked", np)
			}
			if !strings.Contains(np.Message, tc.wantMsg) {
				t.Fatalf("message = %q, want substring %q", np.Message, tc.wantMsg)
			}
		})
	}
}

func TestVerify_NodePlacement_ForbiddenIsUnchecked(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		placementRC("gvisor", gvSel),
		deployment("ns", "web", webSel),
		podOnNode("ns", "web-0", webSel, "gv-1"),
	)
	client.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(k8sschema.GroupResource{Resource: "nodes"}, "gv-1", errors.New("denied"))
	})
	r := runPlacementVerify(t, client)
	if r.Status != schema.VerifyStatusOK {
		t.Fatalf("status = %s; RBAC on nodes must not fail verify", r.Status)
	}
	if r.NodePlacement == nil || r.NodePlacement.Checked || !strings.Contains(r.NodePlacement.Message, "unreadable") {
		t.Fatalf("NodePlacement = %+v", r.NodePlacement)
	}
}

func TestVerify_NodePlacement_SkippedWhenSpecMismatches(t *testing.T) {
	t.Parallel()
	pod := podOnNode("ns", "web-0", webSel, "gv-1")
	pod.Spec.RuntimeClassName = nil
	client := fake.NewSimpleClientset(
		placementRC("gvisor", gvSel),
		placementNode("gv-1", gvSel),
		deployment("ns", "web", webSel),
		pod,
	)
	r := runPlacementVerify(t, client)
	if r.Status != schema.VerifyStatusMismatch {
		t.Fatalf("status = %s", r.Status)
	}
	if r.NodePlacement != nil {
		t.Fatalf("NodePlacement = %+v, want nil when the spec check already failed", r.NodePlacement)
	}
}

// TestVerify_NodePlacement_CachesReads: N steps on the same node must cost
// one RuntimeClass GET and one Node GET, not N of each.
func TestVerify_NodePlacement_CachesReads(t *testing.T) {
	t.Parallel()
	apiSel := map[string]string{"app": "api"}
	client := fake.NewSimpleClientset(
		placementRC("gvisor", gvSel),
		placementNode("gv-1", gvSel),
		deployment("ns", "web", webSel),
		deployment("ns", "api", apiSel),
		podOnNode("ns", "web-0", webSel, "gv-1"),
		podOnNode("ns", "api-0", apiSel, "gv-1"),
	)
	_, err := Verify(context.Background(), Options{
		Client: client,
		Plan:   makePlan(stepRef("Deployment", "ns", "web", "gvisor"), stepRef("Deployment", "ns", "api", "gvisor")),
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	rcGets, nodeGets := 0, 0
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			continue
		}
		switch a.GetResource().Resource {
		case "runtimeclasses":
			rcGets++
		case "nodes":
			nodeGets++
		}
	}
	if rcGets != 1 || nodeGets != 1 {
		t.Fatalf("runtimeclass gets = %d, node gets = %d; want 1 and 1", rcGets, nodeGets)
	}
}
