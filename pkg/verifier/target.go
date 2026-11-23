// Package verifier: reading one target.
//
// resolveTarget reads the workload that a plan step names, and returns what
// the verifier needs for its verdict: the live pods, the runtimeClassName of
// the controller's pod template, and whether the controller wants pods now.
//
// Why the template and "wants pods" are read, and not only the pods
//
//   - A workload can have no pods and be correct. A Deployment at
//     `replicas: 0`, a DaemonSet that selects no node, a Job that is done,
//     and a CronJob between two runs have nothing to look at. For these the
//     pod template is the only evidence, and it is good evidence: it is what
//     the next pod gets. The verdict then has Basis "template".
//
//   - A workload that wants pods and has none is a different case: a rollout
//     is in progress, or the workload is broken. That stays an error, and
//     `verify --wait` looks again.
//
//   - A CronJob is always read from its template. A Job that was created
//     before the change keeps its old pod template until it ends (the
//     template of a Job is immutable), so its pods never get the class. To
//     hold them against the plan would report a mismatch for a migration
//     that is correct for every Job from now on.
//
// A Job and a bare Pod run to an end. Two things follow, and each has a
// field here:
//
//   - A pod that ran to its end (phase Succeeded) is never Ready again and
//     has no container to exec into. Its runtimeClassName is still
//     evidence: it is the runtime that the pod ran in. The verdict is on
//     that field, with no readiness check and no probe (ended).
//   - A Job with the condition Failed has a pod template with the class,
//     and it did not come to a good end with it. With a wait time, where a
//     step is ok only when the workload came up, that is a mismatch
//     (failed).
package verifier

import (
	"context"
	"errors"
	"fmt"

	"github.com/0hardik1/agentmoat/internal/schema"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// target is what the verifier knows about one plan step after it read the
// workload.
type target struct {
	// pods are the live pods of the workload: not in deletion and not in
	// a terminal phase (see listPodsFiltered).
	pods []corev1.Pod

	// hasTemplate is true for a controller. templateClass is then the
	// runtimeClassName of its pod template ("" when the field is not
	// set). A Pod has no template.
	hasTemplate   bool
	templateClass string

	// idle is not empty when the controller wants no pods now. It holds
	// the reason in a few words, for the message: "replicas: 0".
	idle string

	// templateDecides is true for a CronJob: the verdict on the class is
	// from the template, and the pods are more evidence when they have
	// the class.
	templateDecides bool

	// bySelector is true when the pods come from the selector of a
	// manifest and the verifier did not read the object (a kind with no
	// reader). There is then no template and no idle reason: with no pods
	// the verifier does not know why there are none.
	bySelector bool

	// ended is not empty for a bare Pod in the phase Succeeded: "the pod
	// ran to its end (phase Succeeded)". The verdict is then on its
	// runtimeClassName only.
	ended string

	// failed is not empty for a Job with the condition Failed. It holds
	// the reason from the condition, for the message.
	failed string
}

// errNotFound is in the error of a read when the API server does not have
// the object. The text of the error is as before ("Job ns/name not found");
// the wrap lets the caller tell this case from other API errors.
var errNotFound = errors.New("not found")

// resolve reads the workload of one step. A target with a selector from a
// manifest is not read as an object: only its pods are listed.
func (d stepDeps) resolve(ctx context.Context, ref schema.WorkloadRef) (target, error) {
	if sel, ok := d.selectors[ref]; ok {
		pods, err := listByLabelSelector(ctx, d.client, ref.Namespace, sel)
		return target{pods: pods, bySelector: true}, err
	}
	return resolveTarget(ctx, d.client, ref)
}

// resolveTarget reads the workload that ref names. A workload that is not
// found, and any other API error, comes back as an error; the caller makes
// it a result with Status "error".
func resolveTarget(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) (target, error) {
	switch ref.Kind {
	case kindPod:
		pods, err := resolveSinglePod(ctx, client, ref)
		t := target{pods: pods}
		if len(pods) == 1 && pods[0].Status.Phase == corev1.PodSucceeded {
			t.ended = "the pod ran to its end (phase Succeeded)"
		}
		return t, err
	case kindDeployment:
		return resolveDeployment(ctx, client, ref)
	case kindStatefulSet:
		return resolveStatefulSet(ctx, client, ref)
	case kindDaemonSet:
		return resolveDaemonSet(ctx, client, ref)
	case kindJob:
		return resolveJob(ctx, client, ref)
	case kindCronJob:
		return resolveCronJob(ctx, client, ref)
	default:
		return target{}, fmt.Errorf("unsupported kind %q", ref.Kind)
	}
}

// resolveSinglePod handles the Kind="Pod" case: fetch the one named pod.
// A NotFound gets a friendly message; other API errors propagate wrapped.
func resolveSinglePod(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) ([]corev1.Pod, error) {
	pod, err := client.CoreV1().Pods(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("pod %s/%s %w", ref.Namespace, ref.Name, errNotFound)
		}
		return nil, fmt.Errorf("getting pod %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	return []corev1.Pod{*pod}, nil
}

func resolveDeployment(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) (target, error) {
	dep, err := client.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return target{}, controllerErr("Deployment", ref, err)
	}
	pods, err := listByLabelSelector(ctx, client, ref.Namespace, dep.Spec.Selector)
	return target{
		pods:          pods,
		hasTemplate:   true,
		templateClass: templateClassName(dep.Spec.Template.Spec),
		idle:          idleReplicas(dep.Spec.Replicas),
	}, err
}

func resolveStatefulSet(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) (target, error) {
	ss, err := client.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return target{}, controllerErr("StatefulSet", ref, err)
	}
	pods, err := listByLabelSelector(ctx, client, ref.Namespace, ss.Spec.Selector)
	return target{
		pods:          pods,
		hasTemplate:   true,
		templateClass: templateClassName(ss.Spec.Template.Spec),
		idle:          idleReplicas(ss.Spec.Replicas),
	}, err
}

