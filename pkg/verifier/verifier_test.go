// Tests for the verifier core (verifier.go). We exercise Verify against
// k8s.io/client-go/kubernetes/fake.NewSimpleClientset, which is an
// in-memory implementation of kubernetes.Interface: List/Get against it
// returns objects we pre-seeded, so the tests run in milliseconds and
// never touch a real cluster.
//
// What these tests cover
//
//   - The happy path: every live pod's runtimeClassName matches the
//     plan's expectation, so the summary reports OK=Total.
//
//   - Mismatch detection: a pod with nil or different runtimeClassName
//     surfaces as Status="mismatch" with Actual populated.
//
//   - Error paths: missing controllers, controllers with no selected
//     pods, and unsupported kinds.
//
//   - Pod-kind PlanSteps (no controller indirection).
//
//   - The in-pod probe seam (ExecRunner): three sub-cases mapping to the
//     three branches in applyProbeResult (gvisor marker found, no marker
//     found, exec error).
//
//   - Pre-API validation: nil Client and nil Plan return Go errors
//     without making any API calls.
package verifier

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// ---------------------------------------------------------------------------
// Test helpers.
// ---------------------------------------------------------------------------

// strPtr returns a pointer to s. Used to populate pod.Spec.RuntimeClassName
// (which is *string in client-go) in fixtures.
func strPtr(s string) *string { return &s }

// runningPod constructs a pod with the given name/namespace/runtimeClassName
// and Status.Phase = Running, with the supplied labels. A nil rcn pointer
// means "field unset", which is the case we want to express as "no
// RuntimeClass" in the mismatch tests.
func runningPod(ns, name string, labels map[string]string, rcn *string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			RuntimeClassName: rcn,
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}
}

// deployment returns a minimal Deployment whose selector matches the
// given labels. We do not bother with replicas / template here because
// the verifier never reads them (it only consumes Spec.Selector).
func deployment(ns, name string, matchLabels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}

func statefulSet(ns, name string, matchLabels map[string]string) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: matchLabels},
		},
	}
}

// stepRef builds a one-step plan targeting kind/ns/name with the given
// runtimeClassName expectation. The Order is 1 and the rest of the
// fields are zero values, which is enough for the verifier.
func stepRef(kind, ns, name, rcn string) schema.PlanStep {
	return schema.PlanStep{
		Order:            1,
		Target:           schema.WorkloadRef{Kind: kind, Namespace: ns, Name: name},
		Action:           "set-runtime-class",
		RuntimeClassName: rcn,
	}
}

// makePlan builds a MigrationPlan envelope around the given steps, with
// a fixed PlanHash so the metadata round-trips deterministically. We
// renumber Order as 1..N here so individual step fixtures (built via
// stepRef) do not all carry Order=1.
func makePlan(steps ...schema.PlanStep) *schema.MigrationPlan {
	plan := schema.NewMigrationPlan()
	plan.Metadata = schema.PlanMetadata{PlanHash: "test-hash"}
	for i := range steps {
		steps[i].Order = i + 1
	}
	plan.Spec.Steps = steps
	return plan
}

// fakeExec is the test seam for the in-pod probe. It implements
// ExecRunner and returns canned stdout/stderr/exit. It also records the
// last call so tests can assert which pod was probed.
type fakeExec struct {
	stdout    []byte
	stderr    []byte
	err       error
	lastNs    string
	lastPod   string
	callCount int
}

