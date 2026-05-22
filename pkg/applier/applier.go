// Package applier: Apply + Rollback entry points.
//
// Both functions follow the same shape:
//
//  1. Validate options.
//  2. For every namespace the plan touches, read the existing
//     `agentmoat.io/plan-hash` annotation.
//  3. Walk the plan's steps in order. For each step:
//     a. Decide if the step is needed (idempotency: if the namespace
//     annotation already equals plan.Metadata.PlanHash, the step
//     is `already-applied`).
//     b. Build the patch bytes for the step.
//     c. In dry-run mode, surface the patch in the StepResult and
//     continue.
//     d. Otherwise, send the patch to the API server, emit an
//     Event, and append an audit line.
//  4. If all steps in a namespace succeeded, write the namespace
//     annotation (for Apply) or clear it (for Rollback).
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
	"time"

	"github.com/0hardik1/agentmoat/internal/audit"
	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apitypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

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
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	res := schema.NewApplyResult()
	res.Metadata = schema.ApplyMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          opts.Cluster,
		AgentmoatVersion: opts.AgentmoatVersion,
		PlanHash:         opts.Plan.Metadata.PlanHash,
		DryRun:           opts.DryRun,
	}

	// Pre-flight: read the existing annotation for every namespace the
	// plan touches so we can decide per-step whether the work is needed.
	existing, err := readAllNamespaceAnnotations(ctx, opts.Client, opts.Plan)
	if err != nil {
		return nil, fmt.Errorf("applier: reading namespace annotations: %w", err)
	}

	stepResults := make([]schema.StepResult, 0, len(opts.Plan.Spec.Steps))
	touchedNamespaces := make(map[string]bool) // ns -> any step applied?
	allSuccess := make(map[string]bool)        // ns -> all-steps-succeeded?
	for ns := range existing {
		allSuccess[ns] = true
	}

	for _, step := range opts.Plan.Spec.Steps {
		ns := step.Target.Namespace
		sr := schema.StepResult{
			Order:  step.Order,
			Target: step.Target,
		}

		// Idempotency: if this namespace already carries our plan hash,
		// short-circuit.
		if existing[ns] == opts.Plan.Metadata.PlanHash && opts.Plan.Metadata.PlanHash != "" {
			sr.Status = schema.StepStatusAlreadyApplied
			stepResults = append(stepResults, sr)
			emitAudit(opts, step, sr, "apply")
			continue
		}

		// Build patch bytes (deterministic; same step -> same bytes).
		body, ptype, err := applyPatchBytes(step)
		if err != nil {
			sr.Status = schema.StepStatusFailed
			sr.Error = err.Error()
			allSuccess[ns] = false
			stepResults = append(stepResults, sr)
			emitAudit(opts, step, sr, "apply")
			continue
		}
		sr.Patch = string(body)

		if opts.DryRun {
			sr.Status = schema.StepStatusApplied
			stepResults = append(stepResults, sr)
			_, _ = fmt.Fprintf(stderr, "dry-run: would patch %s/%s %s\n",
				step.Target.Kind, step.Target.Namespace, step.Target.Name)
			emitAudit(opts, step, sr, "apply")
			continue
		}

		// Real mutation: send the patch.
		if err := sendPatch(ctx, opts.Client, step, body, ptype); err != nil {
			sr.Status = schema.StepStatusFailed
			sr.Error = err.Error()
			allSuccess[ns] = false
			_, _ = fmt.Fprintf(stderr, "failed: %s/%s %s: %v\n",
				step.Target.Kind, step.Target.Namespace, step.Target.Name, err)
			stepResults = append(stepResults, sr)
			emitAudit(opts, step, sr, "apply")
			continue
		}

		sr.Status = schema.StepStatusApplied
		stepResults = append(stepResults, sr)
		touchedNamespaces[ns] = true
		_, _ = fmt.Fprintf(stderr, "applied: %s/%s %s\n",
			step.Target.Kind, step.Target.Namespace, step.Target.Name)
		if opts.EmitEvents {
			emitEvent(ctx, opts.Client, step, "Applied", "RuntimeClass set to "+step.RuntimeClassName)
		}
		emitAudit(opts, step, sr, "apply")
	}

	// Stamp the namespace annotation on every namespace where every step
	// succeeded (or was already-applied). A namespace with even one
	// failed step is intentionally NOT stamped: a re-run should retry.
	if !opts.DryRun {
		writeNamespaceStamps(ctx, opts.Client, opts.Plan, allSuccess, stderr, opts.Plan.Metadata.PlanHash, "stamp")
	}

	res.Spec = schema.ApplySpec{
		Summary: summariseSteps(stepResults),
		Steps:   stepResults,
	}
	return res, nil
}

