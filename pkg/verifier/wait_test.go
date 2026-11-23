// Tests for the wait loop, the readiness check, and the pod template
// verdict.
//
// The wait tests use a fake clock that moves only when the loop sleeps. A
// hook on that clock changes the fake cluster between two rounds, which is
// how a rollout, a crash loop, and a replaced pod are played. No test here
// sleeps for real.

package verifier

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// fakeClock is a Clock that moves only in Sleep. onSleep runs after each
// sleep with the number of sleeps so far, so "after the second sleep" is
// "just before round 3". The count is an int32 because the tests use it as
// a restart count, which is an int32 in the pod status.
type fakeClock struct {
	now     time.Time
	sleeps  int32
	slept   []time.Duration
	onSleep func(n int32)
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) {
	c.now = c.now.Add(d)
	c.sleeps++
	c.slept = append(c.slept, d)
	if c.onSleep != nil {
		c.onSleep(c.sleeps)
	}
}

// readyPod is runningPod with the condition Ready=True.
func readyPod(ns, name string, labels map[string]string, rcn *string) *corev1.Pod {
	pod := runningPod(ns, name, labels, rcn)
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	return pod
}

// crashingPod is a pod with the class whose container is in a crash loop.
func crashingPod(ns, name string, labels map[string]string, restartCount int32) *corev1.Pod {
	pod := runningPod(ns, name, labels, strPtr("gvisor"))
	pod.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady",
	}}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "app",
		RestartCount: restartCount,
		State:        corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	return pod
}

// setPod replaces a pod in the fake cluster.
func setPod(t *testing.T, client *fake.Clientset, pod *corev1.Pod) {
	t.Helper()
	if _, err := client.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update pod %s: %v", pod.Name, err)
	}
}

func deletePod(t *testing.T, client *fake.Clientset, ns, name string) {
	t.Helper()
	if err := client.CoreV1().Pods(ns).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod %s: %v", name, err)
	}
}

func createPod(t *testing.T, client *fake.Clientset, pod *corev1.Pod) {
	t.Helper()
	if _, err := client.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod %s: %v", pod.Name, err)
	}
}

// waitOptions is Options for a wait of d with the fake clock.
func waitOptions(client *fake.Clientset, plan *schema.MigrationPlan, clock *fakeClock, d time.Duration) Options {
	return Options{Client: client, Plan: plan, Wait: d, Clock: clock, Stderr: &bytes.Buffer{}}
}

var webLabels = map[string]string{"app": "web"}

// ---------------------------------------------------------------------------
// The wait loop.
// ---------------------------------------------------------------------------

// TestWait_Rollout: the case the flag is for. Verify starts directly after
// an apply: the old pod has no class. The rollout replaces it in the second
// interval. The step is ok when the new pod was Ready for the settle time.
func TestWait_Rollout(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		deployment("ns", "web", webLabels),
		readyPod("ns", "web-old", webLabels, nil),
	)
	clock := newFakeClock()
	clock.onSleep = func(n int32) {
		if n == 2 { // just before round 3, at t=10s
			deletePod(t, client, "ns", "web-old")
			createPod(t, client, readyPod("ns", "web-new", webLabels, strPtr("gvisor")))
		}
	}
	stderr := &bytes.Buffer{}
	opts := waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), clock, 3*time.Minute)
	opts.Stderr = stderr

	report, err := Verify(context.Background(), opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusOK {
		t.Fatalf("status = %q (%s), want ok", r.Status, r.Message)
	}
	if r.Readiness == nil || !r.Readiness.Stable || r.Readiness.Pods != 1 || r.Readiness.Ready != 1 {
		t.Errorf("readiness = %+v, want 1 of 1 Ready and stable", r.Readiness)
	}
	// Rounds at 0s and 5s see the old pod. Round 3 (10s) sees the new
	// pod Ready for the first time. Rounds 4 (15s) and 5 (20s): Ready
	// for 5s, then for 10s, which is the settle time.
	want := schema.VerifyWait{RequestedSeconds: 180, WaitedSeconds: 20, Rounds: 5}
	if report.Metadata.Wait == nil || *report.Metadata.Wait != want {
		t.Errorf("metadata.wait = %+v, want %+v", report.Metadata.Wait, want)
	}
	if !strings.Contains(stderr.String(), "verify: waiting: 0 of 1 step(s) ok, 3m0s left") {
		t.Errorf("stderr must have a progress line from before the rollout was done:\n%s", stderr.String())
	}
}