func (f *fakeExec) Exec(_ context.Context, namespace, pod, _ string, _ []string) ([]byte, []byte, error) {
	f.callCount++
	f.lastNs = namespace
	f.lastPod = pod
	return f.stdout, f.stderr, f.err
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

// TestVerify_EveryPodMatchesOK: the happy-path baseline. Two controllers
// in two namespaces, every selected pod reports the expected
// runtimeClassName. Summary is all-OK and per-step Actual mirrors Expected.
func TestVerify_EveryPodMatchesOK(t *testing.T) {
	webLabels := map[string]string{"app": "web"}
	cacheLabels := map[string]string{"app": "cache"}
	client := fake.NewSimpleClientset(
		deployment("ns-a", "web", webLabels),
		statefulSet("ns-b", "cache", cacheLabels),
		runningPod("ns-a", "web-0", webLabels, strPtr("gvisor")),
		runningPod("ns-b", "cache-0", cacheLabels, strPtr("gvisor")),
	)
	plan := makePlan(
		stepRef("Deployment", "ns-a", "web", "gvisor"),
		stepRef("StatefulSet", "ns-b", "cache", "gvisor"),
	)

	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := report.Spec.Summary; got.OK != 2 || got.Mismatch != 0 || got.Error != 0 || got.Total != 2 {
		t.Errorf("summary: %+v want OK=2, Mismatch=0, Error=0, Total=2", got)
	}
	for _, r := range report.Spec.Results {
		if r.Status != schema.VerifyStatusOK {
			t.Errorf("step %d %s/%s: status=%q want ok",
				r.Order, r.Target.Namespace, r.Target.Name, r.Status)
		}
		if r.Actual != "gvisor" {
			t.Errorf("step %d %s/%s: actual=%q want %q",
				r.Order, r.Target.Namespace, r.Target.Name, r.Actual, "gvisor")
		}
	}
}

// TestVerify_PodMissingRuntimeClass_Mismatch: the pod exists and matches
// the controller's selector, but its runtimeClassName is nil. We treat
// nil as empty string and report Actual="" with Status=mismatch.
func TestVerify_PodMissingRuntimeClass_Mismatch(t *testing.T) {
	webLabels := map[string]string{"app": "web"}
	client := fake.NewSimpleClientset(
		deployment("ns-a", "web", webLabels),
		runningPod("ns-a", "web-0", webLabels, nil),
	)
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))

	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := report.Spec.Summary; got.Mismatch != 1 || got.OK != 0 {
		t.Errorf("summary: %+v want Mismatch=1", got)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusMismatch {
		t.Errorf("status: got %q want mismatch", r.Status)
	}
	if r.Actual != "" {
		t.Errorf("actual: got %q want empty", r.Actual)
	}
}

// TestVerify_PodWrongRuntimeClass_Mismatch: the pod runs with a non-empty
// but incorrect runtimeClassName. Actual reflects the live value verbatim.
func TestVerify_PodWrongRuntimeClass_Mismatch(t *testing.T) {
	webLabels := map[string]string{"app": "web"}
	client := fake.NewSimpleClientset(
		deployment("ns-a", "web", webLabels),
		runningPod("ns-a", "web-0", webLabels, strPtr("other")),
	)
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))

	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusMismatch {
		t.Errorf("status: got %q want mismatch", r.Status)
	}
	if r.Actual != "other" {
		t.Errorf("actual: got %q want %q", r.Actual, "other")
	}
}

func TestVerify_NoPodsForController_Error(t *testing.T) {
	// Deployment exists but no pods match its selector. The verifier
	// cannot tell whether this is a paused rollout or a real bug, so it
	// reports error and names the workload in the message.
	client := fake.NewSimpleClientset(
		deployment("ns-a", "web", map[string]string{"app": "web"}),
	)
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))

	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := report.Spec.Summary.Error; got != 1 {
		t.Errorf("summary.Error: got %d want 1", got)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusError {
		t.Errorf("status: got %q want error", r.Status)
	}
	if !strings.Contains(r.Message, "web") {
		t.Errorf("message should mention the workload name: %q", r.Message)
	}
}

func TestVerify_ControllerNotFound_Error(t *testing.T) {
	// No controllers in the client at all. The Get should NotFound and
	// the per-step result becomes error with "not found" in the message.
	client := fake.NewSimpleClientset()
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))

	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusError {
		t.Errorf("status: got %q want error", r.Status)
	}
	if !strings.Contains(r.Message, "not found") {
		t.Errorf("message should contain 'not found': %q", r.Message)
	}
}

