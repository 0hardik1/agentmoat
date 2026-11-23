// Package verifier: pod readiness.
//
// With a wait time (`verify --wait`), a step is ok only when its pods are
// Ready. The reason is the pipeline that the wait is for:
//
//	apply --dry-run=false  ->  verify --wait 3m  ->  on failure: rollback
//
// A pod can have runtimeClassName=gvisor and never run in the sandbox: the
// program uses something that gVisor does not have and stops at start, or
// the node has no runsc. With a rolling update the old pods stay until the
// new ones are Ready, so the class check alone sees the problem (the old
// pods do not have the class). With a StatefulSet, a Recreate Deployment, or
// a DaemonSet, the old pod is deleted first. Then the only live pod has the
// class and is in a crash loop, and the class check alone says ok for a
// workload that is down.
//
// Without a wait time the verifier looks one time and does not read
// readiness: a pod that is still starting is the normal state just after an
// apply, and to fail on it would make every verify without a wait fail.
//
// This file reads the Ready condition and says why a pod is not Ready. The
// rule that Ready must hold for some time is in wait.go.
package verifier

import (
	"fmt"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
)

const (
	// maxNotReadyNames is the most pod names that PodReadiness.NotReady
	// holds. A DaemonSet on a large cluster can have hundreds of pods.
	maxNotReadyNames = 10

	// maxReasonLen is the most characters of a message from the cluster
	// (a scheduler message can list every node) in a not-Ready reason.
	maxReasonLen = 200
)

// podReadiness counts the pods that are Ready and says why the first one
// that is not Ready (by name) is not.
func podReadiness(pods []corev1.Pod) *schema.PodReadiness {
	r := &schema.PodReadiness{Pods: len(pods)}
	var notReady []corev1.Pod
	for _, pod := range pods {
		if podIsReady(pod) {
			r.Ready++
			continue
		}
		notReady = append(notReady, pod)
	}
	if len(notReady) == 0 {
		return r
	}
	sort.Slice(notReady, func(i, j int) bool { return notReady[i].Name < notReady[j].Name })
	for i, pod := range notReady {
		if i == maxNotReadyNames {
			break
		}
		r.NotReady = append(r.NotReady, pod.Name)
	}
	first := notReady[0]
	r.Message = fmt.Sprintf("pod %s is not Ready: %s", first.Name, notReadyReason(first))
	if more := len(notReady) - 1; more > 0 {
		r.Message += fmt.Sprintf(" (and %d more pod(s) not Ready)", more)
	}
	return r
}

// podIsReady reports whether the pod has the condition Ready=True.
func podIsReady(pod corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// notReadyReason says, in a few words, why a pod is not Ready. It reads the
// most specific source first:
//
//  1. A container that waits or stopped (init containers first): the reason
//     that the kubelet gives, for example CrashLoopBackOff or
//     ImagePullBackOff. This is the line that tells an operator "it does
//     not start in gVisor".
//  2. A pod that is not scheduled: the message of the scheduler.
//  3. The Ready condition itself.
func notReadyReason(pod corev1.Pod) string {
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			return fmt.Sprintf("container %s is waiting: %s%s", cs.Name, w.Reason, restarts(cs))
		}
		if t := cs.State.Terminated; t != nil && t.ExitCode != 0 {
			return fmt.Sprintf("container %s stopped: %s, exit code %d%s", cs.Name, orUnknown(t.Reason), t.ExitCode, restarts(cs))
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse {
			return shorten(fmt.Sprintf("not scheduled (%s): %s", orUnknown(c.Reason), c.Message))
		}
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Reason != "" {
			return shorten(strings.TrimSuffix(fmt.Sprintf("%s: %s", c.Reason, c.Message), ": "))
		}
	}
	return fmt.Sprintf("phase %s", orUnknown(string(pod.Status.Phase)))
}

// restarts adds the restart count of a container to a reason, when it is
// not zero. "CrashLoopBackOff, 7 restarts" reads differently from a first
// start that is slow.
func restarts(cs corev1.ContainerStatus) string {
	if cs.RestartCount == 0 {
		return ""
	}
	return fmt.Sprintf(", %d restart(s)", cs.RestartCount)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// shorten cuts a message from the cluster to maxReasonLen characters.
func shorten(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= maxReasonLen {
		return s
	}
	return string(runes[:maxReasonLen-1]) + "…"
}

// podsFingerprint identifies a set of pods and their restart counts. Two
// looks that give the same fingerprint saw the same pods, and no container
// in them started again between the looks.
func podsFingerprint(pods []corev1.Pod) string {
	parts := make([]string, 0, len(pods))
	for _, pod := range pods {
		var restartCount int32
		for _, cs := range pod.Status.InitContainerStatuses {
			restartCount += cs.RestartCount
		}
		for _, cs := range pod.Status.ContainerStatuses {
			restartCount += cs.RestartCount
		}
		// The UID changes when a pod is replaced by one with the same
		// name (a StatefulSet pod). The name is there for a fake
		// clientset, which gives no UID.
		parts = append(parts, fmt.Sprintf("%s/%s/%s#%d", pod.Namespace, pod.Name, pod.UID, restartCount))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