// TestWait_ReadyThenNotReadyIsNotOK: a container with no readiness probe is
// Ready from its start until it stops. A pod in a crash loop is therefore
// Ready in some rounds. One such round must not make the step ok.
func TestWait_ReadyThenNotReadyIsNotOK(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		statefulSet("ns", "web", webLabels),
		crashingPod("ns", "web-0", webLabels, 1),
	)
	clock := newFakeClock()
	clock.onSleep = func(n int32) {
		// Odd sleeps: the container is up, and Ready. Even sleeps: it
		// stopped again, with one more restart.
		if n%2 == 1 {
			pod := readyPod("ns", "web-0", webLabels, strPtr("gvisor"))
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: n}}
			setPod(t, client, pod)
			return
		}
		setPod(t, client, crashingPod("ns", "web-0", webLabels, n))
	}

	report, err := Verify(context.Background(),
		waitOptions(client, makePlan(stepRef("StatefulSet", "ns", "web", "gvisor")), clock, time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusMismatch {
		t.Fatalf("status = %q (%s), want mismatch: the pod was never Ready in two looks that are 10s apart", r.Status, r.Message)
	}
	if w := report.Metadata.Wait; w == nil || !w.TimedOut || w.RequestedSeconds != 60 || w.WaitedSeconds != 60 || w.Rounds != 13 {
		t.Errorf("metadata.wait = %+v, want a timeout after 60s and 13 rounds", w)
	}
	// The last round (sleep 12, an even one) saw the crash loop. The
	// report says so, with the reason that the kubelet gives.
	if want := "pod web-0 is not Ready: container app is waiting: CrashLoopBackOff, 12 restart(s)"; r.Message != want {
		t.Errorf("message = %q\nwant      %q", r.Message, want)
	}
	if r.Readiness == nil || r.Readiness.Stable || r.Readiness.Ready != 0 || len(r.Readiness.NotReady) != 1 {
		t.Errorf("readiness = %+v, want 0 Ready, not stable, one name", r.Readiness)
	}
}

// TestWait_RestartStartsTheCountAgain: the pod is Ready in every round, and
// its container started again between each two rounds. "Ready in two looks"
// is not "stayed up".
func TestWait_RestartStartsTheCountAgain(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		deployment("ns", "web", webLabels),
		readyPod("ns", "web-1", webLabels, strPtr("gvisor")),
	)
	clock := newFakeClock()
	clock.onSleep = func(n int32) {
		pod := readyPod("ns", "web-1", webLabels, strPtr("gvisor"))
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: n}}
		setPod(t, client, pod)
	}
	report, err := Verify(context.Background(),
		waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), clock, 30*time.Second))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusMismatch || !report.Metadata.Wait.TimedOut {
		t.Fatalf("status = %q, timedOut = %v; want mismatch after a timeout", r.Status, report.Metadata.Wait.TimedOut)
	}
	if want := "the pods are Ready, but only for 0s; with --wait they must stay Ready for 10s with no restart"; r.Message != want {
		t.Errorf("message = %q\nwant      %q", r.Message, want)
	}
	if r.Readiness.Stable || r.Readiness.Ready != 1 {
		t.Errorf("readiness = %+v, want Ready and not stable", r.Readiness)
	}
}

// TestWait_ReplacedPodStartsTheCountAgain: a StatefulSet pod that is
// replaced keeps its name. The UID tells the two apart.
func TestWait_ReplacedPodStartsTheCountAgain(t *testing.T) {
	t.Parallel()
	pod := func(uid string) *corev1.Pod {
		p := readyPod("ns", "web-0", webLabels, strPtr("gvisor"))
		p.UID = types.UID(uid)
		return p
	}
	client := fake.NewSimpleClientset(statefulSet("ns", "web", webLabels), pod("first"))
	clock := newFakeClock()
	clock.onSleep = func(n int32) {
		if n == 2 { // replaced just before round 3
			setPod(t, client, pod("second"))
		}
	}
	report, err := Verify(context.Background(),
		waitOptions(client, makePlan(stepRef("StatefulSet", "ns", "web", "gvisor")), clock, time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Without the replacement the step is ok in round 3 (10s). With it,
	// the count starts again at 10s and ends at 20s: round 5.
	if w := report.Metadata.Wait; w.Rounds != 5 || w.WaitedSeconds != 20 || w.TimedOut {
		t.Errorf("metadata.wait = %+v, want 5 rounds and 20s", w)
	}
	if report.Spec.Results[0].Status != schema.VerifyStatusOK {
		t.Errorf("status = %q, want ok", report.Spec.Results[0].Status)
	}
}

// TestWait_AlreadyInTheWantedState: nothing to wait for. The loop still
// needs two looks that are the settle time apart.
func TestWait_AlreadyInTheWantedState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		wait       time.Duration
		wantRounds int
		wantWaited int
	}{
		{"a long wait ends after the settle time", 3 * time.Minute, 3, 10},
		{"a wait that is the settle time", 10 * time.Second, 3, 10},
		// The settle time is never more than the wait time: a short
		// wait on a good cluster must be able to pass.
		{"a wait that is shorter than the settle time", 5 * time.Second, 2, 5},
		{"a wait that is shorter than one interval", 2 * time.Second, 2, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := fake.NewSimpleClientset(
				deployment("ns", "web", webLabels),
				readyPod("ns", "web-1", webLabels, strPtr("gvisor")),
				readyPod("ns", "web-2", webLabels, strPtr("gvisor")),
			)
			report, err := Verify(context.Background(),
				waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), newFakeClock(), tc.wait))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got := report.Spec.Summary; got.OK != 1 || got.Total != 1 {
				t.Errorf("summary = %+v, want 1 ok (%s)", got, report.Spec.Results[0].Message)
			}
			w := report.Metadata.Wait
			if w.Rounds != tc.wantRounds || w.WaitedSeconds != tc.wantWaited || w.TimedOut {
				t.Errorf("metadata.wait = %+v, want %d rounds, %ds, no timeout", w, tc.wantRounds, tc.wantWaited)
			}
		})
	}
}

