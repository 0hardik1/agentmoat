// Tests for a verify from manifest files (Options.Targets): the workloads
// come from a list and not from a plan, a kind with no reader is checked
// through the selector of its manifest, and a target with no verdict goes
// to the not-checked list and never to the results.
//
// The rule under test: a result that is not ok comes only from evidence in
// the cluster. "No pods match the selector" is not evidence when the
// verifier did not read the object.
package verifier

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/0hardik1/agentmoat/internal/schema"
)

const rolloutKind = "Rollout.argoproj.io"

// rolloutTarget is a target of a kind that the verifier has no reader for,
// with the selector of its manifest.
func rolloutTarget(name string, labels map[string]string) Target {
	return Target{
		Ref:      schema.WorkloadRef{Kind: rolloutKind, Namespace: "shop", Name: name},
		Expected: "gvisor",
		Selector: &metav1.LabelSelector{MatchLabels: labels},
		Origin:   &schema.Origin{File: "k8s/" + name + ".yaml", Line: 3},
	}
}

func deploymentTarget(name string) Target {
	return Target{
		Ref:      schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: name},
		Expected: "gvisor",
		Origin:   &schema.Origin{File: "k8s/" + name + ".yaml", Line: 1},
	}
}

// TestTargets_AKindWithAReader: a Deployment from a manifest is read from
// the cluster as a plan step is, and the result has the origin.
func TestTargets_AKindWithAReader(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		deployment("shop", "web", webLabels),
		runningPod("shop", "web-1", webLabels, strPtr("gvisor")),
	)
	report, err := Verify(context.Background(), Options{
		Client: client, Targets: []Target{deploymentTarget("web")}, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	r := report.Spec.Results[0]
	if r.Order != 1 || r.Basis != "" || r.Origin == nil || r.Origin.File != "k8s/web.yaml" || r.Expected != "gvisor" {
		t.Errorf("result = %+v", r)
	}
	if report.Metadata.PlanHash != "" {
		t.Errorf("planHash = %q, want none", report.Metadata.PlanHash)
	}
}

// TestTargets_BySelector: a kind with no reader. The verdict is on the pods
// that the selector of the manifest selects, and the result says so.
func TestTargets_BySelector(t *testing.T) {
	t.Parallel()
	canary := map[string]string{"app": "canary"}
	tests := []struct {
		name       string
		pod        *string // the runtimeClassName of the one pod
		wantStatus schema.VerifyStatus
		wantInMsg  string
	}{
		{"the pods have the class", strPtr("gvisor"), schema.VerifyStatusOK, "verify did not read the " + rolloutKind + " object"},
		{"a pod does not have the class", nil, schema.VerifyStatusMismatch, `pod canary-1 has runtimeClassName=""`},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset(
			runningPod("shop", "canary-1", canary, tc.pod),
			// A pod of another workload, with other labels: not selected.
			runningPod("shop", "other-1", map[string]string{"app": "other"}, nil),
		)
		report, err := Verify(context.Background(), Options{
			Client: client, Targets: []Target{rolloutTarget("canary", canary)}, Stderr: &bytes.Buffer{},
		})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(report.Spec.Results) != 1 || len(report.Spec.NotChecked) != 0 {
			t.Fatalf("%s: %d results, %d not checked", tc.name, len(report.Spec.Results), len(report.Spec.NotChecked))
		}
		r := report.Spec.Results[0]
		if r.Status != tc.wantStatus || r.Basis != schema.VerifyBasisSelector || !strings.Contains(r.Message, tc.wantInMsg) {
			t.Errorf("%s: result = %+v", tc.name, r)
		}
		// The verifier reads no object of the kind: it has no client for
		// it. Each call is a list of pods, or a read for the placement.
		for _, a := range client.Actions() {
			if res := a.GetResource().Resource; res != "pods" && res != "runtimeclasses" && res != "nodes" {
				t.Errorf("%s: the verifier read %s", tc.name, res)
			}
		}
	}
}

// TestTargets_NoPodsIsNotAVerdict: with no pods for a selector, the verifier
// does not know if the workload is scaled to zero or is not there. That is
// not a mismatch and not an error (each would be exit code 4): the target
// has no result, and is in the not-checked list with the reason.
func TestTargets_NoPodsIsNotAVerdict(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		deployment("shop", "web", webLabels),
		runningPod("shop", "web-1", webLabels, strPtr("gvisor")),
	)
	report, err := Verify(context.Background(), Options{
		Client:  client,
		Targets: []Target{rolloutTarget("canary", map[string]string{"app": "canary"}), deploymentTarget("web")},
		Stderr:  &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1, NotChecked: 1}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	// The result that stays keeps its place in the list of targets and
	// its own origin.
	if r := report.Spec.Results[0]; r.Target.Name != "web" || r.Order != 2 || r.Origin.File != "k8s/web.yaml" {
		t.Errorf("result = %+v", r)
	}
	n := report.Spec.NotChecked[0]
	if n.Target.Name != "canary" || n.Origin.File != "k8s/canary.yaml" || !strings.Contains(n.Reason, "scaled to zero") {
		t.Errorf("not checked = %+v", n)
	}
	for _, r := range report.Spec.Results {
		if r.Status != schema.VerifyStatusOK && r.Status != schema.VerifyStatusMismatch && r.Status != schema.VerifyStatusError {
			t.Errorf("a result has the status %q, which is not a wire value", r.Status)
		}
	}
}

// TestTargets_WaitLooksAgainForPods: with a wait time, a selector with no
// pods is looked at again. The pods come in the second interval, and the
// target is ok when they stayed Ready.
func TestTargets_WaitLooksAgainForPods(t *testing.T) {
	t.Parallel()
	canary := map[string]string{"app": "canary"}
	client := fake.NewSimpleClientset()
	clock := newFakeClock()
	clock.onSleep = func(n int32) {
		if n == 2 {
			createPod(t, client, readyPod("shop", "canary-1", canary, strPtr("gvisor")))
		}
	}
	report, err := Verify(context.Background(), Options{
		Client: client, Targets: []Target{rolloutTarget("canary", canary)},
		Wait: 2 * time.Minute, Clock: clock, Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1}); got != want {
		t.Fatalf("summary = %+v, want %+v (not checked: %+v)", got, want, report.Spec.NotChecked)
	}
	r := report.Spec.Results[0]
	if r.Readiness == nil || !r.Readiness.Stable || r.Basis != schema.VerifyBasisSelector || report.Metadata.Wait.TimedOut {
		t.Errorf("result = %+v, wait = %+v", r, report.Metadata.Wait)
	}
}

// TestTargets_WaitEndsWithNoPods: the wait ends and the selector still has
// no pods. The target is not checked. It is not a failure after the wait
// either: the verifier still does not know.
func TestTargets_WaitEndsWithNoPods(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	report, err := Verify(context.Background(), Options{
		Client: client, Targets: []Target{rolloutTarget("canary", map[string]string{"app": "canary"})},
		Wait: time.Minute, Clock: newFakeClock(), Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := report.Spec.Summary, (schema.VerifySummary{NotChecked: 1}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if len(report.Spec.Results) != 0 || report.Spec.Results == nil {
		t.Errorf("results = %#v, want an empty list that is not nil", report.Spec.Results)
	}
	if w := report.Metadata.Wait; w == nil || w.Rounds < 2 {
		t.Errorf("wait = %+v, want more than one look", w)
	}
}

// TestTargets_Rejected: input that would make a verdict about the wrong
// pods is a programming error of the caller, and no API call is made.
func TestTargets_Rejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"a plan and targets", Options{Plan: makePlan(), Targets: []Target{}}, "one source"},
		{"one target two times", Options{Targets: []Target{deploymentTarget("web"), deploymentTarget("web")}}, "two times"},
		// A selector with no terms selects each pod of the namespace.
		{"a selector with no terms", Options{Targets: []Target{{
			Ref: schema.WorkloadRef{Kind: rolloutKind, Namespace: "shop", Name: "canary"}, Selector: &metav1.LabelSelector{},
		}}}, "has no terms"},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset()
		tc.opts.Client = client
		_, err := Verify(context.Background(), tc.opts)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to have %q", tc.name, err, tc.want)
		}
		if n := len(client.Actions()); n != 0 {
			t.Errorf("%s: %d API actions, want 0", tc.name, n)
		}
	}
}

