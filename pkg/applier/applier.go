// Package applier: Apply + Rollback entry points.
//
// Both functions follow the same shape:
//
//  1. Validate options.
//  2. For every namespace the plan touches, read the two marks
//     (`agentmoat.io/plan-hash` and `agentmoat.io/apply-started`; see
//     idempotency.go for what each means).
//  3. Walk the plan's steps in order. For each step:
//     a. Decide if the step is needed (idempotency: if the namespace
//     annotation already equals plan.Metadata.PlanHash, the step
//     is `already-applied`).
//     b. Build the patch bytes for the step.
//     c. In dry-run mode, surface the patch in the StepResult and
//     continue.
//     d. Apply only: before the first patch in a namespace, write
//     `agentmoat.io/apply-started` on it. Record first, then mutate.
//     e. Send the patch to the API server, emit an Event, and append
//     an audit line.
//  4. If all steps in a namespace succeeded, write the stamp and remove
//     apply-started (for Apply), or remove the marks of this plan (for
//     Rollback).
//  5. Return the assembled ApplyResult / RollbackResult.
//
// The applier is NOT concurrent. PlannerOptions.MaxParallel is honored as
// an envelope value (echoed back so the operator knows what the plan asked
// for), but the applier itself walks steps serially. A K8s API server can
// withstand a parallel apply, but a parallel apply complicates exit-code
// shaping (partial vs full) and dry-run output ordering. v1 keeps it
// simple; revisit in Phase 7 if needed.
package applier

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/0hardik1/agentmoat/internal/audit"
	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// run is what the steps of one Apply or one Rollback share. The two entry
// points build it, and then call one method for each step, so that the
// decision for one step reads from top to bottom in one place.
type run struct {
	opts   Options
	stderr io.Writer

	// hash is the hash of the plan. It is never empty (CheckPlan).
	hash string

	// marks are the annotations of each namespace of the plan, as they
	// were when the run started.
	marks map[string]namespaceMarks

	// recorder writes apply-started before the first patch in a
	// namespace. Apply only.
	recorder *startRecorder

	// incomplete has a namespace when one of its steps failed or was
	// skipped. Such a namespace gets no stamp from an apply, and keeps
	// its marks in a rollback.
	incomplete map[string]bool

	// noRecord has a namespace when a rollback found no mark there and
	// said so on stderr. The line is printed one time for a namespace.
	noRecord map[string]bool
}

// newRun reads the marks of every namespace that the plan touches. This is
// the last point where the run can end with no change to the cluster.
func newRun(ctx context.Context, opts Options) (*run, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	marks, err := readAllNamespaceMarks(ctx, opts.Client, opts.Plan)
	if err != nil {
		return nil, fmt.Errorf("applier: reading namespace annotations: %w", err)
	}
	hash := opts.Plan.Metadata.PlanHash
	return &run{
		opts:       opts,
		stderr:     stderr,
		hash:       hash,
		marks:      marks,
		recorder:   newStartRecorder(opts.Client, hash, marks, stderr),
		incomplete: map[string]bool{},
		noRecord:   map[string]bool{},
	}, nil
}

// metadata is the envelope metadata of the result.
func (r *run) metadata() schema.ApplyMetadata {
	return schema.ApplyMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          r.opts.Cluster,
		AgentmoatVersion: r.opts.AgentmoatVersion,
		PlanHash:         r.hash,
		DryRun:           r.opts.DryRun,
	}
}

// say prints one progress line on stderr: "<verb>: Kind/namespace name".
// detail, when it is not empty, comes after a colon.
func (r *run) say(verb string, step schema.PlanStep, detail string) {
	if detail != "" {
		detail = ": " + detail
	}
	_, _ = fmt.Fprintf(r.stderr, "%s: %s/%s %s%s\n",
		verb, step.Target.Kind, step.Target.Namespace, step.Target.Name, detail)
}

// fail makes the result of a step that did not work, and records that its
// namespace is not complete.
func (r *run) fail(sr schema.StepResult, status schema.StepStatus, reason string) schema.StepResult {
	sr.Status = status
	sr.Error = reason
	r.incomplete[sr.Target.Namespace] = true
	return sr
}

