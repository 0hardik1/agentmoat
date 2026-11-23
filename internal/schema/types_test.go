// Package schema tests focus on contract stability: kind/status string
// constants must not drift (downstream tools switch on them) and the JSON
// + YAML envelopes must round-trip without surprises. Phase 3 adds the
// VerifyReport and ExplainDocument round-trips; Phase 1/2 envelopes are
// implicitly covered by the existing pkg-level tests.
package schema

import (
	"encoding/json"
	"reflect"
	"strings"
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
		Wait:             &VerifyWait{RequestedSeconds: 180, WaitedSeconds: 180, Rounds: 37, TimedOut: true},
	}
	r.Spec = VerifySpec{
		Summary: VerifySummary{Total: 4, OK: 2, Mismatch: 2, Error: 0},
		Results: []VerifyResult{
			{
				Order:    3,
				Target:   WorkloadRef{Kind: "CronJob", Namespace: "ns-a", Name: "report"},
				Status:   VerifyStatusOK,
				Expected: "gvisor",
				Actual:   "gvisor",
				Basis:    VerifyBasisTemplate,
			},
			{
				Order:    4,
				Target:   WorkloadRef{Kind: "StatefulSet", Namespace: "ns-a", Name: "db"},
				Status:   VerifyStatusMismatch,
				Expected: "gvisor",
				Actual:   "gvisor",
				Message:  "pod db-0 is not Ready: container app is waiting: CrashLoopBackOff, 4 restart(s)",
				Readiness: &PodReadiness{
					Pods: 2, Ready: 1, NotReady: []string{"db-0"},
					Message: "pod db-0 is not Ready: container app is waiting: CrashLoopBackOff, 4 restart(s)",
				},
			},
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
// Trivial but pins behavior the orchestrator depends on.
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

// TestNamespaceExplanationRoundTrip exercises the deep namespace-explain
// payload that hangs off ExplainSpec.Namespace. It populates every Evidence
// variant at least once so renaming a field would surface as a round-trip
// mismatch instead of silently dropping data on the wire.
func TestNamespaceExplanationRoundTrip(t *testing.T) {
	t.Parallel()
	d := ExplainDocument{
		APIVersion: APIVersion,
		Kind:       KindExplainDocument,
		Metadata: ExplainMetadata{
			GeneratedAt:      "2026-05-22T12:00:00Z",
			AgentmoatVersion: "test",
		},
		Spec: ExplainSpec{
			Namespace: &NamespaceExplanation{
				Name: "ns-deep",
				Summary: Summary{
					Total:        3,
					Compatible:   1,
					NeedsReview:  1,
					Incompatible: 1,
				},
				Workloads: []WorkloadExplanation{
					{
						Kind:           "Deployment",
						Namespace:      "ns-deep",
						Name:           "good",
						Compatibility:  CompatibilityCompatible,
						Recommendation: "set runtimeClassName: gvisor",
						Overhead:       "CPU-bound: <5%",
						Checked: []RuleCheck{
							{RuleID: "host-network", Outcome: "did-not-fire"},
							{RuleID: "raw-socket", Outcome: "did-not-fire"},
						},
					},
					{
						Kind:           "DaemonSet",
						Namespace:      "ns-deep",
						Name:           "bad",
						Compatibility:  CompatibilityIncompatible,
						Recommendation: "do not migrate",
						Overhead:       "Network throughput: 20-40%",
						Findings: []RuleFinding{
							{
								RuleID:      "host-network",
								Severity:    SeverityError,
								Title:       "uses host network",
								WhyMarkdown: "Pod requests `hostNetwork: true`, which gVisor does not provide.",
								Evidence: Evidence{
									HostNamespaces: []string{"network", "pid"},
									Capabilities: []CapabilityHit{
										{Container: "main", Capability: "NET_ADMIN"},
									},
									HostPaths: []HostPathHit{
										{Volume: "host-root", Path: "/", Containers: []string{"main"}},
									},
									ImageMatches: []ImageMatch{
										{Container: "main", Image: "kube-proxy:latest", HintPattern: "kube-proxy"},
									},
									GPURequests: []GPURequest{
										{Container: "main", Resource: "nvidia.com/gpu", Quantity: "1"},
									},
									EnvVars: []EnvVarHit{
										{Container: "main", Name: "ENABLE_FUSE", Value: "1"},
									},
									Annotations: []AnnotationHit{
										{Key: "io.kubernetes.cri.untrusted-workload", Value: "true"},
									},
									PrivilegedContainers: []string{"main"},
									CSIDrivers: []CSIDriverHit{
										{Volume: "fuse-vol", Driver: "fuse.csi.example.com"},
									},
								},
								RemediationURL: "https://gvisor.dev/docs/user_guide/networking/",
							},
						},
					},
					{
						Kind:          "Pod",
						Namespace:     "ns-deep",
						Name:          "meh",
						Compatibility: CompatibilityReview,
					},
				},
			},
		},
	}

	jsonBytes, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var fromJSON ExplainDocument
	if err := json.Unmarshal(jsonBytes, &fromJSON); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(d, fromJSON) {
		t.Errorf("json round-trip mismatch:\nwant: %#v\ngot:  %#v", d, fromJSON)
	}

	yamlBytes, err := yaml.Marshal(d)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var fromYAML ExplainDocument
	if err := yaml.Unmarshal(yamlBytes, &fromYAML); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(d, fromYAML) {
		t.Errorf("yaml round-trip mismatch:\nwant: %#v\ngot:  %#v", d, fromYAML)
	}
}

// TestExplainSpecNamespaceOmitempty asserts that a nil Spec.Namespace does
// not appear in JSON or YAML output. Important because the renderer needs to
// distinguish static-topic mode from deep mode by key presence.
func TestExplainSpecNamespaceOmitempty(t *testing.T) {
	t.Parallel()
	d := ExplainDocument{
		APIVersion: APIVersion,
		Kind:       KindExplainDocument,
		Metadata: ExplainMetadata{
			GeneratedAt:      "2026-05-22T12:00:00Z",
			AgentmoatVersion: "test",
		},
		Spec: ExplainSpec{
			Topic:   "runtimeclass",
			Content: "# RuntimeClass 101\n",
			Topics:  []string{"runtimeclass"},
		},
	}

	jsonBytes, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(jsonBytes), `"namespace"`) {
		t.Errorf("expected json output to omit namespace key, got: %s", string(jsonBytes))
	}

	yamlBytes, err := yaml.Marshal(d)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	if strings.Contains(string(yamlBytes), "namespace:") {
		t.Errorf("expected yaml output to omit namespace key, got: %s", string(yamlBytes))
	}
}

// TestEvidenceOmitempty asserts that an Evidence value with only HostPaths
// populated marshals without naming any of the other variant fields. Pins the
// omitempty path so adding a future Evidence variant cannot silently leak an
// empty array into every report.
func TestEvidenceOmitempty(t *testing.T) {
	t.Parallel()
	ev := Evidence{
		HostPaths: []HostPathHit{
			{Volume: "host-root", Path: "/", Containers: []string{"main"}},
		},
	}

	jsonBytes, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	js := string(jsonBytes)
	for _, key := range []string{
		"hostNamespaces",
		"capabilities",
		"imageMatches",
		"gpuRequests",
		"envVars",
		"annotations",
		"privilegedContainers",
		"csiDrivers",
	} {
		if strings.Contains(js, key) {
			t.Errorf("expected json output to omit %q, got: %s", key, js)
		}
	}
	if !strings.Contains(js, "hostPaths") {
		t.Errorf("expected json output to include hostPaths, got: %s", js)
	}

	yamlBytes, err := yaml.Marshal(ev)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	ys := string(yamlBytes)
	for _, key := range []string{
		"hostNamespaces:",
		"capabilities:",
		"imageMatches:",
		"gpuRequests:",
		"envVars:",
		"annotations:",
		"privilegedContainers:",
		"csiDrivers:",
	} {
		if strings.Contains(ys, key) {
			t.Errorf("expected yaml output to omit %q, got: %s", key, ys)
		}
	}
	if !strings.Contains(ys, "hostPaths:") {
		t.Errorf("expected yaml output to include hostPaths, got: %s", ys)
	}
}

// TestScanReportManifestSourceRoundTrip covers the fields a scan of manifest
// files adds to the ScanReport: the source block, the per-workload runtime
// class and origin, and the sandboxed count. JSON and YAML must both carry
// them.
func TestScanReportManifestSourceRoundTrip(t *testing.T) {
	t.Parallel()
	r := NewScanReport()
	r.Metadata = ReportMetadata{
		GeneratedAt:      "2026-11-22T12:00:00Z",
		AgentmoatVersion: "test",
		RuntimeClassName: "gvisor",
		Source: &ReportSource{
			Type:       SourceManifests,
			Inputs:     3,
			Skipped:    map[string]int{"Service": 2, "Ingress.networking.k8s.io": 1},
			Duplicates: []string{"Deployment shop/web is defined 2 times (a.yaml:1, b.yaml:1)"},
			Unrendered: []string{"kustomization file: k8s/kustomization.yaml"},
		},
	}
	r.Spec = ReportSpec{
		Summary: Summary{Total: 2, Compatible: 1, Incompatible: 1, Sandboxed: 1},
		Workloads: []WorkloadResult{
			{
				Kind: "Deployment", Namespace: "shop", Name: "web",
				Compatibility:    CompatibilityCompatible,
				RuntimeClassName: "gvisor",
				Origin:           &Origin{File: "k8s/web.yaml", Line: 12},
			},
			{
				Kind: "Rollout.argoproj.io", Namespace: "", Name: "canary",
				Compatibility: CompatibilityIncompatible,
				Origin:        &Origin{File: "chart/templates/rollout.yaml"},
			},
		},
	}

	jsonBytes, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var fromJSON ScanReport
	if err := json.Unmarshal(jsonBytes, &fromJSON); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*r, fromJSON) {
		t.Errorf("json round-trip mismatch:\nwant: %#v\ngot:  %#v", *r, fromJSON)
	}

	yamlBytes, err := yaml.Marshal(r)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var fromYAML ScanReport
	if err := yaml.Unmarshal(yamlBytes, &fromYAML); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(*r, fromYAML) {
		t.Errorf("yaml round-trip mismatch:\nwant: %#v\ngot:  %#v", *r, fromYAML)
	}

	// The wire names are a public surface. Pin them.
	for _, key := range []string{
		`"runtimeClassName":"gvisor"`, `"source":{"type":"manifests"`, `"inputs":3`, `"skipped":{`,
		`"duplicates":[`, `"unrendered":[`, `"sandboxed":1`, `"origin":{"file":"k8s/web.yaml","line":12}`,
		// A Helm source path has no line.
		`"origin":{"file":"chart/templates/rollout.yaml"}`,
	} {
		if !strings.Contains(string(jsonBytes), key) {
			t.Errorf("JSON lacks %s:\n%s", key, jsonBytes)
		}
	}
}

