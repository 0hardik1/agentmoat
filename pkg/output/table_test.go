// Package output: smoke tests for the Phase 3 renderers (VerifyReport,
// ExplainDocument). The Phase 1/2 renderers are exercised by the integration
// e2e in scripts/e2e.sh; the new renderers also get a unit-level smoke here
// so a future refactor of the dispatcher cannot silently drop a Kind.
package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"sigs.k8s.io/yaml"
)

func sampleVerifyReport() *schema.VerifyReport {
	r := schema.NewVerifyReport()
	r.Metadata = schema.VerifyMetadata{
		GeneratedAt:      "2026-05-22T12:00:00Z",
		Cluster:          "kind-agentmoat-e2e",
		AgentmoatVersion: "test",
		PlanHash:         "hash-1",
		InPodProbe:       true,
	}
	r.Spec = schema.VerifySpec{
		Summary: schema.VerifySummary{Total: 2, OK: 1, Mismatch: 1, Error: 0},
		Results: []schema.VerifyResult{
			{
				Order:    1,
				Target:   schema.WorkloadRef{Kind: "Deployment", Namespace: "ns-a", Name: "web"},
				Status:   schema.VerifyStatusOK,
				Expected: "gvisor",
				Actual:   "gvisor",
				Message:  "probe confirmed gVisor markers",
				Probe:    &schema.ProbeResult{Pod: "web-abc", Detected: true, Markers: "gvisor"},
			},
			{
				Order:    2,
				Target:   schema.WorkloadRef{Kind: "StatefulSet", Namespace: "ns-b", Name: "cache"},
				Status:   schema.VerifyStatusMismatch,
				Expected: "gvisor",
				Actual:   "",
				Message:  "pod runtimeClassName empty",
			},
		},
	}
	return r
}

// TestRenderVerifyReportTable asserts the table renderer emits one row per
// step with the expected columns and that the per-row STATUS / EXPECTED /
// ACTUAL / PROBE values land in the right cells.
func TestRenderVerifyReportTable(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := Render(sampleVerifyReport(), FormatTable, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()

	wantStrings := []string{
		"agentmoat verify: 2 steps verified",
		"ok: 1",
		"mismatch: 1",
		"plan-hash: hash-1",
		"in-pod-probe: true",
		"STATUS",
		"KIND/NS/NAME",
		"EXPECTED",
		"ACTUAL",
		"PROBE",
		"MESSAGE",
		"Deployment/ns-a/web",
		"StatefulSet/ns-b/cache",
		"(empty)", // displayActual for the mismatch row
	}
	for _, want := range wantStrings {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}
}

// TestRenderVerifyReportJSONYAML asserts the generic encoders produce a
// document whose key fields are preserved. We don't pin byte layouts; the
// e2e relies on jq paths into .spec.summary.* and .spec.results[].* so just
// confirming the paths exist is enough.
func TestRenderVerifyReportJSONYAML(t *testing.T) {
	t.Parallel()
	r := sampleVerifyReport()

	t.Run("json", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Render(r, FormatJSON, &buf); err != nil {
			t.Fatalf("Render(JSON): %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal JSON: %v", err)
		}
		assertVerifyEnvelope(t, got)
	})

	t.Run("yaml", func(t *testing.T) {
		var buf bytes.Buffer
		if err := Render(r, FormatYAML, &buf); err != nil {
			t.Fatalf("Render(YAML): %v", err)
		}
		var got map[string]any
		if err := yaml.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal YAML: %v", err)
		}
		assertVerifyEnvelope(t, got)
	})
}

func assertVerifyEnvelope(t *testing.T, got map[string]any) {
	t.Helper()
	if got["kind"] != "VerifyReport" {
		t.Errorf("kind: got %v, want VerifyReport", got["kind"])
	}
	if got["apiVersion"] != schema.APIVersion {
		t.Errorf("apiVersion: got %v, want %s", got["apiVersion"], schema.APIVersion)
	}
	spec, ok := got["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing or wrong type")
	}
	summary, ok := spec["summary"].(map[string]any)
	if !ok {
		t.Fatalf("spec.summary missing or wrong type")
	}
	for _, k := range []string{"total", "ok", "mismatch", "error"} {
		if _, ok := summary[k]; !ok {
			t.Errorf("spec.summary missing key %q", k)
		}
	}
	results, ok := spec["results"].([]any)
	if !ok {
		t.Fatalf("spec.results missing or wrong type")
	}
	if len(results) != 2 {
		t.Errorf("spec.results length: got %d, want 2", len(results))
	}
}

// TestRenderExplainDocumentTable_TopicMode asserts the renderer prints the
// raw markdown content when Spec.Content is set. The e2e asserts the
// stdout begins with "# " so we cover that here too.
func TestRenderExplainDocumentTable_TopicMode(t *testing.T) {
	t.Parallel()
	d := schema.NewExplainDocument()
	d.Spec = schema.ExplainSpec{
		Topic:   "runtimeclass",
		Content: "# RuntimeClass 101\n\nbody.\n",
		Topics:  []string{"runtimeclass"},
	}
	var buf bytes.Buffer
	if err := Render(d, FormatTable, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if !strings.HasPrefix(out, "# RuntimeClass 101") {
		t.Errorf("output should start with '# ': got %q", out)
	}
	if !strings.Contains(out, "body.") {
		t.Errorf("output missing body: %q", out)
	}
}

// TestRenderExplainDocumentTable_ListMode asserts the renderer prints the
// available topic names when Spec.Content is empty.
func TestRenderExplainDocumentTable_ListMode(t *testing.T) {
	t.Parallel()
	d := schema.NewExplainDocument()
	d.Spec = schema.ExplainSpec{
		Topics: []string{"compatibility", "gvisor", "performance", "runtimeclass", "threat-model"},
	}
	var buf bytes.Buffer
	if err := Render(d, FormatTable, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	for _, topic := range d.Spec.Topics {
		if !strings.Contains(out, topic) {
			t.Errorf("list-mode output missing topic %q\noutput: %s", topic, out)
		}
	}
}

// TestRenderExplainDocumentJSON ensures the envelope round-trips through the
// generic JSON encoder.
func TestRenderExplainDocumentJSON(t *testing.T) {
	t.Parallel()
	d := schema.NewExplainDocument()
	d.Spec = schema.ExplainSpec{
		Topic:   "runtimeclass",
		Content: "# RuntimeClass 101\n",
		Topics:  []string{"runtimeclass"},
	}
	var buf bytes.Buffer
	if err := Render(d, FormatJSON, &buf); err != nil {
		t.Fatalf("Render(JSON): %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["kind"] != "ExplainDocument" {
		t.Errorf("kind: got %v", got["kind"])
	}
	spec, ok := got["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing")
	}
	if spec["topic"] != "runtimeclass" {
		t.Errorf("topic: got %v", spec["topic"])
	}
}

// TestRenderUnknownType asserts the dispatcher errors cleanly when given a
// type it does not know how to render. Pins behaviour the CLI relies on.
func TestRenderUnknownType(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	err := Render(struct{ Foo int }{Foo: 1}, FormatTable, &buf)
	if err == nil {
		t.Fatalf("expected error for unknown type")
	}
	if !strings.Contains(err.Error(), "no table renderer") {
		t.Errorf("error should mention unknown type: %v", err)
	}
}
