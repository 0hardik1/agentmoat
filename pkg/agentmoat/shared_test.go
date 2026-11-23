// Tests for the pieces that Scan, AssessWorkload, the deep Explain, and Plan
// share: the rule registry builder, the ScanReport loader, the report
// builder, and the client helper. Each of these used to exist as two or
// three inline copies; these tests pin the single copy.
package agentmoat

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// writeFile writes content to a new file in a per-test temp directory and
// returns its path.
func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestBuildRegistry(t *testing.T) {
	t.Parallel()

	t.Run("no override file gives the built-in rules", func(t *testing.T) {
		t.Parallel()
		registry, err := buildRegistry("", io.Discard)
		if err != nil {
			t.Fatalf("buildRegistry: %v", err)
		}
		if n := len(registry.Rules()); n == 0 {
			t.Fatalf("registry has no rules")
		}
	})

	t.Run("override changes a severity", func(t *testing.T) {
		t.Parallel()
		path := writeFile(t, "rules.yaml", "overrides:\n  - id: host-path-mount\n    severity: error\n")
		registry, err := buildRegistry(path, io.Discard)
		if err != nil {
			t.Fatalf("buildRegistry: %v", err)
		}
		for _, r := range registry.Rules() {
			if r.ID == "host-path-mount" && r.Severity != schema.SeverityError {
				t.Errorf("host-path-mount severity = %q, want error", r.Severity)
			}
		}
	})

	t.Run("unknown rule ID is a warning, not an error", func(t *testing.T) {
		t.Parallel()
		path := writeFile(t, "rules.yaml", "overrides:\n  - id: rule-from-the-future\n    severity: warn\n")
		var stderr bytes.Buffer
		if _, err := buildRegistry(path, &stderr); err != nil {
			t.Fatalf("buildRegistry: %v", err)
		}
		if !strings.Contains(stderr.String(), "rule-from-the-future") {
			t.Errorf("stderr = %q, want a warning that names the unknown rule", stderr.String())
		}
	})

	t.Run("missing file is an error that names the path", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "absent.yaml")
		_, err := buildRegistry(path, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "reading rules override "+path) {
			t.Fatalf("err = %v, want 'reading rules override <path>'", err)
		}
	})

	t.Run("malformed file is an error that names the path", func(t *testing.T) {
		t.Parallel()
		path := writeFile(t, "rules.yaml", "overrides: [this is : not valid\n")
		_, err := buildRegistry(path, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "parsing rules override "+path) {
			t.Fatalf("err = %v, want 'parsing rules override <path>'", err)
		}
	})
}

func TestLoadScanReport(t *testing.T) {
	t.Parallel()

	report := schema.NewScanReport()
	report.Spec.Workloads = []schema.WorkloadResult{{
		Kind: "Deployment", Namespace: "ns-a", Name: "web", Compatibility: schema.CompatibilityCompatible,
	}}
	report.Spec.Summary = schema.Summary{Total: 1, Compatible: 1}
	asYAML, err := yaml.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	asJSON, err := yaml.YAMLToJSON(asYAML)
	if err != nil {
		t.Fatalf("to json: %v", err)
	}
	planYAML, err := yaml.Marshal(schema.NewMigrationPlan())
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}

	tests := []struct {
		name    string
		content string
		wantErr string // substring; empty means success
	}{
		{name: "yaml", content: string(asYAML)},
		{name: "json", content: string(asJSON)},
		{name: "wrong kind names both kinds", content: string(planYAML), wantErr: `has kind "MigrationPlan", want "ScanReport"`},
		{name: "no kind at all", content: "spec: {}\n", wantErr: `has kind "", want "ScanReport"`},
		// Table output (`scan > scan.json` without -o json) is not YAML at
		// all, so today it fails in the parser. The message is correct but
		// not friendly.
		{name: "table output is not a report", content: "agentmoat scan\n\ncluster: kind   scanned: 3\n", wantErr: "parsing "},
		{name: "malformed", content: "kind: [unterminated\n", wantErr: "parsing "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeFile(t, "scan.out", tc.content)
			got, err := loadScanReport(path)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadScanReport: %v", err)
			}
			if len(got.Spec.Workloads) != 1 || got.Spec.Workloads[0].Name != "web" {
				t.Errorf("workloads = %+v", got.Spec.Workloads)
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := loadScanReport(filepath.Join(t.TempDir(), "absent.json"))
		if err == nil || !strings.Contains(err.Error(), "reading ") {
			t.Fatalf("err = %v, want a read error", err)
		}
	})
}

