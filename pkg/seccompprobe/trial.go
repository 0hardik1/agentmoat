// Package seccompprobe: the trial and reader pod manifests.
package seccompprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Labels and annotations every pod of the probe carries.
const (
	labelName       = "app.kubernetes.io/name"
	labelComponent  = "app.kubernetes.io/component"
	labelRunID      = "agentmoat.io/seccomp-probe-run"
	annotationTrial = "agentmoat.io/seccomp-trial-of"
	annotationWhy   = "agentmoat.io/purpose"

	componentTrial  = "seccomp-trial"
	componentReader = "seccomp-reader"

	// trialGrace replaces the workload's termination grace period: the
	// trial is deleted after its window, and waiting minutes for a
	// graceful shutdown would only delay the log read.
	trialGrace = int64(5)
)

// trialName is deterministic for a workload and a run: the first 8 hex
// digits of sha256("kind/namespace/name"), then the run ID. Short enough
// for a hostname, and the run ID suffix is what the reader matches on.
func trialName(ref schema.WorkloadRef, runID string) string {
	sum := sha256.Sum256([]byte(ref.Kind + "/" + ref.Namespace + "/" + ref.Name))
	return "agentmoat-seccomp-" + hex.EncodeToString(sum[:4]) + "-" + runID
}

// readerName is the reader pod for one node in one run.
func readerName(node, runID string) string {
	sum := sha256.Sum256([]byte(node))
	return "agentmoat-seccomp-read-" + hex.EncodeToString(sum[:4]) + "-" + runID
}

// buildTrial returns the trial pod for one workload, and the list of
// changes it made to the pod template. What it keeps is the point: the
// containers, their images, commands, environment, service account and,
// above all, every seccomp profile and capability, exactly as the workload
// sets them, so the filter runsc installs is the one the migrated workload
// will get. What it changes, it changes so the trial can run next to the
// real pods without joining them:
//
//   - runtimeClassName is the audit class, whose handler runs runsc with
//     oci-seccomp on and the Sentry debug log the reader reads.
//   - None of the workload's labels: Services, controllers, PDBs and
//     NetworkPolicies select on them, and a trial must not receive
//     traffic or be adopted. Only agentmoat's labels, plus an annotation
//     naming the workload. (A NetworkPolicy that allows the workload's
//     egress by its labels therefore does not apply to the trial.)
//   - restartPolicy Never and activeDeadlineSeconds: a container that
//     exits is reported, not restarted, and the kubelet ends the trial if
//     the probe process disappears.
//   - nodeName, hostname and subdomain are cleared (a standalone Pod's
//     spec has them set from its first run).
//   - A persistentVolumeClaim volume, and a volume a container mounts
//     that the template does not define (a StatefulSet's
//     volumeClaimTemplates), become emptyDir: a trial must not write to
//     the workload's data, and a ReadWriteOnce claim is already attached.
//   - The termination grace period is 5 seconds.
func buildTrial(spec corev1.PodSpec, ref schema.WorkloadRef, o Options, runID string) (*corev1.Pod, []string) {
	s := *spec.DeepCopy()
	var changes []string

	s.RuntimeClassName = &o.AuditRuntimeClassName
	if s.RestartPolicy != corev1.RestartPolicyNever {
		changes = append(changes, fmt.Sprintf("restartPolicy: %s replaced by Never", orDefault(string(s.RestartPolicy), "Always")))
		s.RestartPolicy = corev1.RestartPolicyNever
	}
	deadline := int64((o.Timeout + o.RunFor).Seconds()) + 60
	s.ActiveDeadlineSeconds = &deadline
	grace := trialGrace
	s.TerminationGracePeriodSeconds = &grace
	if s.NodeName != "" {
		changes = append(changes, "nodeName "+s.NodeName+" cleared")
		s.NodeName = ""
	}
	if s.Hostname != "" || s.Subdomain != "" {
		changes = append(changes, "hostname and subdomain cleared")
		s.Hostname, s.Subdomain = "", ""
	}
	s.EphemeralContainers = nil
	changes = append(changes, replaceClaims(&s)...)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      trialName(ref, runID),
			Namespace: ref.Namespace,
			Labels: map[string]string{
				labelName: "agentmoat", labelComponent: componentTrial, labelRunID: runID,
			},
			Annotations: map[string]string{
				annotationTrial: ref.Kind + "/" + ref.Name,
				annotationWhy: "seccomp trial of " + ref.Kind + " " + ref.Namespace + "/" + ref.Name +
					" under RuntimeClass " + o.AuditRuntimeClassName + "; created and deleted by 'agentmoat probe seccomp'",
			},
		},
		Spec: s,
	}
	return pod, changes
}