// TestWait_OKStepsAreNotReadAgain: two steps. One is ok after the settle
// time, the other never. After that the loop reads only the one that is
// not ok.
func TestWait_OKStepsAreNotReadAgain(t *testing.T) {
	t.Parallel()
	apiLabels := map[string]string{"app": "api"}
	client := fake.NewSimpleClientset(
		deployment("ns", "web", webLabels),
		readyPod("ns", "web-1", webLabels, strPtr("gvisor")),
		deployment("ns", "api", apiLabels),
		readyPod("ns", "api-old", apiLabels, nil),
	)
	gets := map[string]int{}
	client.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		gets[action.(k8stesting.GetAction).GetName()]++
		return false, nil, nil
	})
	plan := makePlan(stepRef("Deployment", "ns", "web", "gvisor"), stepRef("Deployment", "ns", "api", "gvisor"))

	report, err := Verify(context.Background(), waitOptions(client, plan, newFakeClock(), 30*time.Second))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	// Rounds at 0, 5, 10, 15, 20, 25, 30 seconds: 7. "web" is final in
	// round 3.
	if gets["web"] != 3 || gets["api"] != 7 {
		t.Errorf("reads: web %d, api %d; want 3 and 7", gets["web"], gets["api"])
	}
	if got := report.Spec.Summary; got.OK != 1 || got.Mismatch != 1 {
		t.Errorf("summary = %+v, want 1 ok and 1 mismatch", got)
	}
	// The results are in plan order, whatever round each one is from.
	if report.Spec.Results[0].Target.Name != "web" || report.Spec.Results[1].Target.Name != "api" {
		t.Errorf("results are not in plan order: %+v", report.Spec.Results)
	}
	if !report.Metadata.Wait.TimedOut {
		t.Errorf("want a timeout")
	}
}

// TestWait_NoPodsNowDoesNotUseTheWaitTime: a CronJob between two runs and a
// Deployment at replicas: 0 have no pods and will have none. If the loop
// waited for pods, every plan with such a step would use the full wait time
// and fail, and the pipeline would roll back a correct migration.
func TestWait_NoPodsNowDoesNotUseTheWaitTime(t *testing.T) {
	t.Parallel()
	zero := int32(0)
	scaledDown := deployment("ns", "web", webLabels)
	scaledDown.Spec.Replicas = &zero
	scaledDown.Spec.Template.Spec.RuntimeClassName = strPtr("gvisor")
	client := fake.NewSimpleClientset(scaledDown, cronJob("ns", "report", map[string]string{"app": "report"}, strPtr("gvisor")))
	plan := makePlan(stepRef("Deployment", "ns", "web", "gvisor"), stepRef("CronJob", "ns", "report", "gvisor"))
	clock := newFakeClock()

	report, err := Verify(context.Background(), waitOptions(client, plan, clock, 3*time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got := report.Spec.Summary; got.OK != 2 {
		t.Fatalf("summary = %+v, want 2 ok: %+v", got, report.Spec.Results)
	}
	for _, r := range report.Spec.Results {
		if r.Basis != schema.VerifyBasisTemplate || r.Readiness != nil {
			t.Errorf("%s: basis = %q, readiness = %+v; want the template and no readiness", r.Target.Name, r.Basis, r.Readiness)
		}
	}
	if w := report.Metadata.Wait; w.Rounds != 1 || w.WaitedSeconds != 0 || clock.sleeps != 0 {
		t.Errorf("metadata.wait = %+v after %d sleeps; want one round and no sleep", w, clock.sleeps)
	}
}

// TestWait_ReadErrorIsPrintedOneTime: a workload that is not there gives
// the same error in every round. stderr gets one line, not one for each
// round.
func TestWait_ReadErrorIsPrintedOneTime(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	opts := waitOptions(fake.NewSimpleClientset(), makePlan(stepRef("Deployment", "ns", "gone", "gvisor")), newFakeClock(), 20*time.Second)
	opts.Stderr = stderr
	report, err := Verify(context.Background(), opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Metadata.Wait.Rounds != 5 || report.Spec.Results[0].Status != schema.VerifyStatusError {
		t.Fatalf("rounds = %d, status = %q; want 5 rounds and an error", report.Metadata.Wait.Rounds, report.Spec.Results[0].Status)
	}
	if n := strings.Count(stderr.String(), "verify error: Deployment/ns gone: "); n != 1 {
		t.Errorf("the read error is on stderr %d time(s), want 1:\n%s", n, stderr.String())
	}
}

// TestWait_TimeoutKeepsTheReason: at the end of the wait time the report
// has the last look at each step, with its reason. The wait time is not a
// deadline on the API calls.
func TestWait_TimeoutKeepsTheReason(t *testing.T) {
	t.Parallel()
	pending := runningPod("ns", "web-1", webLabels, strPtr("gvisor"))
	pending.Status.Phase = corev1.PodPending
	pending.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable",
		Message: "0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector.",
	}}
	client := fake.NewSimpleClientset(deployment("ns", "web", webLabels), pending)
	lists := 0
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		return false, nil, nil
	})

	report, err := Verify(context.Background(),
		waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), newFakeClock(), 12*time.Second))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	want := "pod web-1 is not Ready: not scheduled (Unschedulable): 0/3 nodes are available: 3 node(s) didn't match Pod's node affinity/selector."
	if r.Status != schema.VerifyStatusMismatch || r.Message != want {
		t.Errorf("status = %q, message = %q\nwant mismatch and %q", r.Status, r.Message, want)
	}
	// Rounds at 0, 5, 10, and a last one at the end of the wait time: 12.
	if w := report.Metadata.Wait; !w.TimedOut || w.Rounds != 4 || w.WaitedSeconds != 12 || lists != 4 {
		t.Errorf("metadata.wait = %+v with %d pod lists; want a timeout, 4 rounds, the last one at 12s", w, lists)
	}
}