// Apply runs every step in opts.Plan in order. Returns a populated
// ApplyResult and a non-nil error only on fatal precondition failures
// (nil client, nil plan). Step-level errors are reported per-step in
// ApplyResult.Spec.Steps; the function still returns nil for err in that
// case so the caller can format the result and surface the appropriate
// exit code (3 for partial, 0 for full success).
func Apply(ctx context.Context, opts Options) (*schema.ApplyResult, error) {
	if err := validate(opts); err != nil {
		return nil, err
	}
	r, err := newRun(ctx, opts)
	if err != nil {
		return nil, err
	}
	res := schema.NewApplyResult()
	res.Metadata = r.metadata()

	stepResults := make([]schema.StepResult, 0, len(opts.Plan.Spec.Steps))
	for _, step := range opts.Plan.Spec.Steps {
		sr := r.applyStep(ctx, step)
		stepResults = append(stepResults, sr)
		emitAudit(opts, step, sr, "apply")
	}
	r.stampNamespaces(ctx)

	res.Spec = schema.ApplySpec{
		Summary: summariseSteps(stepResults),
		Steps:   stepResults,
	}
	return res, nil
}

// applyStep does one step of an apply.
func (r *run) applyStep(ctx context.Context, step schema.PlanStep) schema.StepResult {
	ns := step.Target.Namespace
	sr := schema.StepResult{Order: step.Order, Target: step.Target}

	// Idempotency: if this namespace already carries our plan hash,
	// short-circuit.
	if r.marks[ns].stamp == r.hash {
		sr.Status = schema.StepStatusAlreadyApplied
		return sr
	}

	// Build patch bytes (deterministic; same step -> same bytes).
	body, ptype, err := applyPatchBytes(step)
	if err != nil {
		return r.fail(sr, schema.StepStatusFailed, err.Error())
	}
	sr.Patch = string(body)

	if r.opts.DryRun {
		sr.Status = schema.StepStatusApplied
		_, _ = fmt.Fprintf(r.stderr, "dry-run: would patch %s/%s %s\n",
			step.Target.Kind, step.Target.Namespace, step.Target.Name)
		return sr
	}

	// Record first, then mutate. Before the first patch in this
	// namespace, the namespace must say that an apply of this plan
	// started here: that mark is what lets a rollback undo an apply that
	// did not finish. If the mark cannot be written, this step changes
	// nothing. A change that the cluster has no record of is one that
	// `rollback` cannot find.
	if err := r.recorder.record(ctx, ns); err != nil {
		sr = r.fail(sr, schema.StepStatusFailed, fmt.Sprintf(
			"the workload was not changed: the start of the apply could not be recorded on namespace %s (annotation %s): %v",
			ns, schema.ApplyStartedAnnotation, err))
		r.say("failed", step, sr.Error)
		return sr
	}

	// Real mutation: send the patch.
	if err := sendPatch(ctx, r.opts.Client, step, body, ptype); err != nil {
		r.say("failed", step, err.Error())
		return r.fail(sr, schema.StepStatusFailed, err.Error())
	}

	sr.Status = schema.StepStatusApplied
	r.say("applied", step, "")
	if r.opts.EmitEvents {
		emitEvent(ctx, r.opts.Client, step, "Applied", "RuntimeClass set to "+step.RuntimeClassName)
	}
	return sr
}

// stampNamespaces ends an apply. It stamps every namespace where every step
// succeeded (or was already-applied), and removes apply-started there. A
// namespace with even one failed step is intentionally NOT stamped: a re-run
// should retry. Its apply-started mark stays, so a rollback finds it.
//
// A stamp write that fails is a warning and not a failed step: the
// workloads are changed, and apply-started still records it. The cost is
// that a second apply does the steps again.
func (r *run) stampNamespaces(ctx context.Context) {
	if r.opts.DryRun {
		return
	}
	for _, ns := range affectedNamespaces(r.opts.Plan) {
		if r.incomplete[ns] {
			continue
		}
		if err := stampNamespace(ctx, r.opts.Client, ns, r.hash); err != nil {
			_, _ = fmt.Fprintf(r.stderr, "warning: could not stamp namespace %s: %v\n", ns, err)
		}
	}
}

