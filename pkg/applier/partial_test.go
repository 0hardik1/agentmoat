// Tests for an apply that did not finish, and the rollback after it.
//
// Why this file exists
//
//	A pipeline runs `apply`, and when a later step fails it runs `rollback`
//	with the same plan. The rollback must undo what the apply did, also when
//	the apply ended in the middle: one patch failed (exit 3), the process was
//	stopped between two patches (a job that the CI system cancels), or the
//	last write (the namespace stamp) failed.
//
//	The namespace stamp `agentmoat.io/plan-hash` cannot carry that: it is
//	written only when every step in the namespace is done, because a stamp
//	means "do not apply again". So `apply` writes a second annotation,
//	`agentmoat.io/apply-started`, before its first patch in a namespace.
//	`rollback` acts where one of the two carries the hash of its plan.
//
//	The tests use the fake clientset with reactors that make one API call
//	fail, and check the objects after the run, not only the report.
package applier

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

// shopPlan is a plan with two Deployments in one namespace. One namespace is
// the point: the stamp is for a namespace, so one step that fails holds back
// the stamp for the step that worked.
func shopPlan() *schema.MigrationPlan {
	plan := schema.NewMigrationPlan()
	plan.Metadata = schema.PlanMetadata{PlanHash: "shop-hash"}
	plan.Spec = schema.PlanSpec{
		Options: schema.PlannerOptions{RuntimeClassName: "gvisor"},
		Steps: []schema.PlanStep{
			{
				Order:            1,
				Target:           schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "web"},
				Action:           "set-runtime-class",
				RuntimeClassName: "gvisor",
			},
			{
				Order:            2,
				Target:           schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "api"},
				Action:           "set-runtime-class",
				RuntimeClassName: "gvisor",
			},
		},
	}
	return plan
}

// shopClient has the namespace and the two Deployments of shopPlan.
func shopClient() *fake.Clientset {
	return fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"}},
	)
}

// failPatchOf makes every patch of the Deployment with this name fail.
func failPatchOf(client *fake.Clientset, name string) {
	client.PrependReactor("patch", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.PatchAction).GetName() == name {
			return true, nil, errors.New("admission webhook denied the request")
		}
		return false, nil, nil
	})
}

// failNamespacePatchWith makes a namespace patch fail when its body has the
// given text in it. A test uses it to fail one of the two annotation writes.
func failNamespacePatchWith(client *fake.Clientset, text string) {
	client.PrependReactor("patch", "namespaces", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(string(action.(clienttesting.PatchAction).GetPatch()), text) {
			return true, nil, errors.New("namespaces is forbidden")
		}
		return false, nil, nil
	})
}

// classOf returns the runtimeClassName of a Deployment's pod template, ""
// when the field is not set.
func classOf(t *testing.T, client *fake.Clientset, ns, name string) string {
	t.Helper()
	dep, err := client.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment %s/%s: %v", ns, name, err)
	}
	if dep.Spec.Template.Spec.RuntimeClassName == nil {
		return ""
	}
	return *dep.Spec.Template.Spec.RuntimeClassName
}

// setClass sets the runtimeClassName of a Deployment's pod template the way
// some other tool would: with no agentmoat annotation.
func setClass(t *testing.T, client *fake.Clientset, ns, name, class string) {
	t.Helper()
	dep, err := client.AppsV1().Deployments(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment %s/%s: %v", ns, name, err)
	}
	dep.Spec.Template.Spec.RuntimeClassName = &class
	if _, err := client.AppsV1().Deployments(ns).Update(context.Background(), dep, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update Deployment %s/%s: %v", ns, name, err)
	}
}

// annotationsOf returns the annotations of a namespace.
func annotationsOf(t *testing.T, client *fake.Clientset, ns string) map[string]string {
	t.Helper()
	got, err := client.CoreV1().Namespaces().Get(context.Background(), ns, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Namespace %s: %v", ns, err)
	}
	return got.Annotations
}

// statusOf returns the status of the step for the named workload.
func statusOf(t *testing.T, steps []schema.StepResult, name string) schema.StepStatus {
	t.Helper()
	for _, s := range steps {
		if s.Target.Name == name {
			return s.Status
		}
	}
	t.Fatalf("no step for %q in %+v", name, steps)
	return ""
}