// TestWait_CancelKeepsTheLastCompleteRound: the caller cancels in the middle
// of round 2. Round 2 is dropped; the report is round 1.
func TestWait_CancelKeepsTheLastCompleteRound(t *testing.T) {
	t.Parallel()
	apiLabels := map[string]string{"app": "api"}
	client := fake.NewSimpleClientset(
		deployment("ns", "web", webLabels),
		crashingPod("ns", "web-1", webLabels, 3),
		deployment("ns", "api", apiLabels),
		crashingPod("ns", "api-1", apiLabels, 3),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lists := 0
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		lists++
		if lists == 3 { // round 2, first step
			cancel()
			return true, nil, context.Canceled
		}
		return false, nil, nil
	})
	plan := makePlan(stepRef("Deployment", "ns", "web", "gvisor"), stepRef("Deployment", "ns", "api", "gvisor"))
	stderr := &bytes.Buffer{}
	opts := waitOptions(client, plan, newFakeClock(), time.Minute)
	opts.Stderr = stderr

	report, err := Verify(ctx, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if w := report.Metadata.Wait; !w.Interrupted || w.TimedOut || w.Rounds != 1 {
		t.Errorf("metadata.wait = %+v, want interrupted after 1 complete round", w)
	}
	// The round that was cut is dropped. Its reads failed because of the
	// cancel, and that is not news: no line for them on stderr.
	if strings.Contains(stderr.String(), "verify error") || strings.Contains(stderr.String(), "context canceled") {
		t.Errorf("stderr has lines from the round that was dropped:\n%s", stderr.String())
	}
	for _, r := range report.Spec.Results {
		if r.Status != schema.VerifyStatusMismatch || !strings.Contains(r.Message, "CrashLoopBackOff") {
			t.Errorf("%s: status = %q, message = %q; want the verdict of round 1, not a context error", r.Target.Name, r.Status, r.Message)
		}
	}
}

// TestWait_CancelInTheFirstRoundIsAnError: there is no complete round to
// report.
func TestWait_CancelInTheFirstRoundIsAnError(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(deployment("ns", "web", webLabels), readyPod("ns", "web-1", webLabels, strPtr("gvisor")))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Verify(ctx, waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), newFakeClock(), time.Minute))
	if err == nil || !strings.Contains(err.Error(), "canceled before the first look") {
		t.Errorf("err = %v, want an error about the first look", err)
	}
}

// TestWait_CancelInASleep: the same as a cancel in a round, between rounds.
func TestWait_CancelInASleep(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(deployment("ns", "web", webLabels), readyPod("ns", "web-old", webLabels, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := newFakeClock()
	clock.onSleep = func(int32) { cancel() }
	report, err := Verify(ctx, waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), clock, time.Minute))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if w := report.Metadata.Wait; !w.Interrupted || w.Rounds != 1 {
		t.Errorf("metadata.wait = %+v, want interrupted after 1 round", w)
	}
	if report.Spec.Results[0].Status != schema.VerifyStatusMismatch {
		t.Errorf("status = %q, want the mismatch of round 1", report.Spec.Results[0].Status)
	}
}

func TestWait_SleepsAreTheIntervalOrWhatIsLeft(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(deployment("ns", "web", webLabels), readyPod("ns", "web-old", webLabels, nil))
	clock := newFakeClock()
	opts := waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), clock, 7*time.Second)
	opts.PollInterval = 3 * time.Second
	if _, err := Verify(context.Background(), opts); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got, want := fmt.Sprint(clock.slept), "[3s 3s 1s]"; got != want {
		t.Errorf("sleeps = %s, want %s", got, want)
	}
}

func TestWait_NegativeIsRejected(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	_, err := Verify(context.Background(), Options{Client: client, Plan: makePlan(), Wait: -time.Second})
	if err == nil || !strings.Contains(err.Error(), "must be zero or more") {
		t.Errorf("err = %v, want an error about the wait time", err)
	}
}

// TestNoWait_DoesNotReadReadiness: with no wait time, a pod that is not
// Ready is not a failure, and the report has no readiness block. This is
// the behavior before the flag, and it must stay.
func TestNoWait_DoesNotReadReadiness(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(deployment("ns", "web", webLabels), crashingPod("ns", "web-1", webLabels, 5))
	report, err := Verify(context.Background(), Options{Client: client, Plan: makePlan(stepRef("Deployment", "ns", "web", "gvisor"))})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusOK || r.Readiness != nil || report.Metadata.Wait != nil {
		t.Errorf("status = %q, readiness = %+v, wait = %+v; want ok and no readiness or wait block", r.Status, r.Readiness, report.Metadata.Wait)
	}
}

// ---------------------------------------------------------------------------
// The wait and the in-pod probe.
// ---------------------------------------------------------------------------