func TestVerify_MixedOKAndMismatch(t *testing.T) {
	// 3-step plan: two pass, one fails. Confirms the summary tallies
	// per-status and the per-step rows correctly identify which step
	// is the mismatch.
	a := map[string]string{"app": "a"}
	b := map[string]string{"app": "b"}
	c := map[string]string{"app": "c"}
	client := fake.NewSimpleClientset(
		deployment("ns", "a", a),
		deployment("ns", "b", b),
		deployment("ns", "c", c),
		runningPod("ns", "a-0", a, strPtr("gvisor")),
		runningPod("ns", "b-0", b, strPtr("runc")), // mismatch
		runningPod("ns", "c-0", c, strPtr("gvisor")),
	)
	plan := makePlan(
		stepRef("Deployment", "ns", "a", "gvisor"),
		stepRef("Deployment", "ns", "b", "gvisor"),
		stepRef("Deployment", "ns", "c", "gvisor"),
	)

	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := report.Spec.Summary; got.OK != 2 || got.Mismatch != 1 || got.Error != 0 {
		t.Errorf("summary: %+v want OK=2, Mismatch=1, Error=0", got)
	}
	// Per-step assertion: order 2 should be the mismatch.
	for _, r := range report.Spec.Results {
		switch r.Order {
		case 1, 3:
			if r.Status != schema.VerifyStatusOK {
				t.Errorf("step %d: status=%q want ok", r.Order, r.Status)
			}
		case 2:
			if r.Status != schema.VerifyStatusMismatch {
				t.Errorf("step %d: status=%q want mismatch", r.Order, r.Status)
			}
			if r.Actual != "runc" {
				t.Errorf("step 2: actual=%q want %q", r.Actual, "runc")
			}
		}
	}
}

func TestVerify_PodKindStep(t *testing.T) {
	t.Run("pod_kind_step_ok", func(t *testing.T) {
		// A PlanStep targeting a Pod directly. The verifier should Get
		// the pod (no selector lookup) and compare its runtimeClassName.
		client := fake.NewSimpleClientset(
			runningPod("ns-a", "lone", nil, strPtr("gvisor")),
		)
		plan := makePlan(stepRef("Pod", "ns-a", "lone", "gvisor"))

		report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		r := report.Spec.Results[0]
		if r.Status != schema.VerifyStatusOK {
			t.Errorf("status: got %q want ok", r.Status)
		}
	})

	t.Run("pod_kind_step_mismatch", func(t *testing.T) {
		client := fake.NewSimpleClientset(
			runningPod("ns-a", "lone", nil, strPtr("other")),
		)
		plan := makePlan(stepRef("Pod", "ns-a", "lone", "gvisor"))

		report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		r := report.Spec.Results[0]
		if r.Status != schema.VerifyStatusMismatch {
			t.Errorf("status: got %q want mismatch", r.Status)
		}
		if r.Actual != "other" {
			t.Errorf("actual: got %q want %q", r.Actual, "other")
		}
	})
}

// probeFixtureClient returns the fake client used by the InPodProbe seam
// tests: a Deployment with a single matching Running pod whose
// runtimeClassName is "gvisor". The spec-level check passes here; the
// probe is what differentiates the three sub-cases.
func probeFixtureClient() *fake.Clientset {
	labels := map[string]string{"app": "web"}
	return fake.NewSimpleClientset(
		deployment("ns-a", "web", labels),
		runningPod("ns-a", "web-0", labels, strPtr("gvisor")),
	)
}