// Rollback walks opts.Plan in *reverse* order and undoes each step:
//   - sets runtimeClassName back to nil on the controller (via JSON merge
//     patch),
//   - clears the annotations of this plan on each affected namespace.
//
// Rollback is also idempotent: a fully rolled-back namespace re-runs
// cleanly and reports every step as already-applied (the annotation is
// already absent and the spec already lacks runtimeClassName).
//
// Rollback only acts on namespaces that carry a mark of THIS plan. There
// are two (see idempotency.go):
//
//   - The stamp (plan-hash) has the hash of this plan: the apply was
//     complete. Every step is un-patched.
//   - Only apply-started has the hash: the apply did not finish. A step is
//     un-patched when its workload has the class of the step now, which
//     means that the apply got to it. A workload that the apply did not
//     change is left as it is, also when it has some other class.
//
// A namespace with no mark was never changed by this plan: every step is
// reported as already-applied, and no workload is read or patched. A
// namespace with only marks of a different hash is governed by another
// plan; its steps are reported as skipped and its annotations are preserved.
func Rollback(ctx context.Context, opts Options) (*schema.RollbackResult, error) {
	if err := validate(opts); err != nil {
		return nil, err
	}
	r, err := newRun(ctx, opts)
	if err != nil {
		return nil, err
	}
	res := schema.NewRollbackResult()
	res.Metadata = r.metadata()

	// Walk in reverse order so the highest-risk (last-applied) workloads
	// roll back first: this minimizes the time the cluster spends in a
	// partially-rolled-back state if one of the patches fails.
	stepResults := make([]schema.StepResult, 0, len(opts.Plan.Spec.Steps))
	for i := len(opts.Plan.Spec.Steps) - 1; i >= 0; i-- {
		step := opts.Plan.Spec.Steps[i]
		sr := r.rollbackStep(ctx, step)
		stepResults = append(stepResults, sr)
		emitAudit(opts, step, sr, "rollback")
	}
	r.clearNamespaces(ctx)

	// The rollback walked steps in reverse; re-sort the per-step results
	// back into plan order so the rendered output reads the same way as
	// the plan and the apply.
	sortStepResultsByOrder(stepResults)

	res.Spec = schema.RollbackSpec{
		Summary: summariseSteps(stepResults),
		Steps:   stepResults,
	}
	return res, nil
}

