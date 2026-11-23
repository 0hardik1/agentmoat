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
//   - There are two exceptions, and both are in target.go. A controller
//     that wants no pods now (replicas: 0, a Job that is done) has no pods
//     to read, so its pod template gives the verdict. And a CronJob is
//     always read from its template: a Job from before the change keeps
//     its old template until it ends.
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
// The wait (wait.go, readiness.go)
//
//   - With opts.Wait, Verify looks again until every step is ok or the
//     time ends. A step is then ok only when its pods are also Ready and
//     stay Ready. This is the form for a pipeline, which runs verify
//     directly after apply, in the middle of the rollout.
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
	"io"
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
		client:       opts.Client,
		doProbe:      opts.InPodProbe,
		exec:         probe,
		placement:    newPlacementChecker(opts.Client),
		stderr:       stderr,
		requireReady: opts.Wait > 0,
	}
	steps := opts.Plan.Spec.Steps
	var results []schema.VerifyResult
	if opts.Wait > 0 {
		// Look again until every step is ok or the time ends (wait.go).
		waited, wait, err := waitForSteps(ctx, deps, steps, newWaitConfig(opts))
		if err != nil {
			return nil, err
		}
		results = waited
		report.Metadata.Wait = wait
	} else {
		results = make([]schema.VerifyResult, 0, len(steps))
		for _, step := range steps {
			results = append(results, verifyStep(ctx, deps, step))
		}
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
	if opts.Wait < 0 {
		return fmt.Errorf("verifier: the wait time is %s; it must be zero or more", opts.Wait)
	}
	return nil
}

// verifyStep does the work for one PlanStep and returns its result. Errors
// here become per-step Status="error" rows; Verify never returns a non-nil
// Go error from this path.
func verifyStep(ctx context.Context, deps stepDeps, step schema.PlanStep) schema.VerifyResult {
	check := checkStep(ctx, deps, step)
	check.reportReadError(deps.stderr)
	return check.result
}

// stepCheck is the result of one look at one step, with the pods that the
// result is about. The wait loop needs the pods: "Ready" counts only when
// the same pods stay Ready (see wait.go).
type stepCheck struct {
	result schema.VerifyResult
	pods   []corev1.Pod

	// readErr is the error from the read of the workload, when that read
	// failed (not found, forbidden, a network error). The caller prints
	// it: one time for one look, and with a wait only when it changes, so
	// that a workload that is not there does not print a line in each
	// round.
	readErr error
}

// reportReadError prints the read error of this look on stderr, if there is
// one. An operator sees the reason of the API server (NotFound, Forbidden)
// as it occurs, and not only in the report at the end.
func (c stepCheck) reportReadError(stderr io.Writer) {
	if c.readErr == nil {
		return
	}
	t := c.result.Target
	_, _ = fmt.Fprintf(stderr, "verify error: %s/%s %s: %v\n", t.Kind, t.Namespace, t.Name, c.readErr)
}

// checkStep looks at one step one time. It is a pure function modulo the
// API client: same cluster state in, same result out. The checks run in
// this order, and each one runs only when the ones before it passed:
//
//  1. The class: every live pod has the expected runtimeClassName (or, for
//     a workload with no pods now, its pod template does).
//  2. Node placement: the pods run on nodes that the RuntimeClass selects.
//  3. Readiness: the pods are Ready. Only with a wait time.
//  4. The in-pod probe: gVisor markers in one Running pod. Only on request.
//     With a wait time the wait loop runs it, one time, when the pods have
//     settled (wait.go); a probe in each round would be an exec call into
//     the workload every few seconds.
func checkStep(ctx context.Context, deps stepDeps, step schema.PlanStep) stepCheck {
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

	t, err := resolveTarget(ctx, deps.client, step.Target)
	if err != nil {
		result.Status = schema.VerifyStatusError
		// We surface the underlying error so an operator sees the API
		// server's reason (NotFound, Forbidden, etc.) rather than a
		// generic "could not resolve" string. The caller prints it.
		result.Message = err.Error()
		return stepCheck{result: result, readErr: err}
	}

	pods := classVerdict(t, &result)
	if result.Status == schema.VerifyStatusMismatch && deps.requireReady {
		explainStuckRollout(t, &result)
	}
	if result.Status != schema.VerifyStatusOK || len(pods) == 0 {
		// A verdict with no pods behind it: a mismatch, an error, or an
		// ok from the pod template. There is nothing more to look at.
		return stepCheck{result: result}
	}

	// Node placement: a hosting node outside the RuntimeClass nodeSelector
	// means the pod is not where gVisor lives, so the step becomes a
	// mismatch.
	if deps.placement != nil {
		np := deps.placement.check(ctx, expected, pods)
		result.NodePlacement = np
		if np.Checked && len(np.Mismatched) > 0 {
			result.Status = schema.VerifyStatusMismatch
			result.Message = np.Message
			return stepCheck{result: result, pods: pods}
		}
	}

	// Readiness: only with a wait time. See readiness.go.
	if deps.requireReady {
		result.Readiness = podReadiness(pods)
		if result.Readiness.Ready < result.Readiness.Pods {
			result.Status = schema.VerifyStatusMismatch
			result.Message = result.Readiness.Message
			return stepCheck{result: result, pods: pods}
		}
	}

	// In-pod probe: a workload whose spec is already wrong does not need
	// the probe to declare failure, and we save the operator one exec call
	// per misconfigured step. With a wait time, the wait loop runs the
	// probe (see settleStep).
	if deps.doProbe && !deps.requireReady {
		applyProbeResult(ctx, deps.exec, pods, &result)
	}
	return stepCheck{result: result, pods: pods}
}