// TestVerify_InPodProbe_MarkerPresent_StaysOK: the probe stdout contains
// "gvisor", so the spec-level ok is preserved and Probe.Detected=true.
// Also asserts the fake ExecRunner was called against the expected pod.
func TestVerify_InPodProbe_MarkerPresent_StaysOK(t *testing.T) {
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))
	exec := &fakeExec{stdout: []byte("Linux web-0 5.10.0-runsc gVisor #1 SMP")}
	report, err := Verify(context.Background(), Options{
		Client: probeFixtureClient(), Plan: plan, InPodProbe: true, Exec: exec,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusOK {
		t.Errorf("status: got %q want ok", r.Status)
	}
	if r.Probe == nil || !r.Probe.Detected {
		t.Errorf("Probe.Detected: got %v want true (probe=%+v)", r.Probe != nil && r.Probe.Detected, r.Probe)
	}
	if r.Probe.Markers != "gvisor" {
		t.Errorf("Probe.Markers: got %q want %q", r.Probe.Markers, "gvisor")
	}
	if exec.callCount != 1 {
		t.Errorf("exec.callCount: got %d want 1", exec.callCount)
	}
	if exec.lastPod != "web-0" {
		t.Errorf("exec.lastPod: got %q want %q", exec.lastPod, "web-0")
	}
}

// TestVerify_InPodProbe_MarkerAbsent_DemotesToMismatch: the spec-level
// check passed (runtimeClassName=gvisor) but the probe sees a runc
// kernel. This is the misleading case the probe exists for: the pod's
// spec advertises gVisor but the kernel surface inside does not back it
// up. We demote to mismatch.
func TestVerify_InPodProbe_MarkerAbsent_DemotesToMismatch(t *testing.T) {
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))
	exec := &fakeExec{stdout: []byte("Linux web-0 5.15.0-runc #1 SMP")}
	report, err := Verify(context.Background(), Options{
		Client: probeFixtureClient(), Plan: plan, InPodProbe: true, Exec: exec,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusMismatch {
		t.Errorf("status: got %q want mismatch", r.Status)
	}
	if r.Probe == nil || r.Probe.Detected {
		t.Errorf("Probe.Detected: got %v want false", r.Probe != nil && r.Probe.Detected)
	}
	if !strings.Contains(r.Message, "no gVisor markers") {
		t.Errorf("message: %q should mention no gVisor markers", r.Message)
	}
}

// TestVerify_InPodProbe_ExecError_BecomesError: the ExecRunner itself
// errors (e.g. connection refused, container missing). We cannot tell
// whether the workload is on gVisor, so the result is Status=error and
// Probe.Error is populated.
func TestVerify_InPodProbe_ExecError_BecomesError(t *testing.T) {
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))
	exec := &fakeExec{err: errors.New("connection refused")}
	report, err := Verify(context.Background(), Options{
		Client: probeFixtureClient(), Plan: plan, InPodProbe: true, Exec: exec,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusError {
		t.Errorf("status: got %q want error", r.Status)
	}
	if r.Probe == nil || r.Probe.Error == "" {
		t.Errorf("Probe.Error: should be populated, got %+v", r.Probe)
	}
	if !strings.Contains(r.Message, "probe exec failed") {
		t.Errorf("message: %q should mention probe exec failed", r.Message)
	}
}

func TestVerify_NilPlanRejected(t *testing.T) {
	// Confirms a nil plan returns a Go error (the precondition path).
	// We pass a real fake client so the test fails closed if the
	// validation order ever flips.
	client := fake.NewSimpleClientset()
	_, err := Verify(context.Background(), Options{Client: client})
	if err == nil {
		t.Fatalf("expected error for nil plan, got nil")
	}
	if !strings.Contains(err.Error(), "verifier") {
		t.Errorf("error should be prefixed with verifier: %q", err.Error())
	}
	// And no API actions should have been issued.
	if got := len(client.Actions()); got != 0 {
		t.Errorf("API actions: got %d want 0", got)
	}
}

func TestVerify_NilClientRejected(t *testing.T) {
	_, err := Verify(context.Background(), Options{Plan: makePlan()})
	if err == nil {
		t.Fatalf("expected error for nil client, got nil")
	}
	if !strings.Contains(err.Error(), "verifier") {
		t.Errorf("error should be prefixed with verifier: %q", err.Error())
	}
}

func TestVerify_UnsupportedKind_Error(t *testing.T) {
	// A PlanStep with an unsupported kind should not crash the verifier;
	// it should surface as a per-step error.
	client := fake.NewSimpleClientset()
	plan := makePlan(stepRef("ReplicaSet", "ns", "rs", "gvisor"))
	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusError {
		t.Errorf("status: got %q want error", r.Status)
	}
	if !strings.Contains(r.Message, "unsupported kind") {
		t.Errorf("message should mention unsupported kind: %q", r.Message)
	}
}

func TestVerify_ProbeNoRunningPod_Error(t *testing.T) {
	// The deployment exists, its pod matches the selector and has the
	// correct runtimeClassName, but the pod is not yet Running. With
	// the probe on, we cannot exec, so the result becomes error.
	labels := map[string]string{"app": "web"}
	pending := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns-a", Name: "web-0", Labels: labels},
		Spec:       corev1.PodSpec{RuntimeClassName: strPtr("gvisor")},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	client := fake.NewSimpleClientset(
		deployment("ns-a", "web", labels),
		pending,
	)
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))

	exec := &fakeExec{stdout: []byte("never called")}
	report, err := Verify(context.Background(), Options{
		Client: client, Plan: plan, InPodProbe: true, Exec: exec,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusError {
		t.Errorf("status: got %q want error", r.Status)
	}
	if !strings.Contains(r.Message, "no Running pod") {
		t.Errorf("message: %q should mention no Running pod", r.Message)
	}
	if exec.callCount != 0 {
		t.Errorf("exec should not have been called: callCount=%d", exec.callCount)
	}
}

func TestVerify_DefaultRuntimeClassName_WhenStepEmpty(t *testing.T) {
	// When a PlanStep leaves RuntimeClassName empty the verifier should
	// fall back to schema.DefaultRuntimeClassName, mirroring the
	// applier's contract.
	labels := map[string]string{"app": "web"}
	client := fake.NewSimpleClientset(
		deployment("ns-a", "web", labels),
		runningPod("ns-a", "web-0", labels, strPtr(schema.DefaultRuntimeClassName)),
	)
	plan := makePlan(stepRef("Deployment", "ns-a", "web", ""))

	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Expected != schema.DefaultRuntimeClassName {
		t.Errorf("Expected: got %q want %q", r.Expected, schema.DefaultRuntimeClassName)
	}
	if r.Status != schema.VerifyStatusOK {
		t.Errorf("status: got %q want ok", r.Status)
	}
}

func TestVerify_JobAndDaemonSetSelectors(t *testing.T) {
	// Cover the Job and DaemonSet branches in resolvePods.
	jobLabels := map[string]string{"app": "j"}
	dsLabels := map[string]string{"app": "d"}
	client := fake.NewSimpleClientset(
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "j"},
			Spec: batchv1.JobSpec{
				Selector: &metav1.LabelSelector{MatchLabels: jobLabels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				},
			},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "d"},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: dsLabels},
			},
		},
		runningPod("ns", "j-0", jobLabels, strPtr("gvisor")),
		runningPod("ns", "d-0", dsLabels, strPtr("gvisor")),
	)
	plan := makePlan(
		stepRef("Job", "ns", "j", "gvisor"),
		stepRef("DaemonSet", "ns", "d", "gvisor"),
	)
	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := report.Spec.Summary.OK; got != 2 {
		t.Errorf("summary.OK: got %d want 2", got)
	}
}