// TestPlan_ScanReportPath: Plan loads the file itself, so the CLI (--scan)
// and the MCP tool (scan_report_path) only pass a path. A plan from the
// file equals a plan from the same report in memory.
func TestPlan_ScanReportPath(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Workloads = []schema.WorkloadResult{
		{Kind: "Deployment", Namespace: "ns-a", Name: "web", Compatibility: schema.CompatibilityCompatible},
		{Kind: "Deployment", Namespace: "ns-a", Name: "priv", Compatibility: schema.CompatibilityIncompatible},
	}
	report.Spec.Summary = schema.Summary{Total: 2, Compatible: 1, Incompatible: 1}
	data, err := yaml.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := writeFile(t, "scan.yaml", string(data))

	fromPath, err := Plan(context.Background(), PlanOptions{ScanReportPath: path, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan from path: %v", err)
	}
	fromMemory, err := Plan(context.Background(), PlanOptions{ScanReport: report, Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan from memory: %v", err)
	}
	if fromPath.Metadata.PlanHash == "" || fromPath.Metadata.PlanHash != fromMemory.Metadata.PlanHash {
		t.Errorf("plan hash from path %q != from memory %q", fromPath.Metadata.PlanHash, fromMemory.Metadata.PlanHash)
	}
	if len(fromPath.Spec.Steps) != 1 || len(fromPath.Spec.Excluded) != 1 {
		t.Errorf("steps = %d, excluded = %d, want 1 and 1", len(fromPath.Spec.Steps), len(fromPath.Spec.Excluded))
	}

	// An in-memory report wins over the path: the path is not even read.
	if _, err := Plan(context.Background(), PlanOptions{
		ScanReport: report, ScanReportPath: filepath.Join(t.TempDir(), "absent.yaml"), Stderr: io.Discard,
	}); err != nil {
		t.Errorf("Plan with ScanReport and a bad path: %v (the path should be ignored)", err)
	}

	// A file of the wrong kind is refused, and the error says what was
	// expected. (No kubeconfig is involved: the failure comes first.)
	planData, err := yaml.Marshal(schema.NewMigrationPlan())
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	wrong := writeFile(t, "wrong.yaml", string(planData))
	_, err = Plan(context.Background(), PlanOptions{ScanReportPath: wrong, Stderr: io.Discard})
	if err == nil || !strings.Contains(err.Error(), "plan: loading scan report:") || !strings.Contains(err.Error(), "ScanReport") {
		t.Fatalf("err = %v, want 'plan: loading scan report: ... ScanReport'", err)
	}
}

// TestBuildReport: the shared half of a scan. Results come out sorted by
// (namespace, kind, name) whatever the input order, the summary counts the
// verdicts, and the provenance fields land in the metadata.
func TestBuildReport(t *testing.T) {
	t.Parallel()
	registry, err := buildRegistry("", io.Discard)
	if err != nil {
		t.Fatalf("buildRegistry: %v", err)
	}
	privileged := true
	plain := corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "example.test/app:1"}}}
	bad := corev1.PodSpec{Containers: []corev1.Container{{
		Name: "c", Image: "example.test/app:1",
		SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
	}}}

	// Deliberately out of order.
	in := scanInput{
		Workloads: []scanner.Workload{
			{Kind: "StatefulSet", Namespace: "b", Name: "db", PodSpec: plain},
			{Kind: "Deployment", Namespace: "b", Name: "zeta", PodSpec: bad},
			{Kind: "Deployment", Namespace: "a", Name: "web", PodSpec: plain},
			{Kind: "Deployment", Namespace: "b", Name: "alpha", PodSpec: plain},
		},
		Cluster:    "ctx-under-test",
		Namespaces: []string{"a", "b"},
	}
	report := buildReport(in, registry)

	var order []string
	for _, w := range report.Spec.Workloads {
		order = append(order, w.Namespace+"/"+w.Kind+"/"+w.Name)
	}
	want := "a/Deployment/web b/Deployment/alpha b/Deployment/zeta b/StatefulSet/db"
	if got := strings.Join(order, " "); got != want {
		t.Errorf("order = %q, want %q", got, want)
	}
	if s := report.Spec.Summary; s.Total != 4 || s.Compatible != 3 || s.Incompatible != 1 || s.NeedsReview != 0 {
		t.Errorf("summary = %+v, want 4 total, 3 compatible, 1 incompatible", s)
	}
	if report.Metadata.Cluster != "ctx-under-test" || len(report.Metadata.Namespaces) != 2 {
		t.Errorf("metadata provenance = %+v", report.Metadata)
	}
	if report.Metadata.ClusterFacts != nil {
		t.Errorf("ClusterFacts = %+v, want nil when the input has no facts", report.Metadata.ClusterFacts)
	}
	if report.Kind != schema.KindScanReport || report.APIVersion != schema.APIVersion {
		t.Errorf("envelope = %s/%s", report.APIVersion, report.Kind)
	}
}

// TestExplainNamespace_HonorsInjectedClient: the deep Explain used to build
// its own client and ignore ScanOptions.KubeClient, so it could not run
// against a fake clientset at all. It now takes the same path as Scan.
func TestExplainNamespace_HonorsInjectedClient(t *testing.T) {
	t.Parallel()
	ns := "agentmoat-test"
	client := fake.NewSimpleClientset(privilegedPod(ns, "naughty"))

	doc, err := Explain(context.Background(), ExplainOptions{
		Namespace: ns,
		Scan: ScanOptions{
			KubeClient: client,
			// A kubeconfig that does not exist: if Explain built its own
			// client, this would be the error.
			KubeconfigPath:   filepath.Join(t.TempDir(), "no-such-kubeconfig"),
			SkipClusterFacts: true,
			Stderr:           io.Discard,
		},
	})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if doc.Spec.Namespace == nil || len(doc.Spec.Namespace.Workloads) != 1 {
		t.Fatalf("namespace explanation = %+v", doc.Spec.Namespace)
	}
	if got := doc.Spec.Namespace.Workloads[0].Compatibility; got != schema.CompatibilityIncompatible {
		t.Errorf("compatibility = %q, want incompatible", got)
	}
}
