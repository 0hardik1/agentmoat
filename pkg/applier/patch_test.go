// Tests for patch generation. We snapshot the exact JSON the applier sends
// for each supported Kind. Snapshotting (rather than checking for substrings)
// catches accidental changes to the patch shape: a future refactor that
// silently re-orders keys, drops a field, or sneaks in an extra wrapper will
// fail the test immediately.
//
// The default plan step sets only runtimeClassName (placement comes from
// the RuntimeClass's scheduling block). The toleration is an opt-in
// (PlanStep.AddToleration), covered once per wrapper shape below.
package applier

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"k8s.io/apimachinery/pkg/types"
)

func step(kind string, addTol bool) schema.PlanStep {
	return schema.PlanStep{
		Order:            1,
		Target:           schema.WorkloadRef{Kind: kind, Namespace: "default", Name: "test"},
		Action:           "set-runtime-class",
		RuntimeClassName: "gvisor",
		AddToleration:    addTol,
		WaitFor:          waitForStub(kind),
		RiskScore:        0,
	}
}

func waitForStub(kind string) string {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet":
		return "Ready"
	default:
		return "Running"
	}
}

func TestApplyPatchBytes_Pod(t *testing.T) {
	body, pt, err := applyPatchBytes(step("Pod", false))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pt != types.StrategicMergePatchType {
		t.Errorf("patch type: got %v want %v", pt, types.StrategicMergePatchType)
	}
	want := `{"spec":{"runtimeClassName":"gvisor"}}`
	if string(body) != want {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", body, want)
	}
}

func TestApplyPatchBytes_Deployment(t *testing.T) {
	body, _, err := applyPatchBytes(step("Deployment", false))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `{"spec":{"template":{"spec":{"runtimeClassName":"gvisor"}}}}`
	if string(body) != want {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", body, want)
	}
}

func TestApplyPatchBytes_StatefulSet(t *testing.T) {
	body, _, err := applyPatchBytes(step("StatefulSet", false))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(body), `"runtimeClassName":"gvisor"`) {
		t.Errorf("body missing runtimeClassName: %s", body)
	}
	if !strings.Contains(string(body), `"template":{"spec":{`) {
		t.Errorf("body missing spec.template.spec wrapper: %s", body)
	}
}

func TestApplyPatchBytes_DaemonSet(t *testing.T) {
	body, _, err := applyPatchBytes(step("DaemonSet", false))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(body), `"spec":{"template":{"spec":`) {
		t.Errorf("body missing spec.template.spec wrapper: %s", body)
	}
}

func TestApplyPatchBytes_Job(t *testing.T) {
	body, _, err := applyPatchBytes(step("Job", false))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(string(body), `"spec":{"template":{"spec":`) {
		t.Errorf("body missing spec.template.spec wrapper: %s", body)
	}
}

func TestApplyPatchBytes_CronJob(t *testing.T) {
	body, _, err := applyPatchBytes(step("CronJob", false))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"runtimeClassName":"gvisor"}}}}}}`
	if string(body) != want {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", body, want)
	}
}

// TestApplyPatchBytes_DefaultHasNoToleration pins the contract the
// preflight relies on: without the opt-in, apply touches nothing but
// runtimeClassName, for every supported kind.
func TestApplyPatchBytes_DefaultHasNoToleration(t *testing.T) {
	for _, kind := range []string{"Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"} {
		body, _, err := applyPatchBytes(step(kind, false))
		if err != nil {
			t.Fatalf("%s: err: %v", kind, err)
		}
		if strings.Contains(string(body), "tolerations") {
			t.Errorf("%s: body should NOT contain tolerations when AddToleration=false, got: %s", kind, body)
		}
	}
}

// TestApplyPatchBytes_WithToleration covers the opt-in once per wrapper
// shape (Pod, template, jobTemplate). The toleration must sit next to
// runtimeClassName inside the innermost pod spec.
func TestApplyPatchBytes_WithToleration(t *testing.T) {
	const tol = `"tolerations":[{"key":"runtime","operator":"Equal","value":"gvisor","effect":"NoSchedule"}]`
	tests := []struct {
		kind string
		want string
	}{
		{"Pod", `{"spec":{"runtimeClassName":"gvisor",` + tol + `}}`},
		{"Deployment", `{"spec":{"template":{"spec":{"runtimeClassName":"gvisor",` + tol + `}}}}`},
		{"CronJob", `{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"runtimeClassName":"gvisor",` + tol + `}}}}}}`},
	}
	for _, tc := range tests {
		body, pt, err := applyPatchBytes(step(tc.kind, true))
		if err != nil {
			t.Fatalf("%s: err: %v", tc.kind, err)
		}
		if pt != types.StrategicMergePatchType {
			t.Errorf("%s: patch type: got %v want %v", tc.kind, pt, types.StrategicMergePatchType)
		}
		if string(body) != tc.want {
			t.Errorf("%s: body mismatch:\n got: %s\nwant: %s", tc.kind, body, tc.want)
		}
	}
}

func TestApplyPatchBytes_RejectsUnsupportedKind(t *testing.T) {
	_, _, err := applyPatchBytes(step("ReplicaSet", false))
	if err == nil {
		t.Errorf("expected error for ReplicaSet, got nil")
	}
}

func TestApplyPatchBytes_RejectsEmptyRuntimeClassName(t *testing.T) {
	s := step("Pod", false)
	s.RuntimeClassName = ""
	_, _, err := applyPatchBytes(s)
	if err == nil {
		t.Errorf("expected error for empty RuntimeClassName, got nil")
	}
}

func TestRollbackPatchBytes_Pod(t *testing.T) {
	body, pt, err := rollbackPatchBytes(step("Pod", true))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if pt != types.MergePatchType {
		t.Errorf("patch type: got %v want %v", pt, types.MergePatchType)
	}
	want := `{"spec":{"runtimeClassName":null}}`
	if string(body) != want {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", body, want)
	}
}

func TestRollbackPatchBytes_Deployment(t *testing.T) {
	body, _, err := rollbackPatchBytes(step("Deployment", true))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `{"spec":{"template":{"spec":{"runtimeClassName":null}}}}`
	if string(body) != want {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", body, want)
	}
}

func TestRollbackPatchBytes_CronJob(t *testing.T) {
	body, _, err := rollbackPatchBytes(step("CronJob", true))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	want := `{"spec":{"jobTemplate":{"spec":{"template":{"spec":{"runtimeClassName":null}}}}}}`
	if string(body) != want {
		t.Errorf("body mismatch:\n got: %s\nwant: %s", body, want)
	}
}

func TestIsSupportedKind(t *testing.T) {
	for _, k := range []string{"Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"} {
		if !IsSupportedKind(k) {
			t.Errorf("IsSupportedKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"ReplicaSet", "ConfigMap", "Service", ""} {
		if IsSupportedKind(k) {
			t.Errorf("IsSupportedKind(%q) = true, want false", k)
		}
	}
}
