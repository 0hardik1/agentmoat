// Tests for the Probe orchestrator. The probe mechanics live in pkg/probe;
// here we check the wiring: defaults, dry-run reporting, and that a dry run
// creates nothing.
package agentmoat

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	"github.com/0hardik1/agentmoat/pkg/probe"
	"k8s.io/client-go/kubernetes/fake"
)

func TestProbe_DryRunReportsThePodAndCreatesNothing(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(gvisorReady()...)
	var stderr bytes.Buffer
	report, err := Probe(context.Background(), ProbeOptions{KubeClient: client, DryRun: true, Stderr: &stderr})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	wantProbe := &schema.ProbeMetadata{DryRun: true, Namespace: probe.DefaultNamespace, PodName: probe.PodName, Node: "gv-1",
		Image: probe.DefaultImage, RunscPath: probe.DefaultRunscPath}
	if !reflect.DeepEqual(report.Metadata.Probe, wantProbe) {
		t.Fatalf("Probe metadata = %+v, want %+v", report.Metadata.Probe, wantProbe)
	}
	if report.Kind != schema.KindPreflightReport || !report.Spec.Summary.Ready {
		t.Fatalf("report = %s ready=%v", report.Kind, report.Spec.Summary.Ready)
	}
	if !hasFinding(report, preflight.FindingNvproxyProbeDryRun) {
		t.Fatalf("missing dry-run finding: %+v", report.Spec.Findings)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() == "create" {
			t.Fatal("dry run created something")
		}
	}
	for _, want := range []string{"dry run, no pod will be created", "would create pod default/agentmoat-nvproxy-probe on node gv-1", "ready=true"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr missing %q:\n%s", want, stderr.String())
		}
	}
}

func hasFinding(r *schema.PreflightReport, id string) bool {
	for _, f := range r.Spec.Findings {
		if f.ID == id {
			return true
		}
	}
	return false
}

func TestProbe_NotReadyClusterSkipsTheProbe(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(applyWorkload()...) // no RuntimeClass
	report, err := Probe(context.Background(), ProbeOptions{KubeClient: client, DryRun: false, Stderr: new(bytes.Buffer)})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if report.Spec.Summary.Ready || report.Metadata.Probe.Node != "" {
		t.Fatalf("summary=%+v probe=%+v", report.Spec.Summary, report.Metadata.Probe)
	}
	ids := []string{}
	for _, f := range report.Spec.Findings {
		ids = append(ids, f.ID)
	}
	if strings.Join(ids, ",") != preflight.FindingRuntimeClassMissing+","+preflight.FindingNvproxyProbeSkipped {
		t.Fatalf("findings = %v", ids)
	}
}