// TestScanReportClusterSourceOmitsManifestFields: a cluster scan must not
// grow a `source` or per-workload `origin` key. `sandboxed` is always there,
// like the other counts.
func TestScanReportClusterSourceOmitsManifestFields(t *testing.T) {
	t.Parallel()
	r := NewScanReport()
	r.Metadata = ReportMetadata{GeneratedAt: "2026-11-22T12:00:00Z", Cluster: "kind-x", AgentmoatVersion: "test"}
	r.Spec = ReportSpec{
		Summary:   Summary{Total: 1, Compatible: 1},
		Workloads: []WorkloadResult{{Kind: "Deployment", Namespace: "ns", Name: "web", Compatibility: CompatibilityCompatible}},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, key := range []string{`"source"`, `"origin"`, `"runtimeClassName"`} {
		if strings.Contains(string(data), key) {
			t.Errorf("a cluster report with none of these set has the key %s:\n%s", key, data)
		}
	}
	if !strings.Contains(string(data), `"sandboxed":0`) {
		t.Errorf("JSON lacks \"sandboxed\":0:\n%s", data)
	}
}

// TestPlanMetadataSource: a plan carries the source type of its scan, and a
// plan from a cluster scan has no `source` key.
func TestPlanMetadataSource(t *testing.T) {
	t.Parallel()
	p := NewMigrationPlan()
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(data), `"source"`) {
		t.Errorf("a plan with no source has the key:\n%s", data)
	}

	p.Metadata.Source = &ReportSource{Type: SourceManifests}
	data, err = json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var back MigrationPlan
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back.Metadata.Source == nil || back.Metadata.Source.Type != SourceManifests {
		t.Errorf("source did not survive the round trip: %s", data)
	}
	if !strings.Contains(string(data), `"source":{"type":"manifests"}`) {
		t.Errorf("JSON lacks the source block:\n%s", data)
	}
}