// Rollback walks opts.Plan in *reverse* order and undoes each step:
//   - sets runtimeClassName back to nil on the controller (via JSON merge
//     patch),
//   - clears the agentmoat.io/plan-hash annotation on each affected
//     namespace.
//
// Rollback is also idempotent: a fully rolled-back namespace re-runs
// cleanly and reports every step as already-applied (the annotation is
// already absent and the spec already lacks runtimeClassName).
func Rollback(ctx context.Context, opts Options) (*schema.RollbackResult, error) {
	if err := validate(opts); err != nil {
		return nil, err
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	res := schema.NewRollbackResult()
	res.Metadata = schema.ApplyMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          opts.Cluster,
		AgentmoatVersion: opts.AgentmoatVersion,
		PlanHash:         opts.Plan.Metadata.PlanHash,
		DryRun:           opts.DryRun,
	}

	// For rollback, the namespace-level signal is the inverse of apply:
	// if the annotation is absent the rollback is already-applied.
	existing, err := readAllNamespaceAnnotations(ctx, opts.Client, opts.Plan)
	if err != nil {
		return nil, fmt.Errorf("applier: reading namespace annotations: %w", err)
	}

	stepResults := make([]schema.StepResult, 0, len(opts.Plan.Spec.Steps))
	allSuccess := make(map[string]bool)
	for ns := range existing {
		allSuccess[ns] = true
	}

	// Walk in reverse order so the highest-risk (last-applied) workloads
	// roll back first: this minimizes the time the cluster spends in a
	// partially-rolled-back state if one of the patches fails.
	for i := len(opts.Plan.Spec.Steps) - 1; i >= 0; i-- {
		step := opts.Plan.Spec.Steps[i]
		ns := step.Target.Namespace
		sr := schema.StepResult{
			Order:  step.Order,
			Target: step.Target,
		}

		// If the annotation is absent we never applied this plan to that
		// namespace; report already-applied (the rollback target state).
		if existing[ns] == "" {
			sr.Status = schema.StepStatusAlreadyApplied
			stepResults = append(stepResults, sr)
			emitAudit(opts, step, sr, "rollback")
			continue
		}

		body, ptype, err := rollbackPatchBytes(step)
		if err != nil {
			sr.Status = schema.StepStatusFailed
			sr.Error = err.Error()
			allSuccess[ns] = false
			stepResults = append(stepResults, sr)
			emitAudit(opts, step, sr, "rollback")
			continue
		}
		sr.Patch = string(body)

		if opts.DryRun {
			sr.Status = schema.StepStatusApplied
			stepResults = append(stepResults, sr)
			_, _ = fmt.Fprintf(stderr, "dry-run: would un-patch %s/%s %s\n",
				step.Target.Kind, step.Target.Namespace, step.Target.Name)
			emitAudit(opts, step, sr, "rollback")
			continue
		}

		if err := sendPatch(ctx, opts.Client, step, body, ptype); err != nil {
			// A 404 here means the workload no longer exists. Treat that
			// as already-rolled-back, since the rollback's goal state is
			// "the runtime class is no longer set", and a missing object
			// satisfies that vacuously.
			if apierrors.IsNotFound(err) {
				sr.Status = schema.StepStatusAlreadyApplied
				stepResults = append(stepResults, sr)
				emitAudit(opts, step, sr, "rollback")
				continue
			}
			sr.Status = schema.StepStatusFailed
			sr.Error = err.Error()
			allSuccess[ns] = false
			_, _ = fmt.Fprintf(stderr, "rollback failed: %s/%s %s: %v\n",
				step.Target.Kind, step.Target.Namespace, step.Target.Name, err)
			stepResults = append(stepResults, sr)
			emitAudit(opts, step, sr, "rollback")
			continue
		}

		sr.Status = schema.StepStatusApplied
		stepResults = append(stepResults, sr)
		_, _ = fmt.Fprintf(stderr, "rolled back: %s/%s %s\n",
			step.Target.Kind, step.Target.Namespace, step.Target.Name)
		if opts.EmitEvents {
			emitEvent(ctx, opts.Client, step, "RolledBack", "RuntimeClass cleared")
		}
		emitAudit(opts, step, sr, "rollback")
	}

	// Clear the namespace annotation for any namespace where every step
	// rolled back successfully.
	if !opts.DryRun {
		writeNamespaceStamps(ctx, opts.Client, opts.Plan, allSuccess, stderr, "", "clear plan-hash on")
	}

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

// ---------------------------------------------------------------------------
// Helpers.
// ---------------------------------------------------------------------------

// validate checks the precondition fields of Options. Returns a non-nil
// error if any are missing.
func validate(opts Options) error {
	if opts.Client == nil {
		return fmt.Errorf("applier: opts.Client is nil")
	}
	if opts.Plan == nil {
		return fmt.Errorf("applier: opts.Plan is nil")
	}
	return nil
}

// writeNamespaceStamps writes (or clears) the plan-hash annotation on every
// namespace where every step in the plan succeeded. A namespace with even
// one failed step is intentionally NOT stamped/cleared so a re-run can
// retry. Used by both Apply (value = planHash, verb = "stamp") and Rollback
// (value = "", verb = "clear plan-hash on").
func writeNamespaceStamps(ctx context.Context, client kubernetes.Interface, plan *schema.MigrationPlan, allSuccess map[string]bool, stderr io.Writer, value, verb string) {
	for _, ns := range affectedNamespaces(plan) {
		if !allSuccess[ns] {
			continue
		}
		if _, err := writeNamespaceAnnotation(ctx, client, ns, value, false); err != nil {
			_, _ = fmt.Fprintf(stderr, "warning: could not %s namespace %s: %v\n", verb, ns, err)
		}
	}
}

// readAllNamespaceAnnotations reads the agentmoat.io/plan-hash annotation
// from every namespace touched by the plan. Returns a map keyed by
// namespace; values are the annotation string ("" if absent).
//
// A NotFound on a namespace is treated as "no annotation" (annotation map
// gets the empty string). A real error short-circuits.
func readAllNamespaceAnnotations(ctx context.Context, client kubernetes.Interface, plan *schema.MigrationPlan) (map[string]string, error) {
	out := make(map[string]string)
	for _, ns := range affectedNamespaces(plan) {
		val, err := readNamespaceAnnotation(ctx, client, ns)
		if err != nil {
			if apierrors.IsNotFound(err) {
				out[ns] = ""
				continue
			}
			return nil, err
		}
		out[ns] = val
	}
	return out, nil
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
	for i := 1; i < len(steps); i++ {
		j := i
		for j > 0 && steps[j-1].Order > steps[j].Order {
			steps[j-1], steps[j] = steps[j], steps[j-1]
			j--
		}
	}
}

// _ keeps the io import live for callers that pass opts.Stderr.
var _ io.Writer = (io.Writer)(nil)
