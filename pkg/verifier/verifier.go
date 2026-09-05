// Package verifier: the Verify entry point.
//
// Verify walks every step in the given MigrationPlan in plan order, looks
// up the live workload, and compares the live pods' .spec.runtimeClassName
// against what the plan asked for. The function is intentionally shaped
// like applier.Apply so the orchestrator can call them with parallel
// idioms (validate, walk, summarize, return).
//
// What we check, and why we check the pod and not the controller template
//
//   - When the applier patches a Deployment we patch
//     `spec.template.spec.runtimeClassName`. That field is the *intent*: a
//     rolling deploy creates new pods carrying it, but the old pods linger
//     until the rollout completes. Verifying the template only tells you
//     "the spec is good"; verifying the pods tells you "the spec took
//     effect on the live workload." Plan.md section 12.2 requires the
//     latter, so the verifier reads `pod.Spec.RuntimeClassName` and not
//     the controller's pod template.
//
//   - For controllers (Deployment / StatefulSet / DaemonSet / Job /
//     CronJob) we list the controller's selected pods via the full
//     `spec.selector` (matchLabels AND matchExpressions) and check each.
//     Pods being deleted and pods in a terminal phase (Succeeded/Failed)
//     are skipped: they carry the runtime class they were created with
//     and can never converge to the plan. CronJob is the awkward one: it
//     has no spec.selector of its own (the Jobs it spawns do). We fall
//     back to the labels on the JobTemplate's pod template, which is what
//     those Jobs end up selecting on.
//
//   - For a "Pod" PlanStep we Get the pod directly. A NotFound is the
//     verdict-bearing error; we report it and move on.
//
// Node placement (placement.go)
//
//   - After the spec-level check passes, the verifier reads the
//     RuntimeClass's scheduling.nodeSelector and the nodes the step's pods
//     run on. A hosting node whose labels do not satisfy the selector
//     demotes the step to mismatch: the spec says gVisor but the pod is on
//     a node the RuntimeClass never meant it for (typically because the
//     RuntimeClass has no selector, which `agentmoat preflight` reports).
//     The check is API-only and never fails verify by itself: when the
//     identity cannot read nodes or RuntimeClasses, NodePlacement.Checked
//     is false and the step keeps its spec-level status.
//
// In-pod probe semantics
//
//   - When opts.InPodProbe is true AND the spec-level check would have
//     said ok, we pick the first Running pod and exec a small script that
//     prints dmesg, /proc/cmdline, and uname output. If the stdout
//     contains the case-insensitive substring "gvisor", we keep the result
//     ok and annotate Probe.Detected=true. If the probe ran cleanly but
//     found no marker, we *demote* the result to mismatch: the workload's
//     spec advertises gVisor but the kernel surface inside the pod
//     disagrees. If the exec call itself errors, the result becomes error
//     (Probe.Error is populated) and Message names the exec failure.
//
//   - We probe only the first Running pod. A controller with mixed
//     populations (some pods Running, some not) is verifiable as soon as
//     one healthy sample passes; verifying every pod would multiply API
//     load with no qualitative benefit. The mismatched-pod case (one
//     Running pod missing the RuntimeClass) is already caught by the
//     spec-level loop above the probe.
package verifier

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
)

// Supported workload kinds. Kept private here because we mirror the
// applier's kind handling rather than re-export the constants.
const (
	kindPod         = "Pod"
	kindDeployment  = "Deployment"
	kindStatefulSet = "StatefulSet"
	kindDaemonSet   = "DaemonSet"
	kindJob         = "Job"
	kindCronJob     = "CronJob"
)

// probeCommand is the shell pipeline we run inside each probed pod. The
// `2>/dev/null` swallows errors when /proc/cmdline or dmesg are not
// readable (some restricted PodSecurity policies block them); the `___`
// separators delimit sections so a future enhancement could parse them.
// Today we only scan the combined stdout for the "gvisor" substring,
// which is the simplest reliable marker (gVisor's `uname -a` reports
// "Linux ... gVisor ..." and dmesg announces "Starting gVisor" early).
var probeCommand = []string{
	"sh", "-c",
	"dmesg 2>/dev/null | head -40 ; echo ___ ; cat /proc/cmdline 2>/dev/null ; echo ___ ; uname -a 2>/dev/null",
}