func TestVerify_CronJobFallbackToTemplateLabels(t *testing.T) {
	// CronJob has no top-level selector. The verifier falls back to the
	// JobTemplate's pod-template labels: the pod the cron eventually
	// creates carries those labels.
	cjLabels := map[string]string{"cronjob": "nightly"}
	client := fake.NewSimpleClientset(
		&batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "cj"},
			Spec: batchv1.CronJobSpec{
				JobTemplate: batchv1.JobTemplateSpec{
					Spec: batchv1.JobSpec{
						Template: corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{Labels: cjLabels},
						},
					},
				},
			},
		},
		runningPod("ns", "cj-1234", cjLabels, strPtr("gvisor")),
	)
	plan := makePlan(stepRef("CronJob", "ns", "cj", "gvisor"))
	report, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusOK {
		t.Errorf("status: got %q (msg=%q) want ok", r.Status, r.Message)
	}
}

func TestVerify_ReportMetadataPopulated(t *testing.T) {
	// Verifies the envelope metadata gets the values we threaded through
	// Options. Useful so a wire-shape regression (e.g. forgetting to
	// stamp PlanHash) does not slip past the per-step assertions.
	client := fake.NewSimpleClientset()
	plan := makePlan()
	report, err := Verify(context.Background(), Options{
		Client: client, Plan: plan,
		Cluster:          "test-ctx",
		AgentmoatVersion: "v0.0.0-test",
		InPodProbe:       false,
	})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.APIVersion != schema.APIVersion {
		t.Errorf("APIVersion: got %q want %q", report.APIVersion, schema.APIVersion)
	}
	if report.Kind != schema.KindVerifyReport {
		t.Errorf("Kind: got %q want %q", report.Kind, schema.KindVerifyReport)
	}
	if report.Metadata.Cluster != "test-ctx" {
		t.Errorf("Cluster: got %q want %q", report.Metadata.Cluster, "test-ctx")
	}
	if report.Metadata.AgentmoatVersion != "v0.0.0-test" {
		t.Errorf("AgentmoatVersion: got %q want %q", report.Metadata.AgentmoatVersion, "v0.0.0-test")
	}
	if report.Metadata.PlanHash != "test-hash" {
		t.Errorf("PlanHash: got %q want %q", report.Metadata.PlanHash, "test-hash")
	}
	if report.Metadata.InPodProbe != false {
		t.Errorf("InPodProbe: got %v want false", report.Metadata.InPodProbe)
	}
	if report.Metadata.GeneratedAt == "" {
		t.Errorf("GeneratedAt should be populated")
	}
}