func resolveDaemonSet(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) (target, error) {
	ds, err := client.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return target{}, controllerErr("DaemonSet", ref, err)
	}
	pods, err := listByLabelSelector(ctx, client, ref.Namespace, ds.Spec.Selector)
	t := target{pods: pods, hasTemplate: true, templateClass: templateClassName(ds.Spec.Template.Spec)}
	// A DaemonSet has no replica count. Its status says how many nodes it
	// wants a pod on. Trust that number only when the controller has seen
	// the current spec: just after a change, the status is of the spec
	// before it.
	if ds.Status.ObservedGeneration >= ds.Generation && ds.Status.DesiredNumberScheduled == 0 {
		t.idle = "the DaemonSet selects no node"
	}
	return t, err
}

// resolveJob lists the pods of a Job. A Job always has a selector in a real
// cluster (the controller-uid label is added by the API server). If a
// hand-built Job has none, fall back to the labels of its pod template, as
// for a CronJob.
func resolveJob(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) (target, error) {
	job, err := client.BatchV1().Jobs(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return target{}, controllerErr("Job", ref, err)
	}
	var pods []corev1.Pod
	if hasSelectorTerms(job.Spec.Selector) {
		pods, err = listByLabelSelector(ctx, client, ref.Namespace, job.Spec.Selector)
	} else {
		pods, err = listBySelectorMatchLabels(ctx, client, ref.Namespace, job.Spec.Template.Labels)
	}
	t := target{pods: pods, hasTemplate: true, templateClass: templateClassName(job.Spec.Template.Spec)}
	if jobIsFinished(job) {
		t.idle = "the Job is finished"
	}
	t.failed = jobFailure(job)
	return t, err
}

// resolveCronJob lists the pods that the Jobs of a CronJob govern. A CronJob
// has no selector of its own (its Jobs do). To list its recent Jobs and
// union their selectors would double the API calls and race a Job that the
// controller has not created yet. The labels of the JobTemplate's pod
// template are what those Jobs select on by default, so the verifier lists
// by them. An explicit selector in the JobTemplate comes first.
func resolveCronJob(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) (target, error) {
	cj, err := client.BatchV1().CronJobs(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return target{}, controllerErr("CronJob", ref, err)
	}
	jt := cj.Spec.JobTemplate
	var pods []corev1.Pod
	if hasSelectorTerms(jt.Spec.Selector) {
		pods, err = listByLabelSelector(ctx, client, ref.Namespace, jt.Spec.Selector)
	} else {
		pods, err = listBySelectorMatchLabels(ctx, client, ref.Namespace, jt.Spec.Template.Labels)
	}
	return target{
		pods:            pods,
		hasTemplate:     true,
		templateClass:   templateClassName(jt.Spec.Template.Spec),
		idle:            "no Job of the CronJob runs now",
		templateDecides: true,
	}, err
}

// templateClassName is the runtimeClassName of a pod template, "" when the
// field is not set.
func templateClassName(spec corev1.PodSpec) string {
	if spec.RuntimeClassName == nil {
		return ""
	}
	return *spec.RuntimeClassName
}

// idleReplicas returns the idle reason for a replica count of zero. A nil
// count is the default of one.
func idleReplicas(replicas *int32) string {
	if replicas != nil && *replicas == 0 {
		return "replicas: 0"
	}
	return ""
}

// jobFailure returns "the Job failed (<reason>: <message>)" for a Job with
// the condition Failed, and "" for each other Job. The reason and the
// message are the ones of the condition (BackoffLimitExceeded,
// DeadlineExceeded).
func jobFailure(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return fmt.Sprintf("the Job failed (%s: %s)", c.Reason, c.Message)
		}
	}
	return ""
}

// jobIsFinished reports whether the Job has the condition Complete or
// Failed. A Job that is new and has no pod yet is not finished: it wants a
// pod, and the verifier must not read "no pods" as "nothing to check".
func jobIsFinished(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