// probeMarker is the case-insensitive substring we scan probe output for.
// gVisor identifies itself this way in every surface we look at: dmesg,
// uname, and the kernel boot line. A single marker keeps the rule both
// simple and forgiving (the gVisor team may add markers in the future
// without breaking us).
const probeMarker = "gvisor"

// Verify is the single exported entry point of this package. It validates
// inputs, walks the plan, and returns a *schema.VerifyReport. Returns a
// non-nil Go error only on precondition failures (nil client, nil plan).
// Per-step problems are reported as Status="error" inside the per-step
// VerifyResult so the orchestrator can shape the exit code from the
// summary without distinguishing fatal from non-fatal here.
func Verify(ctx context.Context, opts Options) (*schema.VerifyReport, error) {
	if err := validate(opts); err != nil {
		return nil, err
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	// Build the report envelope eagerly so a partial failure midway still
	// produces a parseable document. NewVerifyReport stamps APIVersion /
	// Kind for us.
	report := schema.NewVerifyReport()
	report.Metadata = schema.VerifyMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          opts.Cluster,
		AgentmoatVersion: opts.AgentmoatVersion,
		PlanHash:         opts.Plan.Metadata.PlanHash,
		InPodProbe:       opts.InPodProbe,
	}

	// Lazily build the default ExecRunner if the caller asked for the
	// probe but did not supply one. Tests always supply opts.Exec, so this
	// branch only runs in production.
	probe := opts.Exec
	if opts.InPodProbe && probe == nil {
		// Without a *rest.Config we cannot dial the SPDY upgrade. Treat
		// this as a precondition failure: returning here is friendlier
		// than letting every per-step probe call fail with the same
		// transport error.
		if opts.Config == nil {
			return nil, fmt.Errorf("verifier: InPodProbe is true but opts.Config is nil and no opts.Exec was supplied")
		}
		probe = newDefaultExecRunner(opts.Client, opts.Config)
	}

	deps := stepDeps{
		client:    opts.Client,
		doProbe:   opts.InPodProbe,
		exec:      probe,
		placement: newPlacementChecker(opts.Client),
		stderr:    stderr,
	}
	results := make([]schema.VerifyResult, 0, len(opts.Plan.Spec.Steps))
	for _, step := range opts.Plan.Spec.Steps {
		results = append(results, verifyStep(ctx, deps, step))
	}

	report.Spec = schema.VerifySpec{
		Summary: summariseResults(results),
		Results: results,
	}
	return report, nil
}

// validate enforces the pre-API contract: both Client and Plan must be
// non-nil. We deliberately validate before constructing the report so
// programming errors (nil pointers) surface as Go errors and not as
// per-step Status="error" rows.
func validate(opts Options) error {
	if opts.Client == nil {
		return fmt.Errorf("verifier: opts.Client is nil")
	}
	if opts.Plan == nil {
		return fmt.Errorf("verifier: opts.Plan is nil")
	}
	return nil
}

