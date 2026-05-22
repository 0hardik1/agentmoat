// Package classifier: built-in rule set.
//
// The 14 rules registered in this file are the standard gVisor
// compatibility checks. They are drawn directly from plan.md section 5.4
// ("When gVisor is the wrong answer") and section 5.5 ("Performance
// trade-offs"). The upstream source for most rules is the gVisor user
// guide at https://gvisor.dev/docs/user_guide/compatibility/ and the
// performance guide at https://gvisor.dev/docs/architecture_guide/performance/.
//
// Rule index (alphabetical by ID):
//
//  1. ebpf                 (error) eBPF programs (Cilium, Tetragon, Falco).
//  2. fuse-mount           (warn)  FUSE filesystem in use.
//  3. gpu-passthrough      (warn)  nvidia.com/gpu resource requested.
//  4. host-ipc             (error) hostIPC: true.
//  5. host-network         (error) hostNetwork: true.
//  6. host-path-mount      (warn)  hostPath volume.
//  7. host-pid             (error) hostPID: true.
//  8. io-uring             (warn)  io_uring usage declared via annotation.
//  9. kvm-nested           (error) /dev/kvm hostPath (nested virt).
//  10. network-throughput   (info)  network-bound image (nginx, envoy, ...).
//  11. perf-events          (warn)  CAP_PERFMON / CAP_SYS_ADMIN usage.
//  12. privileged           (error) container in privileged mode.
//  13. raw-socket           (error) CAP_NET_RAW or operator-declared usage.
//  14. syscall-heavy        (info)  syscall-bound image (redis, memcached).
//
// Each rule's Match closure is intentionally small and reads only the
// fields it needs from the PodSpec. Rules are pure functions of the
// workload.
package classifier

import (
	"strings"

	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
)