// workloadPatches counts the patch calls on Deployments that the client saw.
func workloadPatches(client *fake.Clientset) int {
	n := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "deployments" {
			n++
		}
	}
	return n
}

// mustApply runs Apply and fails the test on an error (a step that fails is
// in the result, not an error).
func mustApply(t *testing.T, client *fake.Clientset, plan *schema.MigrationPlan, dryRun bool) *schema.ApplyResult {
	t.Helper()
	res, err := Apply(context.Background(), Options{Client: client, Plan: plan, DryRun: dryRun})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return res
}

// mustRollback runs Rollback and fails the test on an error.
func mustRollback(t *testing.T, client *fake.Clientset, plan *schema.MigrationPlan, dryRun bool) *schema.RollbackResult {
	t.Helper()
	res, err := Rollback(context.Background(), Options{Client: client, Plan: plan, DryRun: dryRun})
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	return res
}

// TestRollback_AfterAPartialApply: one patch of the apply fails. The
// rollback undoes the patch that worked.
func TestRollback_AfterAPartialApply(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	failPatchOf(client, "api")

	applied := mustApply(t, client, plan, false)
	if applied.Spec.Summary.Applied != 1 || applied.Spec.Summary.Failed != 1 {
		t.Fatalf("apply summary = %+v, want 1 applied and 1 failed", applied.Spec.Summary)
	}
	if got := classOf(t, client, "shop", "web"); got != "gvisor" {
		t.Fatalf("after the apply, web has class %q, want gvisor", got)
	}

	rolled := mustRollback(t, client, plan, false)

	if got := classOf(t, client, "shop", "web"); got != "" {
		t.Errorf("after the rollback, web has class %q; the rollback must undo a partial apply", got)
	}
	if got := statusOf(t, rolled.Spec.Steps, "web"); got != schema.StepStatusApplied {
		t.Errorf("rollback status of web = %q, want %q", got, schema.StepStatusApplied)
	}
	if got := statusOf(t, rolled.Spec.Steps, "api"); got != schema.StepStatusAlreadyApplied {
		t.Errorf("rollback status of api = %q, want %q (the apply did not change it)", got, schema.StepStatusAlreadyApplied)
	}
}

// TestRollback_AfterTheStampWriteFailed: every workload patch works and the
// write of the stamp fails. Before, that was a warning, and the rollback
// then found no stamp and did nothing.
func TestRollback_AfterTheStampWriteFailed(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	failNamespacePatchWith(client, `"`+schema.PlanHashAnnotation+`":"`)

	applied := mustApply(t, client, plan, false)
	if applied.Spec.Summary.Applied != 2 {
		t.Fatalf("apply summary = %+v, want 2 applied", applied.Spec.Summary)
	}
	if got := annotationsOf(t, client, "shop")[schema.PlanHashAnnotation]; got != "" {
		t.Fatalf("the stamp is %q; the reactor did not make its write fail", got)
	}

	mustRollback(t, client, plan, false)

	for _, name := range []string{"web", "api"} {
		if got := classOf(t, client, "shop", name); got != "" {
			t.Errorf("after the rollback, %s has class %q, want none", name, got)
		}
	}
}

// TestRollback_NoMarkChangesNothing: the plan was never applied (the
// preflight gate stopped the apply, or the apply failed before its first
// write). After the scan, some other tool put the class on a workload of the
// plan. A rollback step in a pipeline runs all the same, and it must change
// nothing: the namespace has no mark of this plan.
func TestRollback_NoMarkChangesNothing(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	setClass(t, client, "shop", "web", "gvisor")
	client.ClearActions()

	rolled := mustRollback(t, client, plan, false)

	if got := classOf(t, client, "shop", "web"); got != "gvisor" {
		t.Errorf("web has class %q; a rollback of a plan that was never applied removed a class that it did not set", got)
	}
	if n := workloadPatches(client); n != 0 {
		t.Errorf("the rollback sent %d workload patch(es), want 0", n)
	}
	if rolled.Spec.Summary.AlreadyApplied != 2 {
		t.Errorf("rollback summary = %+v, want 2 already-applied", rolled.Spec.Summary)
	}
}