// TestWait_Probe: with a wait time the probe runs when the pods have
// settled, and one time for a set of pods. A probe in each round would be
// an exec call into the workload every few seconds.
func TestWait_Probe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		pod         *corev1.Pod
		exec        *fakeExec
		wantStatus  schema.VerifyStatus
		wantMessage string
		wantCalls   int
		wantRounds  int
	}{
		{
			name:       "markers found: one exec call, and the step is final",
			pod:        readyPod("ns", "web-1", webLabels, strPtr("gvisor")),
			exec:       &fakeExec{stdout: []byte("Starting gVisor...")},
			wantStatus: schema.VerifyStatusOK, wantMessage: "probe confirmed gVisor markers",
			wantCalls: 1, wantRounds: 3,
		},
		{
			// "No markers" is an answer. It cannot change while the
			// pods are the same, so the loop does not ask again.
			name:       "no markers: one exec call, and a mismatch at the end of the wait",
			pod:        readyPod("ns", "web-1", webLabels, strPtr("gvisor")),
			exec:       &fakeExec{stdout: []byte("Linux version 6.8")},
			wantStatus: schema.VerifyStatusMismatch, wantMessage: "probe found no gVisor markers",
			wantCalls: 1, wantRounds: 7,
		},
		{
			// An exec call that failed is not an answer about gVisor.
			// Rounds at 10, 15, 20, 25, and 30 seconds try again.
			name:       "exec error: tried again in each round after the pods settled",
			pod:        readyPod("ns", "web-1", webLabels, strPtr("gvisor")),
			exec:       &fakeExec{err: errors.New("connection reset")},
			wantStatus: schema.VerifyStatusError, wantMessage: "probe exec failed: connection reset",
			wantCalls: 5, wantRounds: 7,
		},
		{
			name:        "pods that do not settle: no exec call",
			pod:         crashingPod("ns", "web-1", webLabels, 2),
			exec:        &fakeExec{stdout: []byte("Starting gVisor...")},
			wantStatus:  schema.VerifyStatusMismatch,
			wantMessage: "pod web-1 is not Ready: container app is waiting: CrashLoopBackOff, 2 restart(s)",
			wantCalls:   0, wantRounds: 7,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := fake.NewSimpleClientset(deployment("ns", "web", webLabels), tc.pod)
			opts := waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), newFakeClock(), 30*time.Second)
			opts.InPodProbe = true
			opts.Exec = tc.exec
			report, err := Verify(context.Background(), opts)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			r := report.Spec.Results[0]
			if r.Status != tc.wantStatus || r.Message != tc.wantMessage {
				t.Errorf("status = %q, message = %q; want %q and %q", r.Status, r.Message, tc.wantStatus, tc.wantMessage)
			}
			if tc.exec.callCount != tc.wantCalls || report.Metadata.Wait.Rounds != tc.wantRounds {
				t.Errorf("%d exec call(s) in %d round(s); want %d in %d", tc.exec.callCount, report.Metadata.Wait.Rounds, tc.wantCalls, tc.wantRounds)
			}
			if tc.wantCalls > 0 && (r.Probe == nil || r.Readiness == nil || !r.Readiness.Stable) {
				t.Errorf("a step that was probed must have the probe result and stable pods: probe %+v, readiness %+v", r.Probe, r.Readiness)
			}
		})
	}
}

// TestWait_ProbeRunsAgainForNewPods: the answer of the probe is for a set
// of pods. When a pod is replaced, the count starts again and so does the
// probe.
func TestWait_ProbeRunsAgainForNewPods(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(deployment("ns", "web", webLabels), readyPod("ns", "web-1", webLabels, strPtr("gvisor")))
	exec := &fakeExec{stdout: []byte("Linux version 6.8")}
	clock := newFakeClock()
	clock.onSleep = func(n int32) {
		if n == 4 { // just before round 5, at t=20s
			deletePod(t, client, "ns", "web-1")
			createPod(t, client, readyPod("ns", "web-2", webLabels, strPtr("gvisor")))
			exec.stdout = []byte("Starting gVisor...")
		}
	}
	opts := waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), clock, time.Minute)
	opts.InPodProbe = true
	opts.Exec = exec
	report, err := Verify(context.Background(), opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	r := report.Spec.Results[0]
	// web-1 settles at 10s: probe 1, no markers. web-2 is first seen at
	// 20s and settles at 30s: probe 2, markers.
	if r.Status != schema.VerifyStatusOK || exec.callCount != 2 || exec.lastPod != "web-2" || report.Metadata.Wait.WaitedSeconds != 30 {
		t.Errorf("status = %q after %d exec call(s), last pod %q, waited %ds; want ok, 2 calls, web-2, 30s",
			r.Status, exec.callCount, exec.lastPod, report.Metadata.Wait.WaitedSeconds)
	}
}

// ---------------------------------------------------------------------------
// A rolling update that does not go on.
// ---------------------------------------------------------------------------

