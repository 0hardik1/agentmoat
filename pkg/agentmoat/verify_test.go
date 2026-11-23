// Tests for the Verify orchestrator. The deep cases (per-step status
// matrix, in-pod-probe seam) live in pkg/verifier/verifier_test.go;
// these tests verify the orchestrator's plan-load and envelope-stamp
// responsibilities.
package agentmoat

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/planner"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
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
	// #nosec G304 -- planPath is a t.TempDir() join; not attacker-controlled.
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	// Tamper with the content but keep the original hash: swap the
	// runtime class the steps set.
	tampered := strings.ReplaceAll(string(data), "runtimeClassName: gvisor", "runtimeClassName: kata")
	tamperedPath := filepath.Join(t.TempDir(), "tampered.yaml")
	// #nosec G703 -- tamperedPath is a t.TempDir() join; not attacker-controlled.
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
	// #nosec G703 -- noHashPath is a t.TempDir() join; not attacker-controlled.
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

// jumpClock is a verifier.Clock that moves only when the wait loop sleeps,
// so a wait of minutes runs in no time.
type jumpClock struct{ now time.Time }

func (c *jumpClock) Now() time.Time { return c.now }

func (c *jumpClock) Sleep(_ context.Context, d time.Duration) { c.now = c.now.Add(d) }

// verifyCluster is a fake cluster for the plan of writeTestPlan: the
// Deployment ns-a/web and one pod with the class. ready sets the condition
// Ready on the pod.
func verifyCluster(ready bool) *fake.Clientset {
	labels := map[string]string{"app": "web"}
	class := "gvisor"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "web-1", Labels: labels},
		Spec:       corev1.PodSpec{RuntimeClassName: &class},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "web"},
			Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}},
		},
		pod,
	)
}

// TestVerify_Wait: the orchestrator gives the wait time to the verifier,
// and the report and the stderr summary say how the wait ended.
func TestVerify_Wait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		ready      bool
		wantOK     int
		wantWait   schema.VerifyWait
		wantStderr string
	}{
		{
			name: "pods that are Ready", ready: true, wantOK: 1,
			wantWait:   schema.VerifyWait{RequestedSeconds: 30, WaitedSeconds: 10, Rounds: 3},
			wantStderr: "verify: 1 ok, 0 mismatch, 0 error (in-pod-probe=false, waited 10s of 30s)",
		},
		{
			name: "a pod that is never Ready", ready: false, wantOK: 0,
			wantWait:   schema.VerifyWait{RequestedSeconds: 30, WaitedSeconds: 30, Rounds: 7, TimedOut: true},
			wantStderr: "verify: 0 ok, 1 mismatch, 0 error (in-pod-probe=false, wait of 30s timed out)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stderr := &bytes.Buffer{}
			report, err := Verify(context.Background(), VerifyOptions{
				PlanPath:   writeTestPlan(t),
				KubeClient: verifyCluster(tc.ready),
				Wait:       30 * time.Second,
				Clock:      &jumpClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)},
				Stderr:     stderr,
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if report.Spec.Summary.OK != tc.wantOK {
				t.Errorf("summary = %+v, want %d ok", report.Spec.Summary, tc.wantOK)
			}
			if report.Metadata.Wait == nil || *report.Metadata.Wait != tc.wantWait {
				t.Errorf("metadata.wait = %+v, want %+v", report.Metadata.Wait, tc.wantWait)
			}
			if !strings.Contains(stderr.String(), tc.wantStderr) {
				t.Errorf("stderr lacks %q:\n%s", tc.wantStderr, stderr.String())
			}
		})
	}
}

// TestVerify_NoWaitIsOneLook: with no wait time the report has no wait
// block, and a pod that is not Ready is not a failure. This is the behavior
// from before the wait option.
func TestVerify_NoWaitIsOneLook(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	report, err := Verify(context.Background(), VerifyOptions{
		PlanPath: writeTestPlan(t), KubeClient: verifyCluster(false), Stderr: stderr,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Metadata.Wait != nil || report.Spec.Summary.OK != 1 || report.Spec.Results[0].Readiness != nil {
		t.Errorf("wait = %+v, summary = %+v, readiness = %+v; want no wait block, 1 ok, no readiness",
			report.Metadata.Wait, report.Spec.Summary, report.Spec.Results[0].Readiness)
	}
	if want := "verify: 1 ok, 0 mismatch, 0 error (in-pod-probe=false)\n"; !strings.Contains(stderr.String(), want) {
		t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
	}
}

// TestVerify_NegativeWaitIsRefusedFirst: before the plan file is read.
func TestVerify_NegativeWaitIsRefusedFirst(t *testing.T) {
	t.Parallel()
	_, err := Verify(context.Background(), VerifyOptions{Wait: -time.Second})
	if err == nil || !strings.Contains(err.Error(), "the wait time is -1s") {
		t.Errorf("err = %v, want an error about the wait time", err)
	}
}