// TestRollback_AfterAnApplyThatWasStopped: the apply process ended between
// two patches (a job that the CI system cancels). The test builds the state
// that such an apply leaves: apply-started on the namespace, the class on
// the first workload, and nothing more.
func TestRollback_AfterAnApplyThatWasStopped(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	hash := plan.Metadata.PlanHash
	if err := patchNamespaceMarks(context.Background(), client, "shop", map[string]*string{schema.ApplyStartedAnnotation: &hash}); err != nil {
		t.Fatalf("seeding apply-started: %v", err)
	}
	setClass(t, client, "shop", "web", "gvisor")

	rolled := mustRollback(t, client, plan, false)

	if got := classOf(t, client, "shop", "web"); got != "" {
		t.Errorf("after the rollback, web has class %q, want none", got)
	}
	if rolled.Spec.Summary.Applied != 1 || rolled.Spec.Summary.AlreadyApplied != 1 {
		t.Errorf("rollback summary = %+v, want 1 applied (web) and 1 already-applied (api)", rolled.Spec.Summary)
	}
	if marks := annotationsOf(t, client, "shop"); len(marks) != 0 {
		t.Errorf("after a complete rollback the namespace has annotations %v, want none", marks)
	}
}

// TestRollback_AfterAPartialApply_LeavesAnotherClass: the planner makes a
// step also for a workload that has another RuntimeClass. If the apply did
// not get to that step, the rollback must not remove that class: agentmoat
// did not set it.
func TestRollback_AfterAPartialApply_LeavesAnotherClass(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	setClass(t, client, "shop", "api", "kata")
	failPatchOf(client, "api")
	mustApply(t, client, plan, false)
	client.ClearActions()

	rolled := mustRollback(t, client, plan, false)

	if got := classOf(t, client, "shop", "api"); got != "kata" {
		t.Errorf("after the rollback, api has class %q, want kata (the apply never changed api)", got)
	}
	if got := classOf(t, client, "shop", "web"); got != "" {
		t.Errorf("after the rollback, web has class %q, want none", got)
	}
	if got := statusOf(t, rolled.Spec.Steps, "api"); got != schema.StepStatusAlreadyApplied {
		t.Errorf("rollback status of api = %q, want %q", got, schema.StepStatusAlreadyApplied)
	}
	if n := workloadPatches(client); n != 1 {
		t.Errorf("the rollback sent %d workload patch(es), want 1 (web only)", n)
	}
}