// verifyStep does the work for one PlanStep. It is a pure function modulo
// the API client: same inputs (modulo cluster state) -> same outputs.
// Errors here become per-step Status="error" rows; Verify never returns
// a non-nil Go error from this path.
func verifyStep(ctx context.Context, deps stepDeps, step schema.PlanStep) schema.VerifyResult {
	// Expected = what the plan asked for; fall back to the project's
	// default RuntimeClass name when the step left it empty. The applier
	// uses the same fallback so the comparison is consistent on both ends.
	expected := step.RuntimeClassName
	if expected == "" {
		expected = schema.DefaultRuntimeClassName
	}

	result := schema.VerifyResult{
		Order:    step.Order,
		Target:   step.Target,
		Expected: expected,
	}

	// Resolve the live pods that this step's target governs.
	pods, err := resolvePods(ctx, deps.client, step.Target)
	if err != nil {
		result.Status = schema.VerifyStatusError
		// We surface the underlying error so an operator running with
		// --verbose sees the API server's reason (NotFound, Forbidden,
		// etc.) rather than a generic "could not resolve" string.
		result.Message = err.Error()
		_, _ = fmt.Fprintf(deps.stderr, "verify error: %s/%s %s: %v\n",
			step.Target.Kind, step.Target.Namespace, step.Target.Name, err)
		return result
	}
	if len(pods) == 0 {
		// resolvePods returned cleanly but found no pods. For a
		// controller step that means the selector matched zero live
		// pods (replicas: 0, paused rollout, all pods crash-looping
		// during a rollout). The verdict is error because the verifier
		// cannot tell from the apply-time signal alone whether the
		// rollout is in flight or genuinely empty.
		result.Status = schema.VerifyStatusError
		result.Message = fmt.Sprintf("no pods matched controller selector for %s/%s %s",
			step.Target.Kind, step.Target.Namespace, step.Target.Name)
		return result
	}

	// Spec-level check: compare every live pod's runtimeClassName against
	// Expected. The first mismatching pod wins the report; we do not list
	// every offender because the operator generally needs to fix one
	// rollout, not enumerate every replica.
	mismatch := false
	for _, pod := range pods {
		got := podRuntimeClassName(pod)
		if got != expected {
			mismatch = true
			result.Actual = got
			result.Message = fmt.Sprintf("pod %s has runtimeClassName=%q, expected %q",
				pod.Name, got, expected)
			break
		}
	}
	if !mismatch {
		// All pods reported the expected RuntimeClass name. Default the
		// Actual field to that same value so the JSON output is symmetric
		// with the mismatch case.
		result.Status = schema.VerifyStatusOK
		result.Actual = expected
	} else {
		result.Status = schema.VerifyStatusMismatch
	}

	// Node placement: only on the ok path (a wrong spec already has its
	// verdict). A hosting node outside the RuntimeClass nodeSelector means
	// the pod is not where gVisor lives, so the step becomes a mismatch.
	if result.Status == schema.VerifyStatusOK && deps.placement != nil {
		np := deps.placement.check(ctx, expected, pods)
		result.NodePlacement = np
		if np.Checked && len(np.Mismatched) > 0 {
			result.Status = schema.VerifyStatusMismatch
			result.Message = np.Message
		}
	}

	// In-pod probe: only run when we are still on the ok path. A workload
	// whose spec is already wrong does not need the probe to declare
	// failure, and we save the operator one exec call per misconfigured
	// step.
	if deps.doProbe && result.Status == schema.VerifyStatusOK {
		applyProbeResult(ctx, deps.exec, pods, &result)
	}

	return result
}

// resolvePods returns the live pods that the given target governs.
// For Kind="Pod" it returns a slice with the single pod. For controllers
// it lists pods in the controller's namespace matching the controller's
// selector. NotFound on the controller surfaces as an error from this
// function (the caller turns that into Status="error" with a friendly
// message); other API errors propagate untouched.
func resolvePods(ctx context.Context, client kubernetes.Interface, target schema.WorkloadRef) ([]corev1.Pod, error) {
	switch target.Kind {
	case kindPod:
		return resolveSinglePod(ctx, client, target)

	case kindDeployment:
		dep, err := client.AppsV1().Deployments(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return nil, controllerErr("Deployment", target, err)
		}
		return listByLabelSelector(ctx, client, target.Namespace, dep.Spec.Selector)

	case kindStatefulSet:
		ss, err := client.AppsV1().StatefulSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return nil, controllerErr("StatefulSet", target, err)
		}
		return listByLabelSelector(ctx, client, target.Namespace, ss.Spec.Selector)

	case kindDaemonSet:
		ds, err := client.AppsV1().DaemonSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return nil, controllerErr("DaemonSet", target, err)
		}
		return listByLabelSelector(ctx, client, target.Namespace, ds.Spec.Selector)

	case kindJob:
		return resolveJobPods(ctx, client, target)

	case kindCronJob:
		return resolveCronJobPods(ctx, client, target)

	default:
		return nil, fmt.Errorf("unsupported kind %q", target.Kind)
	}
}

// resolveSinglePod handles the Kind="Pod" case: fetch the one named pod.
// A NotFound gets a friendly message; other API errors propagate wrapped.
func resolveSinglePod(ctx context.Context, client kubernetes.Interface, target schema.WorkloadRef) ([]corev1.Pod, error) {
	pod, err := client.CoreV1().Pods(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("pod %s/%s not found", target.Namespace, target.Name)
		}
		return nil, fmt.Errorf("getting pod %s/%s: %w", target.Namespace, target.Name, err)
	}
	return []corev1.Pod{*pod}, nil
}