func TestVerify_InPodProbe_NilExecAndNilConfig_Rejected(t *testing.T) {
	// When InPodProbe is true but neither Exec nor Config is supplied,
	// Verify cannot build the default SPDY runner. We surface this as a
	// Go error (a programming bug from the orchestrator), not as a
	// per-step error: there is no per-step granularity to attach it to.
	client := fake.NewSimpleClientset()
	_, err := Verify(context.Background(), Options{
		Client: client, Plan: makePlan(), InPodProbe: true,
	})
	if err == nil {
		t.Fatalf("expected error for InPodProbe with nil Exec and nil Config")
	}
	if !strings.Contains(err.Error(), "InPodProbe") {
		t.Errorf("error should mention InPodProbe: %q", err.Error())
	}
}

// TestVerify_TerminatingPodIgnored confirms that pods with a DeletionTimestamp
// set are excluded from the spec-level check. This is the rolling-update
// race the verifier needs to dodge: post-apply, the old replicas linger as
// Terminating while the new replicas come up. The old pods still carry the
// pre-patch (empty) runtimeClassName for a few seconds; if the verifier
// counted them it would flap mismatch every time it ran during a rollout.
func TestVerify_TerminatingPodIgnored(t *testing.T) {
	webLabels := map[string]string{"app": "web"}
	now := metav1.Now()
	// Terminating old pod: deletion timestamp set, runtimeClassName nil
	// (pre-patch). Should be ignored.
	oldPod := runningPod("ns-a", "web-old", webLabels, nil)
	oldPod.DeletionTimestamp = &now
	// Fresh new pod: no deletion timestamp, has the expected gvisor RCN.
	newPod := runningPod("ns-a", "web-new", webLabels, strPtr("gvisor"))

	client := fake.NewSimpleClientset(
		deployment("ns-a", "web", webLabels),
		oldPod, newPod,
	)
	plan := makePlan(stepRef("Deployment", "ns-a", "web", "gvisor"))

	rep, err := Verify(context.Background(), Options{Client: client, Plan: plan})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.Spec.Summary.OK != 1 || rep.Spec.Summary.Mismatch != 0 {
		t.Errorf("expected ok=1 mismatch=0, got %+v\nresults: %+v",
			rep.Spec.Summary, rep.Spec.Results)
	}
}
