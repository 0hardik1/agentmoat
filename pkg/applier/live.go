// Package applier: reading the class that a workload has now.
//
// A rollback after an apply that did not finish does not know which steps
// the apply got to. The plan does not say it either: a step can be for a
// workload that had another RuntimeClass before (the planner does not
// exclude one). To set runtimeClassName to null on such a workload, when the
// apply never changed it, would remove a class that agentmoat did not set.
//
// So, in a namespace that has only the apply-started mark of the plan, the
// rollback reads each workload first and un-patches it only when its pod
// template has the class of the step. That read is the only use of this
// file. In a namespace with the full stamp, every step was applied, and the
// rollback does not read.
package applier

import (
	"context"
	"fmt"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// liveRuntimeClass returns the runtimeClassName that the workload has now:
// of the Pod, or of the pod template of a controller. "" means that the
// field is not set. An API error comes back wrapped, so the caller can test
// it with apierrors.IsNotFound.
func liveRuntimeClass(ctx context.Context, client kubernetes.Interface, ref schema.WorkloadRef) (string, error) {
	get := metav1.GetOptions{}
	var spec corev1.PodSpec
	switch ref.Kind {
	case kindPod:
		obj, err := client.CoreV1().Pods(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", fmt.Errorf("getting Pod %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		spec = obj.Spec
	case kindDeployment:
		obj, err := client.AppsV1().Deployments(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", fmt.Errorf("getting Deployment %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		spec = obj.Spec.Template.Spec
	case kindStatefulSet:
		obj, err := client.AppsV1().StatefulSets(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", fmt.Errorf("getting StatefulSet %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		spec = obj.Spec.Template.Spec
	case kindDaemonSet:
		obj, err := client.AppsV1().DaemonSets(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", fmt.Errorf("getting DaemonSet %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		spec = obj.Spec.Template.Spec
	case kindJob:
		obj, err := client.BatchV1().Jobs(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", fmt.Errorf("getting Job %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		spec = obj.Spec.Template.Spec
	case kindCronJob:
		obj, err := client.BatchV1().CronJobs(ref.Namespace).Get(ctx, ref.Name, get)
		if err != nil {
			return "", fmt.Errorf("getting CronJob %s/%s: %w", ref.Namespace, ref.Name, err)
		}
		spec = obj.Spec.JobTemplate.Spec.Template.Spec
	default:
		return "", fmt.Errorf("unsupported kind %q", ref.Kind)
	}
	if spec.RuntimeClassName == nil {
		return "", nil
	}
	return *spec.RuntimeClassName, nil
}
