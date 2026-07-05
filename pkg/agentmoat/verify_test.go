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
	"github.com/0hardik1/agentmoat/pkg/planner"
	"sigs.k8s.io/yaml"
)

// writeTestPlan writes a minimal MigrationPlan to a temp file and returns
// the path. Used by the load-from-disk tests.
func writeTestPlan(t *testing.T) string {
	t.Helper()
	plan := schema.NewMigrationPlan()
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
	// The loader verifies metadata.planHash against the steps/options, so
	// the fixture carries the real content hash.
	hash, err := planner.ComputePlanHash(plan.Spec.Steps, plan.Spec.Options)
	if err != nil {
		t.Fatalf("compute plan hash: %v", err)
	}
	plan.Metadata = schema.PlanMetadata{
		GeneratedAt: "2026-05-22T12:00:00Z",
		PlanHash:    hash,
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

// TestLoadPlanRejectsTamperedHash asserts the loader's integrity check:
// a plan whose metadata.planHash does not match its steps/options is
// rejected before any Kubernetes call.
func TestLoadPlanRejectsTamperedHash(t *testing.T) {
	t.Parallel()
	planPath := writeTestPlan(t)
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	// Tamper with the content but keep the original hash: swap the
	// runtime class the steps set.
	tampered := strings.ReplaceAll(string(data), "runtimeClassName: gvisor", "runtimeClassName: kata")
	tamperedPath := filepath.Join(t.TempDir(), "tampered.yaml")
	if err := os.WriteFile(tamperedPath, []byte(tampered), 0o600); err != nil {
		t.Fatalf("write tampered plan: %v", err)
	}

	_, err = loadPlan(tamperedPath)
	if err == nil {
		t.Fatalf("expected integrity-check error for tampered plan")
	}
	if !strings.Contains(err.Error(), "integrity") {
		t.Errorf("error should mention the integrity check: %v", err)
	}

	// A plan with no hash at all is allowed through: the applier never
	// short-circuits on an empty hash.
	noHash := strings.ReplaceAll(string(data), "planHash:", "somethingElse:")
	noHashPath := filepath.Join(t.TempDir(), "nohash.yaml")
	if err := os.WriteFile(noHashPath, []byte(noHash), 0o600); err != nil {
		t.Fatalf("write no-hash plan: %v", err)
	}
	if _, err := loadPlan(noHashPath); err != nil {
		t.Errorf("plan without a planHash should load, got: %v", err)
	}
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