// replaceClaims swaps claim-backed volumes for emptyDir and adds an
// emptyDir for every mounted name the template does not define.
func replaceClaims(s *corev1.PodSpec) []string {
	var changes []string
	defined := map[string]bool{}
	for i := range s.Volumes {
		v := &s.Volumes[i]
		defined[v.Name] = true
		switch {
		case v.PersistentVolumeClaim != nil:
			changes = append(changes, fmt.Sprintf("volume %s: persistentVolumeClaim %s replaced by emptyDir", v.Name, v.PersistentVolumeClaim.ClaimName))
			v.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		case v.Ephemeral != nil:
			changes = append(changes, fmt.Sprintf("volume %s: ephemeral volume replaced by emptyDir", v.Name))
			v.VolumeSource = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		}
	}
	missing := map[string]bool{}
	for _, c := range append(append([]corev1.Container{}, s.InitContainers...), s.Containers...) {
		for _, m := range c.VolumeMounts {
			if !defined[m.Name] {
				missing[m.Name] = true
			}
		}
	}
	names := make([]string, 0, len(missing))
	for n := range missing {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		changes = append(changes, fmt.Sprintf("volume %s: not in the pod template (a volumeClaimTemplate?), added as emptyDir", n))
		s.Volumes = append(s.Volumes, corev1.Volume{Name: n, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	return changes
}

// buildReader returns the reader pod for one node. Like the nvproxy probe
// pod it runs under runc (no runtimeClassName), pinned to the node,
// tolerating every taint, non-root, with one read-only hostPath of type
// Directory: the Sentry log directory. Type Directory means the pod cannot
// create the directory on a node that lacks it; it stays in
// ContainerCreating and the probe reports that.
func buildReader(o Options, node, runID string) *corev1.Pod {
	falseVal, trueVal := false, true
	uid := int64(65534)
	grace := int64(5)
	deadline := int64(o.Timeout.Seconds()) + 30
	dir := corev1.HostPathDirectory
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      readerName(node, runID),
			Namespace: o.ReaderNamespace,
			Labels: map[string]string{
				labelName: "agentmoat", labelComponent: componentReader, labelRunID: runID,
			},
			Annotations: map[string]string{
				annotationWhy: "count seccomp denials in the gVisor Sentry logs under " + o.LogDir +
					" for run " + runID + "; created and deleted by 'agentmoat probe seccomp'",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:                      node,
			RestartPolicy:                 corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:         &deadline,
			TerminationGracePeriodSeconds: &grace,
			AutomountServiceAccountToken:  &falseVal,
			Tolerations:                   []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   &trueVal,
				RunAsUser:      &uid,
				RunAsGroup:     &uid,
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			Containers: []corev1.Container{{
				Name:    "reader",
				Image:   o.ReaderImage,
				Command: []string{"/bin/sh", "-c", readerScript},
				Env: []corev1.EnvVar{
					{Name: "LOG_DIR", Value: readerMountPath},
					{Name: "RUN_ID", Value: runID},
				},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("16Mi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseVal,
					ReadOnlyRootFilesystem:   &trueVal,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "sentry-logs", MountPath: readerMountPath, ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{
				Name:         "sentry-logs",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: o.LogDir, Type: &dir}},
			}},
		},
	}
}

// readerMountPath is where the log directory appears in the reader pod.
const readerMountPath = "/sentry-logs"

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