// TestTargets_AnEmptyListIsAReport: a verify from manifests in which each
// workload was not checked by the caller gives a report with no results,
// not an error.
func TestTargets_AnEmptyListIsAReport(t *testing.T) {
	t.Parallel()
	report, err := Verify(context.Background(), Options{Client: fake.NewSimpleClientset(), Targets: []Target{}, Stderr: &bytes.Buffer{}})
	if err != nil {
		t.Fatal(err)
	}
	if report.Spec.Summary != (schema.VerifySummary{}) || len(report.Spec.Results) != 0 {
		t.Errorf("report = %+v", report.Spec)
	}
}

// ---------------------------------------------------------------------------
// A Job and a bare Pod: workloads that run to an end.
// ---------------------------------------------------------------------------
//
// A plan has no step for a Job or a Pod, so these kinds reach the verifier
// only from manifest files. They are different from a Deployment in two
// ways, and each one could make a false rollback signal:
//
//   - After a good deploy the object can be gone: a Job with
//     ttlSecondsAfterFinished, a hook that its tool deletes when it
//     succeeds, a test pod that an install never makes. "Not found" is then
//     not evidence that the deploy failed.
//   - A pod that ran to its end is never Ready again. With a wait time, the
//     readiness check must not hold that against it.

func jobTarget(name string) Target {
	return Target{
		Ref:      schema.WorkloadRef{Kind: "Job", Namespace: "shop", Name: name},
		Expected: "gvisor",
		Origin:   &schema.Origin{File: "k8s/" + name + ".yaml", Line: 1},
	}
}

