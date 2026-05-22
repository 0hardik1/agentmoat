// Tests for the Verify orchestrator. The deep cases (per-step status
// matrix, in-pod-probe seam) live in pkg/verifier/verifier_test.go;
// these tests verify the orchestrator's plan-load and envelope-stamp
// responsibilities.
package agentmoat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"sigs.k8s.io/yaml"
)

// writeTestPlan writes a minimal MigrationPlan to a temp file and returns
// the path. Used by the load-from-disk tests.
func writeTestPlan(t *testing.T) string {
	t.Helper()
	plan := schema.NewMigrationPlan()
	plan.Metadata = schema.PlanMetadata{
		GeneratedAt: "2026-05-22T12:00:00Z",
		PlanHash:    "test-hash",
	}
	plan.Spec = schema.PlanSpec{
		Summary: schema.PlanSummary{Total: 1, Included: 1, Excluded: 0},
		Options: schema.PlannerOptions{RuntimeClassName: "gvisor"},
		Steps: []schema.PlanStep{
			{
				Order:            1,
				Target:           schema.WorkloadRef{Kind: "Deployment", Namespace: "ns-a", Name: "web"},
				Action:           "set-runtime-class",
				RuntimeClassName: "gvisor",
			},
		},
		Excluded: []schema.ExcludedWorkload{},
	}
	data, err := yaml.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// TestVerifyRejectsMissingPlan asserts the orchestrator surfaces a clean
// error when --plan is empty. No K8s call should be attempted.
func TestVerifyRejectsMissingPlan(t *testing.T) {
	t.Parallel()
	_, err := Verify(context.Background(), VerifyOptions{})
	if err == nil {
		t.Fatalf("expected error when PlanPath is empty")
	}
	if !strings.Contains(err.Error(), "--plan") {
		t.Errorf("error should mention --plan: %v", err)
	}
}

// TestVerifyRejectsWrongKindFile asserts the orchestrator rejects a file
// whose Kind is not MigrationPlan (e.g. someone hands us a ScanReport).
func TestVerifyRejectsWrongKindFile(t *testing.T) {
	t.Parallel()
	// Build a ScanReport on disk; the loader sniffs Kind and bails.
	report := schema.NewScanReport()
	data, err := yaml.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := filepath.Join(t.TempDir(), "wrong.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = Verify(context.Background(), VerifyOptions{PlanPath: path})
	if err == nil {
		t.Fatalf("expected error when given ScanReport instead of MigrationPlan")
	}
	if !strings.Contains(err.Error(), "MigrationPlan") {
		t.Errorf("error should mention expected Kind: %v", err)
	}
}

// TestVerifyLoadsPlanAndFailsKubeconfig is the integration-light check: the
// loader parses the plan, then the orchestrator tries to build a K8s client
// against an unreachable kubeconfig and errors out. Confirms the plan-load
// path runs before the K8s-client path.
func TestVerifyLoadsPlanAndFailsKubeconfig(t *testing.T) {
	t.Parallel()
	planPath := writeTestPlan(t)
	_, err := Verify(context.Background(), VerifyOptions{
		PlanPath:       planPath,
		KubeconfigPath: "/dev/null",
	})
	if err == nil {
		t.Fatalf("expected error against /dev/null kubeconfig")
	}
	if !strings.Contains(err.Error(), "verify") {
		t.Errorf("error should be wrapped with 'verify': %v", err)
	}
}