// TestVerifyWaitFields pins the wire names of the fields that `verify
// --wait` adds, and checks that a verify with no wait has none of them: a
// reader of the report must be able to tell "one look" from "a wait" by the
// key alone.
func TestVerifyWaitFields(t *testing.T) {
	t.Parallel()
	plain := NewVerifyReport()
	plain.Spec.Results = []VerifyResult{{Order: 1, Status: VerifyStatusOK, Expected: "gvisor", Actual: "gvisor"}}
	data, err := json.Marshal(plain)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, key := range []string{`"wait"`, `"basis"`, `"readiness"`} {
		if strings.Contains(string(data), key) {
			t.Errorf("a verify with no wait has the key %s:\n%s", key, data)
		}
	}

	waited := NewVerifyReport()
	waited.Metadata.Wait = &VerifyWait{RequestedSeconds: 60, WaitedSeconds: 20, Rounds: 5}
	waited.Spec.Results = []VerifyResult{
		{Order: 1, Status: VerifyStatusOK, Basis: VerifyBasisTemplate},
		{Order: 2, Status: VerifyStatusOK, Readiness: &PodReadiness{Pods: 2, Ready: 2, Stable: true}},
	}
	data, err = json.Marshal(waited)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, want := range []string{
		// timedOut is always there in a wait block: false is an answer.
		// interrupted is there only when it is true.
		`"wait":{"requestedSeconds":60,"waitedSeconds":20,"rounds":5,"timedOut":false}`,
		`"basis":"template"`,
		`"readiness":{"pods":2,"ready":2,"stable":true}`,
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("JSON lacks %s:\n%s", want, data)
		}
	}
}