func podTarget(name string) Target {
	return Target{
		Ref:      schema.WorkloadRef{Kind: "Pod", Namespace: "shop", Name: name},
		Expected: "gvisor",
		Origin:   &schema.Origin{File: "k8s/" + name + ".yaml", Line: 1},
	}
}

// endedJob is a Job with one condition that is true (Complete or Failed).
// class "" means that the pod template has no runtimeClassName.
func endedJob(name, class string, condition batchv1.JobConditionType) *batchv1.Job {
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
		Spec: batchv1.JobSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"job-name": name}},
		},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{
			Type: condition, Status: corev1.ConditionTrue, Reason: "BackoffLimitExceeded",
			Message: "Job has reached the specified backoff limit",
		}}},
	}
	if class != "" {
		job.Spec.Template.Spec.RuntimeClassName = &class
	}
	return job
}

// endedPod is a bare pod in a terminal phase.
func endedPod(name, class string, phase corev1.PodPhase) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
		Status: corev1.PodStatus{
			Phase:      phase,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "PodCompleted"}},
		},
	}
	if class != "" {
		pod.Spec.RuntimeClassName = &class
	}
	return pod
}

// TestTargets_RunToAnEnd: one look, and a look with a wait time, at each
// state of a Job and of a bare Pod from a manifest.
func TestTargets_RunToAnEnd(t *testing.T) {
	t.Parallel()
	const notChecked = schema.VerifyStatus("") // the target has no result
	tests := []struct {
		name    string
		target  Target
		cluster []runtime.Object
		// The status with no wait time, and with one.
		look, wait schema.VerifyStatus
		wantInMsg  string
	}{
		{
			name:   "a Job that is not in the cluster",
			target: jobTarget("migrate"),
			look:   notChecked, wait: notChecked,
			wantInMsg: "Job shop/migrate is not in the cluster",
		},
		{
			name:    "a Job that is complete, with the class in its pod template",
			target:  jobTarget("migrate"),
			cluster: []runtime.Object{endedJob("migrate", "gvisor", batchv1.JobComplete)},
			look:    schema.VerifyStatusOK, wait: schema.VerifyStatusOK,
			wantInMsg: "the Job is finished",
		},
		{
			// The Job of the cluster is the one from before the change:
			// the deploy did not replace it. This is evidence.
			name:    "a Job that is complete, with no class in its pod template",
			target:  jobTarget("migrate"),
			cluster: []runtime.Object{endedJob("migrate", "", batchv1.JobComplete)},
			look:    schema.VerifyStatusMismatch, wait: schema.VerifyStatusMismatch,
			wantInMsg: `the pod template has runtimeClassName=""`,
		},
		{
			// One look does not read how a workload does. With a wait
			// time, a Job that failed with the class did not come up in
			// the sandbox.
			name:    "a Job that failed, with the class in its pod template",
			target:  jobTarget("migrate"),
			cluster: []runtime.Object{endedJob("migrate", "gvisor", batchv1.JobFailed)},
			look:    schema.VerifyStatusOK, wait: schema.VerifyStatusMismatch,
			wantInMsg: "the Job",
		},
		{
			name:   "a pod that is not in the cluster",
			target: podTarget("smoke"),
			look:   notChecked, wait: notChecked,
			wantInMsg: "Pod shop/smoke is not in the cluster",
		},
		{
			name:    "a pod that ran to its end, with the class",
			target:  podTarget("smoke"),
			cluster: []runtime.Object{endedPod("smoke", "gvisor", corev1.PodSucceeded)},
			look:    schema.VerifyStatusOK, wait: schema.VerifyStatusOK,
			wantInMsg: "ran to its end",
		},
		{
			name:    "a pod that ran to its end, with no class",
			target:  podTarget("smoke"),
			cluster: []runtime.Object{endedPod("smoke", "", corev1.PodSucceeded)},
			look:    schema.VerifyStatusMismatch, wait: schema.VerifyStatusMismatch,
			wantInMsg: `pod smoke has runtimeClassName=""`,
		},
		{
			// One look reads the class only (an ok result on pods has no
			// message). With a wait time the pod is not Ready, and the
			// readiness check says so.
			name:    "a pod that failed, with the class",
			target:  podTarget("smoke"),
			cluster: []runtime.Object{endedPod("smoke", "gvisor", corev1.PodFailed)},
			look:    schema.VerifyStatusOK, wait: schema.VerifyStatusMismatch,
		},
		{
			// A Deployment is not removed when it is done. Not in the
			// cluster is evidence: the deploy did not make it.
			name:   "a Deployment that is not in the cluster",
			target: deploymentTarget("web"),
			look:   schema.VerifyStatusError, wait: schema.VerifyStatusError,
			wantInMsg: "Deployment shop/web not found",
		},
	}
	for _, tc := range tests {
		for _, wait := range []time.Duration{0, time.Minute} {
			want := tc.look
			if wait > 0 {
				want = tc.wait
			}
			report, err := Verify(context.Background(), Options{
				Client: fake.NewSimpleClientset(tc.cluster...), Targets: []Target{tc.target},
				Wait: wait, Clock: newFakeClock(), Stderr: &bytes.Buffer{},
			})
			if err != nil {
				t.Fatalf("%s (wait %s): %v", tc.name, wait, err)
			}
			got, message := notChecked, ""
			switch {
			case len(report.Spec.Results) == 1 && len(report.Spec.NotChecked) == 0:
				got, message = report.Spec.Results[0].Status, report.Spec.Results[0].Message
			case len(report.Spec.Results) == 0 && len(report.Spec.NotChecked) == 1:
				message = report.Spec.NotChecked[0].Reason
			default:
				t.Fatalf("%s (wait %s): %d results and %d not checked; want one of the two",
					tc.name, wait, len(report.Spec.Results), len(report.Spec.NotChecked))
			}
			if got != want {
				t.Errorf("%s (wait %s): status = %q, want %q (message: %s)", tc.name, wait, got, want, message)
			}
			// The message of the status that the two cases share.
			if got == tc.look && !strings.Contains(message, tc.wantInMsg) {
				t.Errorf("%s (wait %s): message = %q, want it to have %q", tc.name, wait, message, tc.wantInMsg)
			}
		}
	}
}

