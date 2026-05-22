// Package schema tests focus on contract stability: kind/status string
// constants must not drift (downstream tools switch on them) and the JSON
// + YAML envelopes must round-trip without surprises. Phase 3 adds the
// VerifyReport and ExplainDocument round-trips; Phase 1/2 envelopes are
// implicitly covered by the existing pkg-level tests.
package schema

import (
	"encoding/json"
	"reflect"
	"testing"

	"sigs.k8s.io/yaml"
)

// verifyStatusConstants pins the wire strings for VerifyStatus. The renderer
// and the e2e jq assertions both grep for these; renaming them is a breaking
// change that bumps the APIVersion.
func TestVerifyStatusConstants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got, want string
	}{
		{string(VerifyStatusOK), "ok"},
		{string(VerifyStatusMismatch), "mismatch"},
		{string(VerifyStatusError), "error"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("constant drift: got %q, want %q", c.got, c.want)
		}
	}
}

// kindConstants pins the Kind strings the renderer dispatches on.
func TestKindConstants(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got, want string
	}{
		{KindScanReport, "ScanReport"},
		{KindMigrationPlan, "MigrationPlan"},
		{KindApplyResult, "ApplyResult"},
		{KindRollbackResult, "RollbackResult"},
		{KindVerifyReport, "VerifyReport"},
		{KindExplainDocument, "ExplainDocument"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("kind constant drift: got %q, want %q", c.got, c.want)
		}
	}
}

// TestVerifyReportRoundTrip marshals a populated VerifyReport to JSON and to
// YAML, unmarshals both, and asserts the result equals the input. Mirrors the
// envelope guarantee documented at the top of types.go: every field carries
// matching json and yaml tags so the two encodings are byte-equivalent in
// content (modulo whitespace).
func TestVerifyReportRoundTrip(t *testing.T) {
	t.Parallel()
	r := NewVerifyReport()
	r.Metadata = VerifyMetadata{
		GeneratedAt:      "2026-05-22T12:00:00Z",
		Cluster:          "kind-agentmoat-e2e",
		AgentmoatVersion: "test",
		PlanHash:         "abc123",
		InPodProbe:       true,
	}
	r.Spec = VerifySpec{
		Summary: VerifySummary{Total: 2, OK: 1, Mismatch: 1, Error: 0},
		Results: []VerifyResult{
			{
				Order:    1,
				Target:   WorkloadRef{Kind: "Deployment", Namespace: "ns-a", Name: "web"},
				Status:   VerifyStatusOK,
				Expected: "gvisor",
				Actual:   "gvisor",
				Message:  "probe confirmed gVisor markers",
				Probe: &ProbeResult{
					Pod:      "web-abc",
					Detected: true,
					Markers:  "gvisor",
				},
			},
			{
				Order:    2,
				Target:   WorkloadRef{Kind: "StatefulSet", Namespace: "ns-b", Name: "cache"},
				Status:   VerifyStatusMismatch,
				Expected: "gvisor",
				Actual:   "",
				Message:  "pod runtimeClassName empty",
			},
		},
	}

	// JSON round-trip.
	jsonBytes, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var fromJSON VerifyReport
	if err := json.Unmarshal(jsonBytes, &fromJSON); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*r, fromJSON) {
		t.Errorf("json round-trip mismatch:\nwant: %#v\ngot:  %#v", *r, fromJSON)
	}

	// YAML round-trip.
	yamlBytes, err := yaml.Marshal(r)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var fromYAML VerifyReport
	if err := yaml.Unmarshal(yamlBytes, &fromYAML); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*r, fromYAML) {
		t.Errorf("yaml round-trip mismatch:\nwant: %#v\ngot:  %#v", *r, fromYAML)
	}
}

// TestExplainDocumentRoundTrip mirrors the VerifyReport test for the explain
// envelope. Covers both list mode (Topic empty, Topics populated) and topic
// mode (Topic + Content set).
func TestExplainDocumentRoundTrip(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		d    ExplainDocument
	}{
		{
			name: "list_mode",
			d: ExplainDocument{
				APIVersion: APIVersion,
				Kind:       KindExplainDocument,
				Metadata: ExplainMetadata{
					GeneratedAt:      "2026-05-22T12:00:00Z",
					AgentmoatVersion: "test",
				},
				Spec: ExplainSpec{
					Topics: []string{"compatibility", "gvisor", "performance", "runtimeclass", "threat-model"},
				},
			},
		},
		{
			name: "topic_mode",
			d: ExplainDocument{
				APIVersion: APIVersion,
				Kind:       KindExplainDocument,
				Metadata: ExplainMetadata{
					GeneratedAt:      "2026-05-22T12:00:00Z",
					AgentmoatVersion: "test",
				},
				Spec: ExplainSpec{
					Topic:   "runtimeclass",
					Content: "# RuntimeClass 101\n\nshort body.\n",
					Topics:  []string{"compatibility", "gvisor", "performance", "runtimeclass", "threat-model"},
				},
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			jsonBytes, err := json.Marshal(tc.d)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var fromJSON ExplainDocument
			if err := json.Unmarshal(jsonBytes, &fromJSON); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.d, fromJSON) {
				t.Errorf("json round-trip mismatch:\nwant: %#v\ngot:  %#v", tc.d, fromJSON)
			}

			yamlBytes, err := yaml.Marshal(tc.d)
			if err != nil {
				t.Fatalf("yaml.Marshal: %v", err)
			}
			var fromYAML ExplainDocument
			if err := yaml.Unmarshal(yamlBytes, &fromYAML); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tc.d, fromYAML) {
				t.Errorf("yaml round-trip mismatch:\nwant: %#v\ngot:  %#v", tc.d, fromYAML)
			}
		})
	}
}

// TestNewVerifyReportEnvelope asserts the constructor pre-fills the envelope.
// Trivial but pins behaviour the orchestrator depends on.
func TestNewVerifyReportEnvelope(t *testing.T) {
	t.Parallel()
	r := NewVerifyReport()
	if r.APIVersion != APIVersion {
		t.Errorf("APIVersion: got %q, want %q", r.APIVersion, APIVersion)
	}
	if r.Kind != KindVerifyReport {
		t.Errorf("Kind: got %q, want %q", r.Kind, KindVerifyReport)
	}
}

// TestNewExplainDocumentEnvelope mirrors TestNewVerifyReportEnvelope.
func TestNewExplainDocumentEnvelope(t *testing.T) {
	t.Parallel()
	d := NewExplainDocument()
	if d.APIVersion != APIVersion {
		t.Errorf("APIVersion: got %q, want %q", d.APIVersion, APIVersion)
	}
	if d.Kind != KindExplainDocument {
		t.Errorf("Kind: got %q, want %q", d.Kind, KindExplainDocument)
	}
}