// TestRollback_DryRunAfterAPartialApply: a dry-run says what the rollback
// would do, and writes nothing.
func TestRollback_DryRunAfterAPartialApply(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	failPatchOf(client, "api")
	mustApply(t, client, plan, false)
	client.ClearActions()

	rolled := mustRollback(t, client, plan, true)

	if got := statusOf(t, rolled.Spec.Steps, "web"); got != schema.StepStatusApplied {
		t.Errorf("dry-run status of web = %q, want %q (it would be un-patched)", got, schema.StepStatusApplied)
	}
	if got := statusOf(t, rolled.Spec.Steps, "api"); got != schema.StepStatusAlreadyApplied {
		t.Errorf("dry-run status of api = %q, want %q", got, schema.StepStatusAlreadyApplied)
	}
	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Errorf("a dry-run rollback made a %s call on %s; it must only read", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if got := classOf(t, client, "shop", "web"); got != "gvisor" {
		t.Errorf("after a dry-run rollback, web has class %q, want gvisor", got)
	}
}

// TestRollback_SkipsAnUnfinishedApplyOfAnotherPlan: apply-started with
// another hash, and no stamp. Another plan was at work here; this plan
// leaves the namespace alone.
func TestRollback_SkipsAnUnfinishedApplyOfAnotherPlan(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	other := "another-plan-hash"
	if err := patchNamespaceMarks(context.Background(), client, "shop", map[string]*string{schema.ApplyStartedAnnotation: &other}); err != nil {
		t.Fatalf("seeding apply-started: %v", err)
	}
	setClass(t, client, "shop", "web", "gvisor")
	client.ClearActions()

	rolled := mustRollback(t, client, plan, false)

	if rolled.Spec.Summary.Skipped != 2 {
		t.Errorf("rollback summary = %+v, want 2 skipped", rolled.Spec.Summary)
	}
	for _, s := range rolled.Spec.Steps {
		if !strings.Contains(s.Error, "different plan") || !strings.Contains(s.Error, other) {
			t.Errorf("the skipped step must name the other plan, got %q", s.Error)
		}
	}
	if got := classOf(t, client, "shop", "web"); got != "gvisor" {
		t.Errorf("web has class %q, want gvisor: the namespace belongs to another plan", got)
	}
	if got := annotationsOf(t, client, "shop")[schema.ApplyStartedAnnotation]; got != other {
		t.Errorf("apply-started = %q, want the mark of the other plan kept", got)
	}
}

// TestRollback_ClearsOnlyItsOwnMarks: the stamp is of another plan (A), and
// apply-started is of this plan (B). This version of apply does not leave
// that state (the start of B removes the stamp of A), but a version before
// it could, and a person can. The rule for a rollback is the same in each
// case: a rollback of B undoes B and removes the mark of B, and it does not
// remove a mark with another hash.
func TestRollback_ClearsOnlyItsOwnMarks(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	hash, other := plan.Metadata.PlanHash, "plan-a-hash"
	if err := patchNamespaceMarks(context.Background(), client, "shop", map[string]*string{
		schema.PlanHashAnnotation:     &other,
		schema.ApplyStartedAnnotation: &hash,
	}); err != nil {
		t.Fatalf("seeding the marks: %v", err)
	}
	setClass(t, client, "shop", "web", "gvisor")

	mustRollback(t, client, plan, false)

	if got := classOf(t, client, "shop", "web"); got != "" {
		t.Errorf("after the rollback, web has class %q, want none", got)
	}
	marks := annotationsOf(t, client, "shop")
	if got := marks[schema.PlanHashAnnotation]; got != other {
		t.Errorf("the stamp is %q, want the stamp of the other plan kept", got)
	}
	if got, ok := marks[schema.ApplyStartedAnnotation]; ok {
		t.Errorf("apply-started is still %q after a complete rollback of its plan", got)
	}
}

// TestApply_RecordsTheStartBeforeTheFirstPatch: the order of the writes. The
// first write of an apply in a namespace is apply-started, and the workload
// patch comes after it. The reverse order would leave a window in which a
// workload is changed and the cluster has no record of it.
func TestApply_RecordsTheStartBeforeTheFirstPatch(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()

	mustApply(t, client, plan, false)

	var writes []string
	for _, a := range client.Actions() {
		if a.GetVerb() != "patch" {
			continue
		}
		pa := a.(clienttesting.PatchAction)
		writes = append(writes, a.GetResource().Resource+":"+string(pa.GetPatch()))
	}
	if len(writes) != 4 {
		t.Fatalf("the apply made %d writes, want 4 (apply-started, two workloads, the stamp):\n%s", len(writes), strings.Join(writes, "\n"))
	}
	if !strings.HasPrefix(writes[0], "namespaces:") || !strings.Contains(writes[0], `"`+schema.ApplyStartedAnnotation+`":"shop-hash"`) {
		t.Errorf("the first write is %s, want apply-started on the namespace", writes[0])
	}
	if !strings.HasPrefix(writes[1], "deployments:") || !strings.HasPrefix(writes[2], "deployments:") {
		t.Errorf("writes 2 and 3 are %s and %s, want the two workload patches", writes[1], writes[2])
	}
	// The last write sets the stamp and removes apply-started in one patch.
	if !strings.Contains(writes[3], `"`+schema.PlanHashAnnotation+`":"shop-hash"`) || !strings.Contains(writes[3], `"`+schema.ApplyStartedAnnotation+`":null`) {
		t.Errorf("the last write is %s, want the stamp set and apply-started removed", writes[3])
	}
}

// TestApply_ChangesNothingWhenTheStartCannotBeRecorded: the identity cannot
// patch the namespace. Before, the workloads were patched and the missing
// stamp was a warning. Now no workload is patched: a change with no record
// in the cluster is one that a rollback cannot find.
func TestApply_ChangesNothingWhenTheStartCannotBeRecorded(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	failNamespacePatchWith(client, schema.ApplyStartedAnnotation)

	applied := mustApply(t, client, plan, false)

	if applied.Spec.Summary.Failed != 2 || applied.Spec.Summary.Applied != 0 {
		t.Errorf("apply summary = %+v, want 2 failed and 0 applied", applied.Spec.Summary)
	}
	for _, s := range applied.Spec.Steps {
		if !strings.Contains(s.Error, "the workload was not changed") || !strings.Contains(s.Error, schema.ApplyStartedAnnotation) {
			t.Errorf("the failed step must say that nothing changed and name the annotation, got %q", s.Error)
		}
	}
	if n := workloadPatches(client); n != 0 {
		t.Errorf("the apply sent %d workload patch(es), want 0", n)
	}
	for _, name := range []string{"web", "api"} {
		if got := classOf(t, client, "shop", name); got != "" {
			t.Errorf("%s has class %q, want none", name, got)
		}
	}
	// The write is tried one time for the namespace, not one time for
	// each step.
	tries := 0
	for _, a := range client.Actions() {
		if a.GetVerb() == "patch" && a.GetResource().Resource == "namespaces" {
			tries++
		}
	}
	if tries != 1 {
		t.Errorf("apply-started was tried %d time(s), want 1", tries)
	}
}

// TestApply_RetryAfterAPartialApply: the second apply does the steps again
// (the namespace has no stamp), and when all of them work it leaves the
// stamp and no apply-started. This is the idempotency rule from before
// apply-started existed, and it must not change.
func TestApply_RetryAfterAPartialApply(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	plan := shopPlan()
	fail := true
	client.PrependReactor("patch", "deployments", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if fail && action.(clienttesting.PatchAction).GetName() == "api" {
			return true, nil, errors.New("admission webhook denied the request")
		}
		return false, nil, nil
	})

	first := mustApply(t, client, plan, false)
	if first.Spec.Summary.Failed != 1 {
		t.Fatalf("first apply summary = %+v, want 1 failed", first.Spec.Summary)
	}
	marks := annotationsOf(t, client, "shop")
	if marks[schema.PlanHashAnnotation] != "" || marks[schema.ApplyStartedAnnotation] != "shop-hash" {
		t.Fatalf("after a partial apply the namespace has %v, want apply-started only", marks)
	}

	fail = false
	second := mustApply(t, client, plan, false)
	if second.Spec.Summary.Applied != 2 {
		t.Errorf("second apply summary = %+v, want 2 applied (a retry, not already-applied)", second.Spec.Summary)
	}
	marks = annotationsOf(t, client, "shop")
	if marks[schema.PlanHashAnnotation] != "shop-hash" {
		t.Errorf("after the retry the stamp is %q, want shop-hash", marks[schema.PlanHashAnnotation])
	}
	if got, ok := marks[schema.ApplyStartedAnnotation]; ok {
		t.Errorf("after the retry apply-started is still %q", got)
	}

	third := mustApply(t, client, plan, false)
	if third.Spec.Summary.AlreadyApplied != 2 {
		t.Errorf("third apply summary = %+v, want 2 already-applied", third.Spec.Summary)
	}
}

