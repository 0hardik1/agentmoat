// Unit tests for the probe_nvproxy tool: the dry-run gate (omitted or true
// creates nothing; explicit false creates the pod), and the report shape.
// The fake clientset cannot serve pod logs, so the real-run test asserts
// on the pod lifecycle and the resulting nvproxy-probe-failed finding
// rather than on parsed drivers (pkg/probe covers parsing).
package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	"github.com/0hardik1/agentmoat/pkg/probe"
)

func podActions(c *fake.Clientset, verb string) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

func TestProbeNvproxyHandler_DryRunByDefault(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorReadyObjects()...)
	srv := newTestServer(client)

	for _, args := range []map[string]any{{}, {"dry_run": true}} {
		result, err := callTool(t, srv, "probe_nvproxy", args)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		var report schema.PreflightReport
		readTool(t, result, &report)
		if report.Kind != schema.KindPreflightReport || report.Metadata.Probe == nil || !report.Metadata.Probe.DryRun {
			t.Fatalf("args %v: report = %s probe=%+v", args, report.Kind, report.Metadata.Probe)
		}
		if report.Metadata.Probe.Node != "gv-1" || report.Metadata.Probe.Namespace != probe.DefaultNamespace {
			t.Fatalf("probe metadata = %+v", report.Metadata.Probe)
		}
	}
	if podActions(client, "create") != 0 {
		t.Fatal("dry run created a pod")
	}
}

func TestProbeNvproxyHandler_ExplicitFalseRunsAndCleansUp(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorReadyObjects()...)
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		action.(k8stesting.CreateAction).GetObject().(*corev1.Pod).Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})
	srv := newTestServer(client)

	result, err := callTool(t, srv, "probe_nvproxy", map[string]any{
		"dry_run": false, "namespace": "agentmoat-probe", "timeout_seconds": 30,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.PreflightReport
	readTool(t, result, &report)
	pm := report.Metadata.Probe
	if pm == nil || pm.DryRun || pm.Namespace != "agentmoat-probe" || pm.Node != "gv-1" {
		t.Fatalf("probe metadata = %+v", pm)
	}
	if podActions(client, "create") != 1 || podActions(client, "delete") != 1 {
		t.Fatalf("create/delete = %d/%d", podActions(client, "create"), podActions(client, "delete"))
	}
	// The fake's canned log ("fake logs") is not runsc output, so the
	// probe reports a failure finding instead of driver facts.
	found := false
	for _, f := range report.Spec.Findings {
		found = found || f.ID == preflight.FindingNvproxyProbeFailed
	}
	if !found || pm.Succeeded {
		t.Fatalf("expected nvproxy-probe-failed on canned logs; findings=%+v succeeded=%v", report.Spec.Findings, pm.Succeeded)
	}
}

func TestProbeNvproxyHandler_RejectsNonBooleanDryRun(t *testing.T) {
	t.Parallel()
	srv := newTestServer(fake.NewSimpleClientset(gvisorReadyObjects()...))
	result, err := callTool(t, srv, "probe_nvproxy", map[string]any{"dry_run": "false"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	expectToolError(t, result, "dry_run must be a boolean")
}