// TestTargets_APodThatRanToItsEndIsNotProbed: the in-pod probe needs a
// container to exec into, and a pod in the phase Succeeded has none. That
// must not be an error (it would be exit code 4 for a pod that ran in the
// sandbox with no fault). The result is ok on the class, and the exec seam
// is not called.
func TestTargets_APodThatRanToItsEndIsNotProbed(t *testing.T) {
	t.Parallel()
	for _, wait := range []time.Duration{0, time.Minute} {
		exec := &fakeExec{stdout: []byte("no markers here\n")}
		report, err := Verify(context.Background(), Options{
			Client:  fake.NewSimpleClientset(endedPod("smoke", "gvisor", corev1.PodSucceeded)),
			Targets: []Target{podTarget("smoke")}, Wait: wait, Clock: newFakeClock(), Stderr: &bytes.Buffer{},
			InPodProbe: true, Exec: exec,
		})
		if err != nil {
			t.Fatalf("wait %s: %v", wait, err)
		}
		if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1}); got != want {
			t.Fatalf("wait %s: summary = %+v, want %+v (%+v)", wait, got, want, report.Spec.Results)
		}
		r := report.Spec.Results[0]
		if r.Probe != nil || r.Readiness != nil || exec.lastPod != "" {
			t.Errorf("wait %s: probe = %+v, readiness = %+v, exec into %q; want none of them", wait, r.Probe, r.Readiness, exec.lastPod)
		}
		// The step is final at the first look: it does not use the wait.
		if wait > 0 && report.Metadata.Wait.Rounds != 1 {
			t.Errorf("wait %s: %d looks, want 1", wait, report.Metadata.Wait.Rounds)
		}
	}
}