// TestApply_DryRunWritesNoMark: a dry-run changes nothing, and that
// includes the annotations.
func TestApply_DryRunWritesNoMark(t *testing.T) {
	auditTmp(t)
	client := shopClient()

	mustApply(t, client, shopPlan(), true)

	for _, a := range client.Actions() {
		if a.GetVerb() != "get" {
			t.Errorf("a dry-run apply made a %s call on %s; it must only read", a.GetVerb(), a.GetResource().Resource)
		}
	}
	if marks := annotationsOf(t, client, "shop"); len(marks) != 0 {
		t.Errorf("after a dry-run the namespace has annotations %v, want none", marks)
	}
}

// TestRollback_NoMarkMakesNoWrite: a rollback in a namespace with no mark
// reads the namespace and does nothing more. (Before, it sent a patch to
// remove a stamp that was not there, and an identity with no patch
// permission on namespaces got a warning for a rollback that had nothing
// to do.)
func TestRollback_NoMarkMakesNoWrite(t *testing.T) {
	auditTmp(t)
	client := shopClient()

	mustRollback(t, client, shopPlan(), false)

	for _, a := range client.Actions() {
		if a.GetVerb() != "get" || a.GetResource().Resource != "namespaces" {
			t.Errorf("the rollback made a %s call on %s; with no mark it must only read the namespace", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

// TestMarkState is the table for stateFor: what the two annotations mean for
// a rollback with one plan.
func TestMarkState(t *testing.T) {
	cases := []struct {
		name  string
		marks namespaceMarks
		hash  string
		want  markState
	}{
		{"no mark", namespaceMarks{}, "h", marksNone},
		{"the stamp of this plan", namespaceMarks{stamp: "h"}, "h", marksStamped},
		{"apply-started of this plan", namespaceMarks{started: "h"}, "h", marksStarted},
		{"the two of this plan: the stamp decides", namespaceMarks{stamp: "h", started: "h"}, "h", marksStamped},
		{"the stamp of another plan", namespaceMarks{stamp: "x"}, "h", marksOther},
		{"apply-started of another plan", namespaceMarks{started: "x"}, "h", marksOther},
		{"another stamp, and apply-started of this plan", namespaceMarks{stamp: "x", started: "h"}, "h", marksStarted},
		{"the stamp of this plan, and apply-started of another", namespaceMarks{stamp: "h", started: "x"}, "h", marksStamped},
		{"the two of another plan", namespaceMarks{stamp: "x", started: "x"}, "h", marksOther},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.marks.stateFor(tc.hash); got != tc.want {
				t.Errorf("stateFor(%q) on %+v = %d, want %d", tc.hash, tc.marks, got, tc.want)
			}
		})
	}
}

// TestLiveRuntimeClass reads the class of each kind from the right path.
func TestLiveRuntimeClass(t *testing.T) {
	class := "gvisor"
	pod := corev1.PodSpec{RuntimeClassName: &class}
	tmpl := corev1.PodTemplateSpec{Spec: pod}
	meta := metav1.ObjectMeta{Name: "x", Namespace: "ns"}
	client := fake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: meta, Spec: pod},
		&appsv1.Deployment{ObjectMeta: meta, Spec: appsv1.DeploymentSpec{Template: tmpl}},
		&appsv1.StatefulSet{ObjectMeta: meta, Spec: appsv1.StatefulSetSpec{Template: tmpl}},
		&appsv1.DaemonSet{ObjectMeta: meta, Spec: appsv1.DaemonSetSpec{Template: tmpl}},
		&batchv1.Job{ObjectMeta: meta, Spec: batchv1.JobSpec{Template: tmpl}},
		&batchv1.CronJob{ObjectMeta: meta, Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: tmpl}}}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "bare", Namespace: "ns"}},
	)
	ctx := context.Background()
	for _, kind := range []string{"Pod", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob"} {
		got, err := liveRuntimeClass(ctx, client, schema.WorkloadRef{Kind: kind, Namespace: "ns", Name: "x"})
		if err != nil || got != "gvisor" {
			t.Errorf("%s: got %q, %v; want gvisor", kind, got, err)
		}
	}
	if got, err := liveRuntimeClass(ctx, client, schema.WorkloadRef{Kind: "Deployment", Namespace: "ns", Name: "bare"}); err != nil || got != "" {
		t.Errorf("a Deployment with no class: got %q, %v; want an empty string", got, err)
	}
	if _, err := liveRuntimeClass(ctx, client, schema.WorkloadRef{Kind: "Deployment", Namespace: "ns", Name: "gone"}); !apierrors.IsNotFound(err) {
		t.Errorf("a Deployment that is not there: got %v, want an error that IsNotFound accepts", err)
	}
	if _, err := liveRuntimeClass(ctx, client, schema.WorkloadRef{Kind: "Widget", Namespace: "ns", Name: "x"}); err == nil {
		t.Error("an unknown kind: got no error")
	}
}

