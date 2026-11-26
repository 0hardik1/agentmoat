// Package classifier: the seccomp-unconfined rule and the helper that reads
// each container's seccomp profile.
//
// Why a rule for this at all. Until gVisor release 20261123.1, runsc did
// not load a container's OCI seccomp filter inside the sandbox unless the
// runtime was configured with --oci-seccomp, so the profile in the pod spec
// made no difference to what a workload under runsc could call. From
// 20261123.1 the flag defaults to true: a RuntimeDefault or Localhost
// profile now filters syscalls inside the sandbox, and Unconfined is the
// only way to opt out. A migration that carries Unconfined into gVisor is
// therefore a choice the operator should see, not an accident of the old
// default. Source: google/gvisor runsc/config/flags.go at tag
// release-20261123.1 (the default of flagOCISeccomp changed from false to
// true; it is false at release-20261123.0).
//
// The rule is info severity: it changes neither the verdict nor whether the
// workload is in a plan. gVisor still isolates an Unconfined workload; the
// filter is a second layer. An operator who wants it to block can raise it
// in the YAML overrides (internal/rules/gvisor.yaml).
//
// What counts. Only an explicit Unconfined, from securityContext.seccompProfile,
// container setting first, then the pod's. A container with no profile at
// either level ("Unset") is not flagged: what it gets depends on the
// kubelet (Unconfined unless the kubelet runs with seccompDefault: true),
// which the pod spec cannot show. The deprecated seccomp annotations are
// not read.
package classifier

import (
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
)

// RuleSeccompUnconfined is the ID of the rule, used by the planner's
// warning and the explainer's evidence too.
const RuleSeccompUnconfined = "seccomp-unconfined"

// SeccompProfiles returns the seccomp profile each container of spec asks
// for: init containers first, then containers, in spec order. A container's
// own securityContext.seccompProfile wins over the pod's; with neither, the
// type is schema.SeccompProfileUnset.
func SeccompProfiles(spec corev1.PodSpec) []schema.ContainerSeccompProfile {
	var pod *corev1.SeccompProfile
	if spec.SecurityContext != nil {
		pod = spec.SecurityContext.SeccompProfile
	}
	containers := allContainers(spec)
	out := make([]schema.ContainerSeccompProfile, 0, len(containers))
	for _, c := range containers {
		p := pod
		if c.SecurityContext != nil && c.SecurityContext.SeccompProfile != nil {
			p = c.SecurityContext.SeccompProfile
		}
		out = append(out, profileOf(c.Name, p))
	}
	return out
}

func profileOf(container string, p *corev1.SeccompProfile) schema.ContainerSeccompProfile {
	if p == nil || p.Type == "" {
		return schema.ContainerSeccompProfile{Container: container, Type: schema.SeccompProfileUnset}
	}
	out := schema.ContainerSeccompProfile{Container: container, Type: string(p.Type)}
	if p.Type == corev1.SeccompProfileTypeLocalhost && p.LocalhostProfile != nil {
		out.LocalhostProfile = *p.LocalhostProfile
	}
	return out
}

// UnconfinedContainers returns the names of the containers whose effective
// profile is Unconfined, in spec order.
func UnconfinedContainers(spec corev1.PodSpec) []string {
	var out []string
	for _, p := range SeccompProfiles(spec) {
		if p.Type == schema.SeccompProfileUnconfined {
			out = append(out, p.Container)
		}
	}
	return out
}

func matchSeccompUnconfined(w scanner.Workload) bool {
	return len(UnconfinedContainers(w.PodSpec)) > 0
}