// rollbackStep undoes one step. The marks of the namespace decide what
// "undo" means here; see the comment on Rollback.
func (r *run) rollbackStep(ctx context.Context, step schema.PlanStep) schema.StepResult {
	ns := step.Target.Namespace
	sr := schema.StepResult{Order: step.Order, Target: step.Target}

	switch r.marks[ns].stateFor(r.hash) {
	case marksNone:
		// With no mark, no apply of this plan changed that namespace;
		// report already-applied (the rollback target state). Do not
		// look at the workload: a class on it is then from another
		// source, and it is not for this plan to remove.
		//
		// Say it on stderr, one time for the namespace. "Nothing to do"
		// has two causes that look the same in the result: the plan was
		// never applied, or it was and a later apply of another plan
		// replaced its record. The operator of the second case must
		// know that exit 0 does not mean that the workloads are back on
		// runc.
		if !r.noRecord[ns] {
			r.noRecord[ns] = true
			_, _ = fmt.Fprintf(r.stderr, "note: namespace %s has no record of this plan, so nothing is changed there. "+
				"If the plan was applied and another plan replaced its record, apply this plan again and then roll it back.\n", ns)
		}
		sr.Status = schema.StepStatusAlreadyApplied
		return sr

	case marksOther:
		// Another plan governs this namespace. Un-patching now would
		// fight that plan, and clearing the annotation would erase its
		// idempotency stamp. Skip the step and tell the operator which
		// way out to take.
		sr = r.fail(sr, schema.StepStatusSkipped, otherPlanReason(ns, r.marks[ns]))
		r.say("skipped", step, sr.Error)
		return sr

	case marksStarted:
		// The apply of this plan did not finish here. Find out if it
		// got to this step.
		if sr, done := r.nothingToUndo(ctx, step, sr); done {
			return sr
		}

	case marksStamped:
		// The apply was complete: every step was applied. Un-patch.
	}

	body, ptype, err := rollbackPatchBytes(step)
	if err != nil {
		return r.fail(sr, schema.StepStatusFailed, err.Error())
	}
	sr.Patch = string(body)

	if r.opts.DryRun {
		sr.Status = schema.StepStatusApplied
		_, _ = fmt.Fprintf(r.stderr, "dry-run: would un-patch %s/%s %s\n",
			step.Target.Kind, step.Target.Namespace, step.Target.Name)
		return sr
	}

	if err := sendPatch(ctx, r.opts.Client, step, body, ptype); err != nil {
		// A 404 here means the workload no longer exists. Treat that as
		// already-rolled-back, since the rollback's goal state is "the
		// runtime class is no longer set", and a missing object
		// satisfies that vacuously.
		if apierrors.IsNotFound(err) {
			sr.Status = schema.StepStatusAlreadyApplied
			return sr
		}
		r.say("rollback failed", step, err.Error())
		return r.fail(sr, schema.StepStatusFailed, err.Error())
	}

	sr.Status = schema.StepStatusApplied
	r.say("rolled back", step, "")
	if r.opts.EmitEvents {
		emitEvent(ctx, r.opts.Client, step, "RolledBack", "RuntimeClass cleared")
	}
	return sr
}

// nothingToUndo reads the workload of a step in a namespace where the apply
// did not finish. done is true when the step is decided with no patch: the
// workload is gone, it cannot be read, or it does not have the class of the
// step (the apply did not get to it). Only a workload that has the class of
// the step now goes on to the un-patch. See live.go for why the plan cannot
// answer this question.
func (r *run) nothingToUndo(ctx context.Context, step schema.PlanStep, sr schema.StepResult) (result schema.StepResult, done bool) {
	live, err := liveRuntimeClass(ctx, r.opts.Client, step.Target)
	switch {
	case apierrors.IsNotFound(err):
		// The workload is gone: nothing is left to undo.
		sr.Status = schema.StepStatusAlreadyApplied
		return sr, true
	case err != nil:
		r.say("rollback failed", step, err.Error())
		return r.fail(sr, schema.StepStatusFailed, err.Error()), true
	case live != step.RuntimeClassName:
		sr.Status = schema.StepStatusAlreadyApplied
		r.say("nothing to undo", step, fmt.Sprintf("it does not have RuntimeClass %q (the apply did not change it)", step.RuntimeClassName))
		return sr, true
	}
	return sr, false
}

// otherPlanReason is the text of a step that a rollback skips because the
// namespace belongs to another plan.
func otherPlanReason(ns string, marks namespaceMarks) string {
	if marks.stamp != "" {
		return fmt.Sprintf(
			"namespace %s is stamped with a different plan (hash %s); roll back with that plan, or re-apply this one first",
			ns, marks.stamp)
	}
	return fmt.Sprintf(
		"namespace %s has an apply of a different plan that did not finish (hash %s); roll back with that plan, or apply this one first",
		ns, marks.started)
}