// planA is a second plan for the shop namespace: only web, with its own
// hash. With shopPlan (web and api) it makes the case of two plans that list
// the same workload.
func planA() *schema.MigrationPlan {
	plan := shopPlan()
	plan.Metadata.PlanHash = "plan-a-hash"
	var steps []schema.PlanStep
	for _, s := range plan.Spec.Steps {
		if s.Target.Name == "web" {
			s.Order = 1
			steps = append(steps, s)
		}
	}
	plan.Spec.Steps = steps
	return plan
}

// TestApply_TakesTheNamespaceOverFromAnOlderPlan is the state that each
// later run of a pipeline starts from: the namespace has the stamp of an
// older plan.
//
//	plan A = {web}       applied in full: stamp A
//	plan B = {web, api}  api fails: exit 3
//	rollback B           web and api have no class
//	apply A again        must patch web again
//
// If the stamp of A stayed through all of this, the last apply would report
// already-applied for a web that has no class: a stamp that is false. So the
// start of B removes the stamp of A.
func TestApply_TakesTheNamespaceOverFromAnOlderPlan(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	a, b := planA(), shopPlan()

	mustApply(t, client, a, false)
	if got := annotationsOf(t, client, "shop")[schema.PlanHashAnnotation]; got != "plan-a-hash" {
		t.Fatalf("after apply A the stamp is %q, want plan-a-hash", got)
	}

	failPatchOf(client, "api")
	client.ClearActions()
	applied := mustApply(t, client, b, false)
	if applied.Spec.Summary.Applied != 1 || applied.Spec.Summary.Failed != 1 {
		t.Fatalf("apply B summary = %+v, want 1 applied and 1 failed", applied.Spec.Summary)
	}
	marks := annotationsOf(t, client, "shop")
	if got, ok := marks[schema.PlanHashAnnotation]; ok {
		t.Errorf("after the start of B the stamp is still %q; B must take the namespace over", got)
	}
	if got := marks[schema.ApplyStartedAnnotation]; got != "shop-hash" {
		t.Errorf("apply-started = %q, want shop-hash", got)
	}
	// One write does the two things. With two writes, a stop between them
	// would leave the namespace with no mark and a patch on the way, or
	// with two marks that disagree.
	first := firstNamespacePatch(client)
	if !strings.Contains(first, `"`+schema.ApplyStartedAnnotation+`":"shop-hash"`) || !strings.Contains(first, `"`+schema.PlanHashAnnotation+`":null`) {
		t.Errorf("the first namespace write of B is %s, want apply-started set and the old stamp removed in one patch", first)
	}

	mustRollback(t, client, b, false)
	wantNoClassAndNoMark(t, client, "after rollback B")

	again := mustApply(t, client, a, false)
	if again.Spec.Summary.Applied != 1 || again.Spec.Summary.AlreadyApplied != 0 {
		t.Errorf("second apply of A summary = %+v, want 1 applied: web has no class, so already-applied would be false", again.Spec.Summary)
	}
	if got := classOf(t, client, "shop", "web"); got != "gvisor" {
		t.Errorf("after the second apply of A, web has class %q, want gvisor", got)
	}
}