// explainStuckRollout adds to a class mismatch the reason that a wait most
// often ends on one. A rolling update keeps the old pods until the new pods
// are Ready. When the new pods do not start in gVisor, the old pods stay,
// and the class check reports one of them: "pod web-old has
// runtimeClassName="", expected "gvisor"". That is true, and it does not
// tell the operator what is wrong. The pods that have the class do: this
// function adds their readiness to the result, and the reason of the first
// one that is not Ready to the message.
//
// It adds nothing when no pod has the class yet, and nothing when the pods
// that have it are all Ready (the rollout is in progress and is healthy).
func explainStuckRollout(t target, result *schema.VerifyResult) {
	var current []corev1.Pod
	for _, pod := range t.pods {
		if podRuntimeClassName(pod) == result.Expected {
			current = append(current, pod)
		}
	}
	if len(current) == 0 || len(current) == len(t.pods) {
		return
	}
	readiness := podReadiness(current)
	if readiness.Ready == readiness.Pods {
		return
	}
	result.Readiness = readiness
	result.Message += "; and " + readiness.Message
}

// classVerdict sets the verdict on the runtimeClassName: Status, Actual,
// Message, and Basis of result. It returns the pods that the checks after
// it look at; none when the verdict is not ok, and none when the verdict is
// from the pod template.
func classVerdict(t target, result *schema.VerifyResult) []corev1.Pod {
	if t.templateDecides {
		return cronJobVerdict(t, result)
	}
	if len(t.pods) == 0 {
		if t.hasTemplate && t.idle != "" {
			templateVerdict(t, result, t.idle)
			return nil
		}
		// The controller wants pods and has none (a rollout that is in
		// progress, all pods in a crash loop and in deletion). The
		// verifier cannot give a verdict on pods that are not there.
		result.Status = schema.VerifyStatusError
		result.Message = fmt.Sprintf("no pods matched controller selector for %s/%s %s",
			result.Target.Kind, result.Target.Namespace, result.Target.Name)
		return nil
	}

	// Compare every live pod's runtimeClassName against Expected. The
	// first mismatching pod wins the report; we do not list every
	// offender because the operator generally needs to fix one rollout,
	// not enumerate every replica.
	for _, pod := range t.pods {
		if got := podRuntimeClassName(pod); got != result.Expected {
			result.Status = schema.VerifyStatusMismatch
			result.Actual = got
			result.Message = fmt.Sprintf("pod %s has runtimeClassName=%q, expected %q",
				pod.Name, got, result.Expected)
			return nil
		}
	}
	// Default the Actual field to the expected value so the JSON output
	// is symmetric with the mismatch case.
	result.Status = schema.VerifyStatusOK
	result.Actual = result.Expected
	return t.pods
}

// templateVerdict is the verdict for a workload with no pods to look at:
// the runtimeClassName of its pod template against Expected.
func templateVerdict(t target, result *schema.VerifyResult, why string) {
	result.Basis = schema.VerifyBasisTemplate
	result.Actual = t.templateClass
	if t.templateClass != result.Expected {
		result.Status = schema.VerifyStatusMismatch
		result.Message = fmt.Sprintf("the pod template has runtimeClassName=%q, expected %q", t.templateClass, result.Expected)
		return
	}
	result.Status = schema.VerifyStatusOK
	result.Message = fmt.Sprintf("no pods to check (%s); the pod template has runtimeClassName=%q", why, t.templateClass)
}

// cronJobVerdict is classVerdict for a CronJob. The template decides (see
// target.go). The live pods that have the class go on to the checks after
// this one. The live pods that do not have it are of a Job from before the
// change; the message counts them, and the verdict does not.
func cronJobVerdict(t target, result *schema.VerifyResult) []corev1.Pod {
	if t.templateClass != result.Expected {
		templateVerdict(t, result, t.idle)
		return nil
	}
	current := make([]corev1.Pod, 0, len(t.pods))
	for _, pod := range t.pods {
		if podRuntimeClassName(pod) == result.Expected {
			current = append(current, pod)
		}
	}
	earlier := ""
	if n := len(t.pods) - len(current); n > 0 {
		earlier = fmt.Sprintf("%d pod(s) of a Job from before the change do not have runtimeClassName=%q; they end with that Job",
			n, result.Expected)
	}
	if len(current) == 0 {
		templateVerdict(t, result, t.idle)
		if earlier != "" {
			result.Message = fmt.Sprintf("the pod template has runtimeClassName=%q; %s", t.templateClass, earlier)
		}
		return nil
	}
	result.Status = schema.VerifyStatusOK
	result.Actual = result.Expected
	result.Message = earlier
	return current
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