// clearNamespaces ends a rollback. It removes the marks of this plan from
// any namespace where every step rolled back successfully. A namespace with
// no mark of this plan gets no write.
func (r *run) clearNamespaces(ctx context.Context) {
	if r.opts.DryRun {
		return
	}
	for _, ns := range affectedNamespaces(r.opts.Plan) {
		if r.incomplete[ns] {
			continue
		}
		if err := clearOwnMarks(ctx, r.opts.Client, ns, r.hash, r.marks[ns]); err != nil {
			_, _ = fmt.Fprintf(r.stderr, "warning: could not clear the annotations of this plan on namespace %s: %v\n", ns, err)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// validate checks the precondition fields of Options. Returns a non-nil
// error if any are missing.
func validate(opts Options) error {
	if opts.Client == nil {
		return fmt.Errorf("applier: opts.Client is nil")
	}
	return CheckPlan(opts.Plan)
}

// sendPatch sends the prepared patch body to the API server using the right
// typed client for the step's Kind. Each branch maps to one client-go API
// surface; we keep them inlined for clarity rather than hiding behind a
// dynamic client (which would lose the typed PatchType safety net).
func sendPatch(ctx context.Context, client kubernetes.Interface, step schema.PlanStep, body []byte, patchType apitypes.PatchType) error {
	opts := metav1.PatchOptions{}
	ns := step.Target.Namespace
	name := step.Target.Name

	switch step.Target.Kind {
	case kindPod:
		_, err := client.CoreV1().Pods(ns).Patch(ctx, name, patchType, body, opts)
		return err
	case kindDeployment:
		_, err := client.AppsV1().Deployments(ns).Patch(ctx, name, patchType, body, opts)
		return err
	case kindStatefulSet:
		_, err := client.AppsV1().StatefulSets(ns).Patch(ctx, name, patchType, body, opts)
		return err
	case kindDaemonSet:
		_, err := client.AppsV1().DaemonSets(ns).Patch(ctx, name, patchType, body, opts)
		return err
	case kindJob:
		_, err := client.BatchV1().Jobs(ns).Patch(ctx, name, patchType, body, opts)
		return err
	case kindCronJob:
		_, err := client.BatchV1().CronJobs(ns).Patch(ctx, name, patchType, body, opts)
		return err
	default:
		return fmt.Errorf("applier: cannot send patch for unsupported kind %q", step.Target.Kind)
	}
}

// emitEvent records a Kubernetes Event on the affected namespace describing
// the per-step action. Best-effort: a failure here is logged but not
// returned, because the audit log already carries the durable trail.
func emitEvent(ctx context.Context, client kubernetes.Interface, step schema.PlanStep, reason, message string) {
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "agentmoat-",
			Namespace:    step.Target.Namespace,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:      step.Target.Kind,
			Namespace: step.Target.Namespace,
			Name:      step.Target.Name,
		},
		Reason:         reason,
		Message:        message,
		Type:           corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: "agentmoat"},
		FirstTimestamp: metav1.Now(),
		LastTimestamp:  metav1.Now(),
	}
	_, _ = client.CoreV1().Events(step.Target.Namespace).Create(ctx, ev, metav1.CreateOptions{})
}

// emitAudit appends one entry to ~/.agentmoat/audit.jsonl describing the
// outcome of the step. Best-effort: a failure here only emits a warning to
// stderr; the apply itself does not abort.
func emitAudit(opts Options, step schema.PlanStep, result schema.StepResult, action string) {
	if !opts.AuditEnabled {
		return
	}
	entry := audit.Entry{
		Action:   action,
		DryRun:   opts.DryRun,
		PlanHash: opts.Plan.Metadata.PlanHash,
		Workload: step.Target,
		Status:   result.Status,
		Error:    result.Error,
	}
	if _, err := audit.Append(entry); err != nil {
		stderr := opts.Stderr
		if stderr == nil {
			stderr = os.Stderr
		}
		_, _ = fmt.Fprintf(stderr, "warning: audit append failed: %v\n", err)
	}
}

// summariseSteps bucketises the step results into an ApplySummary.
func summariseSteps(steps []schema.StepResult) schema.ApplySummary {
	s := schema.ApplySummary{Total: len(steps)}
	for _, r := range steps {
		switch r.Status {
		case schema.StepStatusApplied:
			s.Applied++
		case schema.StepStatusAlreadyApplied:
			s.AlreadyApplied++
		case schema.StepStatusSkipped:
			s.Skipped++
		case schema.StepStatusFailed:
			s.Failed++
		}
	}
	return s
}

// sortStepResultsByOrder sorts step results by their original Order
// (1-based) ascending. Used by Rollback, which walks the plan in reverse.
func sortStepResultsByOrder(steps []schema.StepResult) {
	sort.SliceStable(steps, func(i, j int) bool {
		return steps[i].Order < steps[j].Order
	})
}