// TestWorkloadResultManagedBy pins the wire shape of the deploy tool marker
// on a scan row, the three tool names, and its absence when there is no
// marker.
func TestWorkloadResultManagedBy(t *testing.T) {
	t.Parallel()
	for got, want := range map[string]string{ManagedByArgoCD: "argocd", ManagedByFlux: "flux", ManagedByHelm: "helm"} {
		if got != want {
			t.Errorf("tool name %q, want %q", got, want)
		}
	}
	row := WorkloadResult{Kind: "Deployment", Namespace: "ns", Name: "web", Compatibility: CompatibilityCompatible}
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(data), "managedBy") {
		t.Errorf("a row with no marker has the key managedBy:\n%s", data)
	}

	row.ManagedBy = &ManagedBy{Tool: ManagedByArgoCD, Evidence: "annotation argocd.argoproj.io/tracking-id"}
	data, err = json.Marshal(row)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if want := `"managedBy":{"tool":"argocd","evidence":"annotation argocd.argoproj.io/tracking-id"}`; !strings.Contains(string(data), want) {
		t.Errorf("JSON lacks %s:\n%s", want, data)
	}
	yamlData, err := yaml.Marshal(row)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var back WorkloadResult
	if err := yaml.Unmarshal(yamlData, &back); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if back.ManagedBy == nil || *back.ManagedBy != *row.ManagedBy {
		t.Errorf("managedBy did not survive the YAML round trip: %s", yamlData)
	}
}