// firstNamespacePatch is the body of the first patch that the client sent
// to a namespace, "" when it sent none.
func firstNamespacePatch(client *fake.Clientset) string {
	for _, action := range client.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "namespaces" {
			return string(action.(clienttesting.PatchAction).GetPatch())
		}
	}
	return ""
}

// wantNoClassAndNoMark checks the state of the shop namespace after a
// complete rollback: no workload has a class, and the namespace has no
// annotation.
func wantNoClassAndNoMark(t *testing.T, client *fake.Clientset, when string) {
	t.Helper()
	for _, name := range []string{"web", "api"} {
		if got := classOf(t, client, "shop", name); got != "" {
			t.Errorf("%s, %s has class %q, want none", when, name, got)
		}
	}
	if marks := annotationsOf(t, client, "shop"); len(marks) != 0 {
		t.Errorf("%s, the namespace has annotations %v, want none", when, marks)
	}
}

// TestRollback_OfAnOlderPlanAfterATakeOver: plan B has the namespace, and a
// rollback comes with plan A. A does not govern the namespace now: its step
// is skipped, with a message that names B, and nothing changes.
func TestRollback_OfAnOlderPlanAfterATakeOver(t *testing.T) {
	auditTmp(t)
	client := shopClient()
	a, b := planA(), shopPlan()
	mustApply(t, client, a, false)
	mustApply(t, client, b, false)
	client.ClearActions()

	rolled := mustRollback(t, client, a, false)

	if rolled.Spec.Summary.Skipped != 1 {
		t.Fatalf("rollback A summary = %+v, want 1 skipped", rolled.Spec.Summary)
	}
	if reason := rolled.Spec.Steps[0].Error; !strings.Contains(reason, "shop-hash") {
		t.Errorf("the skipped step must name the plan that has the namespace, got %q", reason)
	}
	if n := workloadPatches(client); n != 0 {
		t.Errorf("rollback A sent %d workload patch(es), want 0", n)
	}
	if got := annotationsOf(t, client, "shop")[schema.PlanHashAnnotation]; got != "shop-hash" {
		t.Errorf("the stamp is %q, want the stamp of B kept", got)
	}
}