// resolveJobPods lists the pods a Job governs. Jobs always populate
// Spec.Selector when created by the Job controller (the controller-uid
// label is auto-added). If a user-built Job omits the selector we fall
// back to the template's labels, mirroring the CronJob fallback.
func resolveJobPods(ctx context.Context, client kubernetes.Interface, target schema.WorkloadRef) ([]corev1.Pod, error) {
	job, err := client.BatchV1().Jobs(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return nil, controllerErr("Job", target, err)
	}
	if hasSelectorTerms(job.Spec.Selector) {
		return listByLabelSelector(ctx, client, target.Namespace, job.Spec.Selector)
	}
	return listBySelectorMatchLabels(ctx, client, target.Namespace, job.Spec.Template.Labels)
}

// resolveCronJobPods lists the pods a CronJob's Jobs govern. CronJob is
// the awkward one: it has no .spec.selector of its own (the Jobs it spawns
// do). We could enumerate the CronJob's recent Jobs and union their
// selectors, but that doubles the API calls and races a fresh Job that the
// controller has not yet created. The simpler, deterministic approach is
// to inspect the JobTemplate's pod-template labels (which is what those
// Jobs select on by default). If the operator supplied an explicit
// JobTemplate selector we honor it first.
func resolveCronJobPods(ctx context.Context, client kubernetes.Interface, target schema.WorkloadRef) ([]corev1.Pod, error) {
	cj, err := client.BatchV1().CronJobs(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return nil, controllerErr("CronJob", target, err)
	}
	jt := cj.Spec.JobTemplate
	if hasSelectorTerms(jt.Spec.Selector) {
		return listByLabelSelector(ctx, client, target.Namespace, jt.Spec.Selector)
	}
	return listBySelectorMatchLabels(ctx, client, target.Namespace, jt.Spec.Template.Labels)
}

// hasSelectorTerms reports whether the selector carries at least one
// matchLabels or matchExpressions term. An empty selector must not be
// treated as "match everything" here (see listByLabelSelector).
func hasSelectorTerms(sel *metav1.LabelSelector) bool {
	return sel != nil && (len(sel.MatchLabels) > 0 || len(sel.MatchExpressions) > 0)
}

// controllerErr shapes a per-step error from a controller Get. We
// special-case NotFound so the message reads naturally; everything else
// is propagated with a stage prefix.
func controllerErr(kind string, target schema.WorkloadRef, err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%s %s/%s not found", kind, target.Namespace, target.Name)
	}
	return fmt.Errorf("getting %s %s/%s: %w", kind, target.Namespace, target.Name, err)
}

// listByLabelSelector lists pods in ns matching a full metav1.LabelSelector
// (matchLabels AND matchExpressions). A nil or empty selector yields the
// empty list rather than every pod in the namespace: a selector that
// matches everything would lead to absurd verify reports if a controller's
// selector is accidentally empty.
func listByLabelSelector(ctx context.Context, client kubernetes.Interface, ns string, selector *metav1.LabelSelector) ([]corev1.Pod, error) {
	if !hasSelectorTerms(selector) {
		return nil, nil
	}
	sel, err := metav1.LabelSelectorAsSelector(selector)
	if err != nil {
		return nil, fmt.Errorf("converting label selector: %w", err)
	}
	return listPodsFiltered(ctx, client, ns, sel)
}

// listBySelectorMatchLabels is the plain-matchLabels variant, used where
// only a label map is available (Job/CronJob pod-template fallbacks).
func listBySelectorMatchLabels(ctx context.Context, client kubernetes.Interface, ns string, matchLabels map[string]string) ([]corev1.Pod, error) {
	if len(matchLabels) == 0 {
		return nil, nil
	}
	return listPodsFiltered(ctx, client, ns, labels.SelectorFromSet(matchLabels))
}

// listPodsFiltered lists pods matching sel and drops the ones that cannot
// carry a verification verdict:
//
//   - Pods with a DeletionTimestamp are being removed (the old replicas
//     during a rolling update, for example). Counting them would race the
//     rollout: the new replicas already carry the patched runtimeClassName
//     while the old ones (mid-termination) still report the pre-patch spec.
//
//   - Pods in a terminal phase (Succeeded / Failed) already ran to
//     completion and will never be re-admitted. Completed Job and CronJob
//     pods linger until TTL cleanup with the runtime class they were
//     *created* with; holding them against the plan would report a
//     permanent false mismatch for a migration that is correct for every
//     future pod.
func listPodsFiltered(ctx context.Context, client kubernetes.Interface, ns string, sel labels.Selector) ([]corev1.Pod, error) {
	list, err := client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: sel.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods in %s: %w", ns, err)
	}
	alive := make([]corev1.Pod, 0, len(list.Items))
	for _, p := range list.Items {
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		alive = append(alive, p)
	}
	return alive, nil
}