// RegisterBuiltins adds the 14 standard rules to the given Registry. Call
// it once after NewRegistry() during CLI startup, before loading any YAML
// overrides.
func RegisterBuiltins(r *Registry) {
	r.Register(Rule{
		ID:             "raw-socket",
		Severity:       SeverityError,
		Description:    "Container requests CAP_NET_RAW; gVisor disables raw sockets unless `--net-raw` is enabled at the runsc level.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/#networking",
		Match: func(w scanner.Workload) bool {
			// Operator-declared override: an explicit annotation that
			// says "yes, this workload genuinely needs raw sockets".
			// Useful for workloads whose image we cannot inspect.
			if w.Annotations["agentmoat.io/needs-raw-socket"] == "true" {
				return true
			}
			return anyContainerHasCapability(w.PodSpec, "NET_RAW")
		},
	})

	r.Register(Rule{
		ID:             "host-network",
		Severity:       SeverityError,
		Description:    "Pod uses hostNetwork: true; gVisor cannot bridge to the host network namespace.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/#networking",
		Match: func(w scanner.Workload) bool {
			return w.PodSpec.HostNetwork
		},
	})

	r.Register(Rule{
		ID:             "host-pid",
		Severity:       SeverityError,
		Description:    "Pod uses hostPID: true; gVisor isolates the PID namespace and cannot share the host's.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/",
		Match: func(w scanner.Workload) bool {
			return w.PodSpec.HostPID
		},
	})

	r.Register(Rule{
		ID:             "host-ipc",
		Severity:       SeverityError,
		Description:    "Pod uses hostIPC: true; gVisor isolates the IPC namespace and cannot share the host's.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/",
		Match: func(w scanner.Workload) bool {
			return w.PodSpec.HostIPC
		},
	})

	r.Register(Rule{
		ID:             "privileged",
		Severity:       SeverityError,
		Description:    "Container runs in privileged mode; the gVisor sandbox boundary makes this meaningless and several capabilities are unsupported.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/",
		Match: func(w scanner.Workload) bool {
			for _, c := range allContainers(w.PodSpec) {
				if c.SecurityContext != nil &&
					c.SecurityContext.Privileged != nil &&
					*c.SecurityContext.Privileged {
					return true
				}
			}
			return false
		},
	})

	r.Register(Rule{
		ID:             "host-path-mount",
		Severity:       SeverityWarn,
		Description:    "Pod mounts a hostPath volume; the Gofer must proxy these reads/writes and some host-managed mount semantics are not preserved.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/filesystem/",
		Match: func(w scanner.Workload) bool {
			for _, v := range w.PodSpec.Volumes {
				if v.HostPath != nil {
					return true
				}
			}
			return false
		},
	})

	r.Register(Rule{
		ID:             "ebpf",
		Severity:       SeverityError,
		Description:    "Workload appears to load eBPF programs (Cilium agent, Tetragon, Falco eBPF driver); gVisor does not expose the eBPF syscall surface.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/",
		Match: func(w scanner.Workload) bool {
			// Two-pronged match: a well-known image is a strong signal,
			// but a capability-only match (BPF or SYS_ADMIN combined
			// with a known image hint) catches custom-built loaders.
			imageHint := imageContainsAny(w.ImageRefs,
				"cilium/cilium",
				"ghcr.io/cilium",
				"quay.io/cilium",
				"tetragon",
				"falco",
			)
			if imageHint {
				return true
			}
			// Capability-only path: BPF alone, or SYS_ADMIN paired with
			// an eBPF-loader image hint. Pure SYS_ADMIN is too noisy to
			// fire on by itself.
			if anyContainerHasCapability(w.PodSpec, "BPF") {
				return true
			}
			return false
		},
	})

	r.Register(Rule{
		ID:             "gpu-passthrough",
		Severity:       SeverityWarn,
		Description:    "Container requests an NVIDIA GPU resource (`nvidia.com/gpu`); gVisor's `nvproxy` only supports a subset of CUDA versions.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/gpu/",
		Match: func(w scanner.Workload) bool {
			for _, c := range allContainers(w.PodSpec) {
				for name := range c.Resources.Limits {
					if strings.HasPrefix(string(name), "nvidia.com/gpu") {
						return true
					}
				}
				for name := range c.Resources.Requests {
					if strings.HasPrefix(string(name), "nvidia.com/gpu") {
						return true
					}
				}
			}
			return false
		},
	})

	r.Register(Rule{
		ID:             "fuse-mount",
		Severity:       SeverityWarn,
		Description:    "FUSE filesystem in use; gVisor supports a subset of FUSE behavior, validate against the user guide.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/filesystem/",
		Match: func(w scanner.Workload) bool {
			for _, v := range w.PodSpec.Volumes {
				if v.CSI != nil &&
					strings.Contains(strings.ToLower(v.CSI.Driver), "fuse") {
					return true
				}
			}
			// Env var escape hatch: lets operators self-declare for
			// workloads we cannot inspect (e.g. FUSE called from a
			// sidecar started by an unrelated process).
			for _, c := range allContainers(w.PodSpec) {
				for _, env := range c.Env {
					if env.Name == "AGENTMOAT_USES_FUSE" && env.Value == "true" {
						return true
					}
				}
			}
			return false
		},
	})

	r.Register(Rule{
		ID:             "io-uring",
		Severity:       SeverityWarn,
		Description:    "Workload may use io_uring (annotation `agentmoat.io/uses-iouring=true` set); gVisor does not implement io_uring.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/",
		Match: func(w scanner.Workload) bool {
			return w.Annotations["agentmoat.io/uses-iouring"] == "true"
		},
	})

	r.Register(Rule{
		ID:             "perf-events",
		Severity:       SeverityWarn,
		Description:    "Workload requests CAP_PERFMON or CAP_SYS_ADMIN typically used for perf_event_open; gVisor does not expose perf events.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/",
		Match: func(w scanner.Workload) bool {
			return anyContainerHasCapability(w.PodSpec, "PERFMON") ||
				anyContainerHasCapability(w.PodSpec, "SYS_ADMIN")
		},
	})

	r.Register(Rule{
		ID:             "kvm-nested",
		Severity:       SeverityError,
		Description:    "Workload uses /dev/kvm (nested virtualization); gVisor sandbox cannot pass through KVM, and on EKS nested virt is unavailable anyway.",
		RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/",
		Match: func(w scanner.Workload) bool {
			for _, v := range w.PodSpec.Volumes {
				if v.HostPath != nil && v.HostPath.Path == "/dev/kvm" {
					return true
				}
			}
			return false
		},
	})

	r.Register(Rule{
		ID:             "network-throughput",
		Severity:       SeverityInfo,
		Description:    "Workload appears network-throughput-bound (image hint: nginx, envoy, haproxy, traefik); expect ~20-40% throughput overhead under gVisor's sandbox network stack.",
		RemediationURL: "https://gvisor.dev/docs/architecture_guide/performance/",
		Match: func(w scanner.Workload) bool {
			return imageContainsAny(w.ImageRefs, "nginx", "envoy", "haproxy", "traefik")
		},
	})

	r.Register(Rule{
		ID:             "syscall-heavy",
		Severity:       SeverityInfo,
		Description:    "Workload appears syscall-heavy (image hint: redis, memcached); expect higher latency under Sentry; benchmark before migration.",
		RemediationURL: "https://gvisor.dev/docs/architecture_guide/performance/",
		Match: func(w scanner.Workload) bool {
			return imageContainsAny(w.ImageRefs, "redis", "memcached")
		},
	})
}

// allContainers returns init + main containers as a single flat slice. The
// classifier checks both because init containers can also request elevated
// capabilities or privileged mode and the same compatibility constraints
// apply.
func allContainers(spec corev1.PodSpec) []corev1.Container {
	out := make([]corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	out = append(out, spec.InitContainers...)
	out = append(out, spec.Containers...)
	return out
}

// anyContainerHasCapability reports whether any init/main container's
// SecurityContext.Capabilities.Add list includes the given capability
// name (case-sensitive: Kubernetes capability names are uppercase like
// "NET_RAW" or "SYS_ADMIN").
func anyContainerHasCapability(spec corev1.PodSpec, cap string) bool {
	for _, c := range allContainers(spec) {
		if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
			continue
		}
		for _, added := range c.SecurityContext.Capabilities.Add {
			if string(added) == cap {
				return true
			}
		}
	}
	return false
}

// imageContainsAny does a case-insensitive substring match of each image
// reference against the given needles. Used by the image-hint based rules
// (eBPF, network-throughput, syscall-heavy).
func imageContainsAny(images []string, needles ...string) bool {
	for _, img := range images {
		lower := strings.ToLower(img)
		for _, needle := range needles {
			if strings.Contains(lower, strings.ToLower(needle)) {
				return true
			}
		}
	}
	return false
}