// TestWait_StuckRolloutNamesTheNewPod: a rolling update keeps the old pods
// until the new pods are Ready. When the new pods do not start in gVisor,
// the class check alone reports an old pod, which does not tell the operator
// what is wrong. With a wait time the result also has the reason of the new
// pod.
func TestWait_StuckRolloutNamesTheNewPod(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		newPod        *corev1.Pod
		wait          time.Duration
		wantMessage   string
		wantReadiness *schema.PodReadiness
	}{
		{
			name:   "the new pod is in a crash loop",
			newPod: crashingPod("ns", "web-new", webLabels, 3),
			wait:   10 * time.Second,
			wantMessage: `pod web-old has runtimeClassName="", expected "gvisor"; ` +
				`and pod web-new is not Ready: container app is waiting: CrashLoopBackOff, 3 restart(s)`,
			wantReadiness: &schema.PodReadiness{
				Pods: 1, Ready: 0, NotReady: []string{"web-new"},
				Message: "pod web-new is not Ready: container app is waiting: CrashLoopBackOff, 3 restart(s)",
			},
		},
		{
			// The new pod is Ready: the rollout is in progress, and
			// there is nothing to add.
			name:        "the new pod is Ready",
			newPod:      readyPod("ns", "web-new", webLabels, strPtr("gvisor")),
			wait:        10 * time.Second,
			wantMessage: `pod web-old has runtimeClassName="", expected "gvisor"`,
		},
		{
			// With no wait time the verifier does not read readiness.
			name:        "no wait time",
			newPod:      crashingPod("ns", "web-new", webLabels, 3),
			wantMessage: `pod web-old has runtimeClassName="", expected "gvisor"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := fake.NewSimpleClientset(
				deployment("ns", "web", webLabels),
				readyPod("ns", "web-old", webLabels, nil),
				tc.newPod,
			)
			opts := waitOptions(client, makePlan(stepRef("Deployment", "ns", "web", "gvisor")), newFakeClock(), tc.wait)
			report, err := Verify(context.Background(), opts)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			r := report.Spec.Results[0]
			if r.Status != schema.VerifyStatusMismatch || r.Message != tc.wantMessage {
				t.Errorf("status = %q, message = %q\nwant mismatch and %q", r.Status, r.Message, tc.wantMessage)
			}
			if !reflect.DeepEqual(r.Readiness, tc.wantReadiness) {
				t.Errorf("readiness = %+v, want %+v", r.Readiness, tc.wantReadiness)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Node placement in a wait.
// ---------------------------------------------------------------------------

// TestPlacement_AReadErrorIsNotKept: with a wait time one placement checker
// serves every round. A read that failed one time (a network error) must
// not turn the check off for the rest of the wait. "Not found" is an answer
// and is kept.
func TestPlacement_AReadErrorIsNotKept(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset(
		&nodev1.RuntimeClass{
			ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "runsc",
			Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"runtime": "gvisor"}},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "plain-1"}},
	)
	reads := map[string]int{}
	fail := func(action k8stesting.Action) (bool, runtime.Object, error) {
		resource := action.GetResource().Resource
		reads[resource]++
		if reads[resource] == 1 {
			return true, nil, errors.New("connection reset")
		}
		return false, nil, nil
	}
	client.PrependReactor("get", "runtimeclasses", fail)
	client.PrependReactor("get", "nodes", fail)

	pod := readyPod("ns", "web-1", webLabels, strPtr("gvisor"))
	pod.Spec.NodeName = "plain-1"
	checker := newPlacementChecker(client)
	ctx := context.Background()

	// Look 1: the RuntimeClass read fails. Look 2: it works, and the node
	// read fails. Look 3: both work, and the pod is on a node that the
	// RuntimeClass does not select.
	for look, wantChecked := range []bool{false, false, true} {
		np := checker.check(ctx, "gvisor", []corev1.Pod{*pod})
		if np.Checked != wantChecked {
			t.Fatalf("look %d: checked = %v (%s), want %v", look+1, np.Checked, np.Message, wantChecked)
		}
		if wantChecked && len(np.Mismatched) != 1 {
			t.Errorf("look %d: mismatched = %v, want plain-1", look+1, np.Mismatched)
		}
	}
	// Look 4 uses the reads that worked.
	checker.check(ctx, "gvisor", []corev1.Pod{*pod})
	if reads["runtimeclasses"] != 2 || reads["nodes"] != 2 {
		t.Errorf("reads = %v, want 2 of each: one that failed and one that is kept", reads)
	}

	// "Not found" is kept: it is an answer.
	missing := newPlacementChecker(fake.NewSimpleClientset())
	missing.check(ctx, "gvisor", []corev1.Pod{*pod})
	if _, kept := missing.rcs["gvisor"]; !kept {
		t.Errorf("a RuntimeClass that is not found must be kept")
	}
}

// ---------------------------------------------------------------------------
// The pod template verdict.
// ---------------------------------------------------------------------------

// cronJob returns a CronJob whose pod template has the given labels and
// runtimeClassName.
func cronJob(ns, name string, labels map[string]string, rcn *string) *batchv1.CronJob {
	return &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{RuntimeClassName: rcn},
			},
		}}},
	}
}

func TestVerify_TemplateVerdict(t *testing.T) {
	t.Parallel()
	zero, two := int32(0), int32(2)
	withClass := corev1.PodSpec{RuntimeClassName: strPtr("gvisor")}
	labels := map[string]string{"app": "x"}
	selector := &metav1.LabelSelector{MatchLabels: labels}

	dep := func(replicas *int32, spec corev1.PodSpec) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "x"},
			Spec:       appsv1.DeploymentSpec{Replicas: replicas, Selector: selector, Template: corev1.PodTemplateSpec{Spec: spec}},
		}
	}
	daemonSet := func(generation, observed int64, desired int32) *appsv1.DaemonSet {
		return &appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "x", Generation: generation},
			Spec:       appsv1.DaemonSetSpec{Selector: selector, Template: corev1.PodTemplateSpec{Spec: withClass}},
			Status:     appsv1.DaemonSetStatus{ObservedGeneration: observed, DesiredNumberScheduled: desired},
		}
	}
	job := func(conditions ...batchv1.JobCondition) *batchv1.Job {
		return &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "x"},
			Spec:       batchv1.JobSpec{Selector: selector, Template: corev1.PodTemplateSpec{Spec: withClass}},
			Status:     batchv1.JobStatus{Conditions: conditions},
		}
	}
	earlier := readyPod("ns", "x-earlier", labels, nil)
	current := readyPod("ns", "x-current", labels, strPtr("gvisor"))

	tests := []struct {
		name        string
		kind        string
		objects     []runtime.Object
		wantStatus  schema.VerifyStatus
		wantBasis   string
		wantActual  string
		wantMessage string
	}{
		{
			name: "Deployment at replicas 0, the template has the class",
			kind: "Deployment", objects: []runtime.Object{dep(&zero, withClass)},
			wantStatus: schema.VerifyStatusOK, wantBasis: "template", wantActual: "gvisor",
			wantMessage: `no pods to check (replicas: 0); the pod template has runtimeClassName="gvisor"`,
		},
		{
			name: "Deployment at replicas 0, the template does not have the class",
			kind: "Deployment", objects: []runtime.Object{dep(&zero, corev1.PodSpec{})},
			wantStatus: schema.VerifyStatusMismatch, wantBasis: "template", wantActual: "",
			wantMessage: `the pod template has runtimeClassName="", expected "gvisor"`,
		},
		{
			// The controller wants pods and has none: a rollout, or a
			// problem. The template is not evidence of what runs.
			name: "Deployment that wants 2 pods and has none",
			kind: "Deployment", objects: []runtime.Object{dep(&two, withClass)},
			wantStatus:  schema.VerifyStatusError,
			wantMessage: "no pods matched controller selector for Deployment/ns x",
		},
		{
			name: "Deployment with no replica count wants one pod",
			kind: "Deployment", objects: []runtime.Object{dep(nil, withClass)},
			wantStatus:  schema.VerifyStatusError,
			wantMessage: "no pods matched controller selector for Deployment/ns x",
		},
		{
			name: "DaemonSet that selects no node",
			kind: "DaemonSet", objects: []runtime.Object{daemonSet(3, 3, 0)},
			wantStatus: schema.VerifyStatusOK, wantBasis: "template", wantActual: "gvisor",
			wantMessage: `no pods to check (the DaemonSet selects no node); the pod template has runtimeClassName="gvisor"`,
		},
		{
			// The status is of the spec before the change. Its zero is
			// not "selects no node" yet.
			name: "DaemonSet whose status is not current",
			kind: "DaemonSet", objects: []runtime.Object{daemonSet(4, 3, 0)},
			wantStatus:  schema.VerifyStatusError,
			wantMessage: "no pods matched controller selector for DaemonSet/ns x",
		},
		{
			name: "Job that is complete",
			kind: "Job", objects: []runtime.Object{job(batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})},
			wantStatus: schema.VerifyStatusOK, wantBasis: "template", wantActual: "gvisor",
			wantMessage: `no pods to check (the Job is finished); the pod template has runtimeClassName="gvisor"`,
		},
		{
			name: "Job that is new and has no pod yet",
			kind: "Job", objects: []runtime.Object{job()},
			wantStatus:  schema.VerifyStatusError,
			wantMessage: "no pods matched controller selector for Job/ns x",
		},
		{
			name: "CronJob between two runs",
			kind: "CronJob", objects: []runtime.Object{cronJob("ns", "x", labels, strPtr("gvisor"))},
			wantStatus: schema.VerifyStatusOK, wantBasis: "template", wantActual: "gvisor",
			wantMessage: `no pods to check (no Job of the CronJob runs now); the pod template has runtimeClassName="gvisor"`,
		},
		{
			name: "CronJob whose template does not have the class",
			kind: "CronJob", objects: []runtime.Object{cronJob("ns", "x", labels, nil), current},
			wantStatus: schema.VerifyStatusMismatch, wantBasis: "template", wantActual: "",
			wantMessage: `the pod template has runtimeClassName="", expected "gvisor"`,
		},
		{
			// A Job from before the change keeps its template until it
			// ends. Its pod is not held against the plan.
			name: "CronJob with a pod of a Job from before the change",
			kind: "CronJob", objects: []runtime.Object{cronJob("ns", "x", labels, strPtr("gvisor")), earlier},
			wantStatus: schema.VerifyStatusOK, wantBasis: "template", wantActual: "gvisor",
			wantMessage: `the pod template has runtimeClassName="gvisor"; 1 pod(s) of a Job from before the change do not have runtimeClassName="gvisor"; they end with that Job`,
		},
		{
			name: "CronJob with a pod from before and a pod from after the change",
			kind: "CronJob", objects: []runtime.Object{cronJob("ns", "x", labels, strPtr("gvisor")), earlier, current},
			wantStatus: schema.VerifyStatusOK, wantBasis: "", wantActual: "gvisor",
			wantMessage: `1 pod(s) of a Job from before the change do not have runtimeClassName="gvisor"; they end with that Job`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			client := fake.NewSimpleClientset(tc.objects...)
			report, err := Verify(context.Background(), Options{
				Client: client, Plan: makePlan(stepRef(tc.kind, "ns", "x", "gvisor")), Stderr: &bytes.Buffer{},
			})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			r := report.Spec.Results[0]
			if r.Status != tc.wantStatus || r.Basis != tc.wantBasis || r.Actual != tc.wantActual || r.Message != tc.wantMessage {
				t.Errorf("got  status %q, basis %q, actual %q, message %q\nwant status %q, basis %q, actual %q, message %q",
					r.Status, r.Basis, r.Actual, r.Message, tc.wantStatus, tc.wantBasis, tc.wantActual, tc.wantMessage)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Readiness.
// ---------------------------------------------------------------------------

func TestNotReadyReason(t *testing.T) {
	t.Parallel()
	pod := func(change func(*corev1.Pod)) corev1.Pod {
		p := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p"}, Status: corev1.PodStatus{Phase: corev1.PodRunning}}
		change(&p)
		return p
	}
	waiting := func(name, reason string, restartCount int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, RestartCount: restartCount,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
	}
	tests := []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{"a crash loop", pod(func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("app", "CrashLoopBackOff", 4)}
		}), "container app is waiting: CrashLoopBackOff, 4 restart(s)"},
		{"an image that cannot be pulled", pod(func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("app", "ImagePullBackOff", 0)}
		}), "container app is waiting: ImagePullBackOff"},
		{"an init container comes first", pod(func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{waiting("setup", "CreateContainerError", 0)}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{waiting("app", "PodInitializing", 0)}
		}), "container setup is waiting: CreateContainerError"},
		{"a container that stopped with an error", pod(func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 139}}}}
		}), "container app stopped: Error, exit code 139, 1 restart(s)"},
		{"a pod that is not scheduled", pod(func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: "Unschedulable", Message: "0/1 nodes are available:\n 1 node(s) had untolerated taint."}}
		}), "not scheduled (Unschedulable): 0/1 nodes are available: 1 node(s) had untolerated taint."},
		{"a readiness probe that fails", pod(func(p *corev1.Pod) {
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse,
				Reason: "ContainersNotReady", Message: "containers with unready status: [app]"}}
		}), "ContainersNotReady: containers with unready status: [app]"},
		{"nothing to go on", pod(func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), "phase Pending"},
		{"a long scheduler message is cut", pod(func(p *corev1.Pod) {
			p.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
				Reason: "Unschedulable", Message: strings.Repeat("node-x is not ok. ", 40)}}
		}), ""},
	}
	for _, tc := range tests {
		got := notReadyReason(tc.pod)
		if tc.want == "" {
			if n := len([]rune(got)); n != maxReasonLen || !strings.HasSuffix(got, "…") {
				t.Errorf("%s: the reason is %d characters, want %d with the cut marked", tc.name, n, maxReasonLen)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s:\ngot  %q\nwant %q", tc.name, got, tc.want)
		}
	}
}

func TestPodReadiness(t *testing.T) {
	t.Parallel()
	var pods []corev1.Pod
	pods = append(pods, *readyPod("ns", "web-a", webLabels, strPtr("gvisor")))
	// Twelve pods that are not Ready, given out of order.
	for i := 12; i >= 1; i-- {
		pods = append(pods, *crashingPod("ns", fmt.Sprintf("web-%02d", i), webLabels, 2))
	}
	r := podReadiness(pods)
	if r.Pods != 13 || r.Ready != 1 || r.Stable {
		t.Errorf("readiness = %+v, want 13 pods, 1 Ready, not stable", r)
	}
	if len(r.NotReady) != maxNotReadyNames || r.NotReady[0] != "web-01" || r.NotReady[9] != "web-10" {
		t.Errorf("notReady = %v, want the first %d names, sorted", r.NotReady, maxNotReadyNames)
	}
	want := "pod web-01 is not Ready: container app is waiting: CrashLoopBackOff, 2 restart(s) (and 11 more pod(s) not Ready)"
	if r.Message != want {
		t.Errorf("message = %q\nwant      %q", r.Message, want)
	}

	all := podReadiness(pods[:1])
	if all.Pods != 1 || all.Ready != 1 || all.Message != "" || all.NotReady != nil {
		t.Errorf("readiness of one Ready pod = %+v", all)
	}
}

func TestPodsFingerprint(t *testing.T) {
	t.Parallel()
	a := readyPod("ns", "a", webLabels, strPtr("gvisor"))
	b := readyPod("ns", "b", webLabels, strPtr("gvisor"))
	base := podsFingerprint([]corev1.Pod{*a, *b})
	if got := podsFingerprint([]corev1.Pod{*b, *a}); got != base {
		t.Errorf("the order of the pods must not change the fingerprint")
	}
	restarted := a.DeepCopy()
	restarted.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "app", RestartCount: 1}}
	replaced := a.DeepCopy()
	replaced.UID = "another"
	for name, pods := range map[string][]corev1.Pod{
		"a restart":      {*restarted, *b},
		"a replaced pod": {*replaced, *b},
		"one pod less":   {*a},
	} {
		if podsFingerprint(pods) == base {
			t.Errorf("%s must change the fingerprint", name)
		}
	}
}