// TestApplyAndRollback_RefuseAPlanWithNoHash: the marks on the namespace are
// the hash of the plan. A plan with no hash cannot be recorded, so a change
// from it would be one that no rollback finds. The refusal is the same for
// a dry-run: a dry-run that works for a plan that the real run refuses is no
// rehearsal.
func TestApplyAndRollback_RefuseAPlanWithNoHash(t *testing.T) {
	auditTmp(t)
	for _, dryRun := range []bool{true, false} {
		client := shopClient()
		plan := shopPlan()
		plan.Metadata.PlanHash = ""
		opts := Options{Client: client, Plan: plan, DryRun: dryRun, Stderr: io.Discard}

		if _, err := Apply(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "planHash") {
			t.Errorf("Apply (dry-run=%v) of a plan with no hash: got %v, want an error that names planHash", dryRun, err)
		}
		if _, err := Rollback(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "planHash") {
			t.Errorf("Rollback (dry-run=%v) of a plan with no hash: got %v, want an error that names planHash", dryRun, err)
		}
		if n := len(client.Actions()); n != 0 {
			t.Errorf("dry-run=%v: the refusal made %d API call(s), want 0", dryRun, n)
		}
	}
}

// TestNotes_OnStderr: two lines that the result document cannot carry.
//
//   - An apply that replaces the record of another plan says so, with the
//     hash that it replaces. After that write, nothing in the cluster has
//     that hash.
//   - A rollback that finds no record says so, one time for the namespace.
//     Its result is "already-applied" and exit 0, which is also the result
//     for a plan whose record another plan replaced.
func TestNotes_OnStderr(t *testing.T) {
	auditTmp(t)
	ctx := context.Background()
	client := shopClient()
	a, b := planA(), shopPlan()
	mustApply(t, client, a, false)

	var out strings.Builder
	if _, err := Apply(ctx, Options{Client: client, Plan: b, Stderr: &out}); err != nil {
		t.Fatalf("Apply B: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "note: namespace shop had the record of another plan (plan-hash plan-a-hash)") {
		t.Errorf("apply B over the stamp of A: stderr has no take-over note:\n%s", got)
	}

	// A second apply of B is already-applied: it replaces nothing and says
	// nothing about another plan.
	out.Reset()
	if _, err := Apply(ctx, Options{Client: client, Plan: b, Stderr: &out}); err != nil {
		t.Fatalf("second Apply B: %v", err)
	}
	if got := out.String(); strings.Contains(got, "another plan") {
		t.Errorf("a second apply of the same plan must not print the take-over note:\n%s", got)
	}

	// Roll B back. The namespace then has no mark, and a rollback of A
	// finds nothing: two steps would still give one line.
	mustRollback(t, client, b, false)
	out.Reset()
	if _, err := Rollback(ctx, Options{Client: client, Plan: b, Stderr: &out}); err != nil {
		t.Fatalf("second Rollback B: %v", err)
	}
	got := out.String()
	if n := strings.Count(got, "note: namespace shop has no record of this plan"); n != 1 {
		t.Errorf("a rollback with no record: the note is on stderr %d time(s), want 1 (the plan has two steps):\n%s", n, got)
	}
	if !strings.Contains(got, "apply this plan again and then roll it back") {
		t.Errorf("the note must give the way out:\n%s", got)
	}
}