// podRuntimeClassName safely extracts the live pod's runtimeClassName.
// The field is *string in client-go (it may be unset, in which case the
// pod uses the cluster default runtime). We treat unset as the empty
// string to keep the comparison rules trivial.
func podRuntimeClassName(pod corev1.Pod) string {
	if pod.Spec.RuntimeClassName == nil {
		return ""
	}
	return *pod.Spec.RuntimeClassName
}

// applyProbeResult runs the in-pod probe against the first Running pod in
// `pods` and updates `result` accordingly. Mutates result in place because
// the function may need to demote Status from ok to mismatch or error.
func applyProbeResult(ctx context.Context, exec ExecRunner, pods []corev1.Pod, result *schema.VerifyResult) {
	// Pick the first Running pod. PodRunning is the strictest standard
	// we can rely on without also waiting on Ready (which would re-do
	// the controller's wait that the applier already did). A pod that
	// has not reached Running cannot exec into anyway.
	target := pickRunningPod(pods)
	if target == nil {
		result.Status = schema.VerifyStatusError
		result.Message = "no Running pod to probe"
		return
	}

	stdout, stderrBytes, err := exec.Exec(ctx, target.Namespace, target.Name, "", probeCommand)
	probe := &schema.ProbeResult{Pod: target.Name}
	if err != nil {
		// Transport / non-zero-exit: the operator cannot tell from the
		// spec check alone whether the pod is on gVisor. Demote to error
		// so the summary surfaces "we could not verify" instead of a
		// false ok.
		probe.Error = err.Error()
		result.Probe = probe
		result.Status = schema.VerifyStatusError
		// Include stderr if the probe wrote anything useful: most of
		// the time it is empty but a Forbidden error from PodSecurity
		// would land there.
		errSummary := strings.TrimSpace(string(stderrBytes))
		if errSummary != "" {
			result.Message = fmt.Sprintf("probe exec failed: %v: %s", err, errSummary)
		} else {
			result.Message = fmt.Sprintf("probe exec failed: %v", err)
		}
		return
	}

	// Probe ran cleanly. Scan the combined stdout for the marker. We
	// lowercase once and use bytes.Contains so we do not allocate a
	// fresh string per check; the lowercase keeps the rule
	// case-insensitive even on kernels that print "GVisor" or
	// "gVisor".
	lower := bytes.ToLower(stdout)
	if bytes.Contains(lower, []byte(probeMarker)) {
		probe.Detected = true
		probe.Markers = probeMarker
		result.Probe = probe
		result.Message = "probe confirmed gVisor markers"
		// Status stays VerifyStatusOK (we only call applyProbeResult
		// when it was already ok).
		return
	}

	// Spec said gvisor, probe disagrees. This is the classic
	// "RuntimeClass is set but the pod did not actually land on a gVisor
	// node" case. Mismatch is more accurate than error because we did
	// reach a verdict: the workload is not on gVisor.
	probe.Detected = false
	result.Probe = probe
	result.Status = schema.VerifyStatusMismatch
	result.Message = "probe found no gVisor markers"
}

// pickRunningPod returns the first pod whose Status.Phase is Running, or
// nil if no such pod exists in the slice.
func pickRunningPod(pods []corev1.Pod) *corev1.Pod {
	for i := range pods {
		if pods[i].Status.Phase == corev1.PodRunning {
			return &pods[i]
		}
	}
	return nil
}

// summariseResults bucketises the per-step results into a VerifySummary.
func summariseResults(results []schema.VerifyResult) schema.VerifySummary {
	s := schema.VerifySummary{Total: len(results)}
	for _, r := range results {
		switch r.Status {
		case schema.VerifyStatusOK:
			s.OK++
		case schema.VerifyStatusMismatch:
			s.Mismatch++
		case schema.VerifyStatusError:
			s.Error++
		}
	}
	return s
}