// TestTargets_AJobThatFailedSaysWhy: with a wait time, the result of a Job
// that failed has the reason from the condition of the Job.
func TestTargets_AJobThatFailedSaysWhy(t *testing.T) {
	t.Parallel()
	report, err := Verify(context.Background(), Options{
		Client:  fake.NewSimpleClientset(endedJob("migrate", "gvisor", batchv1.JobFailed)),
		Targets: []Target{jobTarget("migrate")}, Wait: time.Minute, Clock: newFakeClock(), Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := report.Spec.Results[0]
	want := "the Job failed (BackoffLimitExceeded: Job has reached the specified backoff limit)"
	if r.Status != schema.VerifyStatusMismatch || !strings.Contains(r.Message, want) || r.Actual != "gvisor" {
		t.Errorf("result = %+v, want a mismatch with %q", r, want)
	}
}

// TestPlan_NotFoundStaysAnError: the rule for an object that is gone is for
// targets from manifests only. A step of a plan names a workload that
// `apply` patched, and "not found" there is an error, as before.
func TestPlan_NotFoundStaysAnError(t *testing.T) {
	t.Parallel()
	report, err := Verify(context.Background(), Options{
		Client: fake.NewSimpleClientset(), Plan: makePlan(stepRef("Job", "shop", "migrate", "gvisor")), Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, Error: 1}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
}

// TestReadError_IsOneLineOnStderr: the line that says why a read failed has
// the names of the target, and with `verify -f` those are text from a
// manifest. A name must not be able to start a second line or send an
// escape sequence to a terminal.
func TestReadError_IsOneLineOnStderr(t *testing.T) {
	t.Parallel()
	hostile := deploymentTarget("web\x1b[2J\r\n::error::forged\u202e")
	var stderr bytes.Buffer
	report, err := Verify(context.Background(), Options{
		Client: fake.NewSimpleClientset(), Targets: []Target{hostile}, Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, Error: 1}); got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
	out := stderr.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Errorf("stderr must be one line:\n%q", out)
	}
	if strings.ContainsAny(out, "\x1b\r\u202e") {
		t.Errorf("stderr has a control or a format character:\n%q", out)
	}
	if !strings.HasPrefix(out, "verify error: Deployment/shop web") {
		t.Errorf("stderr = %q, want the line of the read error", out)
	}
	// The report keeps the name as it is: its renderers make it safe.
	if got := report.Spec.Results[0].Target.Name; got != hostile.Ref.Name {
		t.Errorf("the name in the report = %q, want it as it was given", got)
	}
}
