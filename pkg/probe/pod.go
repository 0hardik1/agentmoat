// Package probe: the probe pod manifest.
package probe

import (
	"github.com/0hardik1/agentmoat/internal/schema"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runscMountPath is where the host runsc binary appears inside the pod.
const runscMountPath = "/host-runsc"

// probeScript is what the container runs. The separator line lets the
// parser tell the version block from the driver list even if a future
// runsc prints more than two version lines.
const probeScript = runscMountPath + " --version && echo --- && " + runscMountPath + " nvproxy list-supported-drivers"

// buildPod returns the probe pod manifest for one node.
//
// Deliberate choices, each load-bearing:
//   - No runtimeClassName: the pod must run under runc so the binary it
//     executes is the host's runsc.
//   - nodeName pins the pod; the scheduler is bypassed, so a toleration for
//     every taint keeps the taint manager from evicting it on a tainted
//     gVisor node.
//   - hostPath type File on the runsc binary only, mounted read-only.
//   - Non-root, no capabilities, read-only root filesystem, RuntimeDefault
//     seccomp: runsc --version and nvproxy list-supported-drivers need none
//     of them.
//   - restartPolicy Never and activeDeadlineSeconds: the pod runs once, and
//     the kubelet kills it if the probe process itself disappears.
func buildPod(opts Options, node string) *corev1.Pod {
	deadline := int64(opts.Timeout.Seconds()) + 30
	falseVal, trueVal := false, true
	uid := int64(65534)
	grace := int64(5)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      PodName,
			Namespace: opts.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":      "agentmoat",
				"app.kubernetes.io/component": "nvproxy-probe",
			},
			Annotations: map[string]string{
				"agentmoat.io/purpose": "read the runsc version and the nvproxy supported-driver list from " + opts.RunscPath + "; created and deleted by 'agentmoat probe nvproxy'",
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
				Name:    "probe",
				Image:   opts.Image,
				Command: []string{"/bin/sh", "-c", probeScript},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("16Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"),
						corev1.ResourceMemory: resource.MustParse("64Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					AllowPrivilegeEscalation: &falseVal,
					ReadOnlyRootFilesystem:   &trueVal,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
				VolumeMounts: []corev1.VolumeMount{{Name: "runsc", MountPath: runscMountPath, ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{{
				Name: "runsc",
				VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
					Path: opts.RunscPath,
					Type: hostPathType(corev1.HostPathFile),
				}},
			}},
		},
	}
}

func hostPathType(t corev1.HostPathType) *corev1.HostPathType { return &t }

// Compile-time reminder that the pod must not request the RuntimeClass:
// the constant is referenced here only so a future edit that adds
// RuntimeClassName to buildPod has to look at this comment.
var _ = schema.DefaultRuntimeClassName
