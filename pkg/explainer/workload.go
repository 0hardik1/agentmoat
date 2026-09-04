// Package explainer: deep, per-workload explanation.
//
// This file implements the evidence-extraction core that powers
// `agentmoat explain namespace <ns>` and `agentmoat explain workload
// <ns>/<name>`. Where the classifier answers the operator's first
// question ("is this workload compatible?"), the explainer answers the
// follow-ups: "which rule fired, what in the PodSpec triggered it, and
// what should I do about it?".
//
// Design choices for a K8s engineer reading this for the first time:
//
//   - The function is a pure transform from (Workload, Verdict, []Rule)
//     to schema.WorkloadExplanation. No I/O at runtime; the prose
//     markdown is read from the docs.FS embed, which is baked into the
//     binary.
//
//   - The per-rule extraction logic mirrors pkg/classifier/builtin_rules.go.
//     If a rule decides to fire because a container has CAP_NET_RAW, the
//     evidence we surface here points at the very same container. The two
//     packages share helpers (allContainers, capability lookup) in spirit
//     but the explainer keeps its own copies because we need slightly
//     different signatures (we want container *names* in the evidence,
//     not booleans).
//
//   - "Checked" entries (rules that did NOT fire) are populated for every
//     verdict, not just Compatible ones. This lets the operator see "we
//     evaluated 14 rules; here is the 1 that fired and the 13 that did
//     not" on a Review verdict, which matches the mental model of an
//     audit log.
//
//   - We never return an error in v1: a missing prose file is a soft
//     fallback (empty WhyMarkdown) so the caller can use the Reason's
//     short description instead. The error return is kept on the
//     signature for forward compatibility, in case a future revision
//     wants to make missing prose a hard failure.
package explainer

import (
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/docs"
	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
)

// ExplainWorkload returns a deep, structured explanation of a single
// workload's compatibility verdict. For each Reason in the verdict it
// walks the workload's PodSpec to pull the concrete evidence that
// triggered the rule, and loads operator-facing prose from the embedded
// docs/explanations/<rule-id>.md. For Compatible verdicts it also
// records every rule that was evaluated but did not fire, so the
// operator sees what was checked.
//
// allRules is the full list of rules in the active classifier registry,
// used to populate Checked entries for Compatible verdicts. The caller
// is responsible for sorting; ExplainWorkload preserves the input order.
func ExplainWorkload(
	w scanner.Workload,
	v classifier.Verdict,
	allRules []classifier.Rule,
) (schema.WorkloadExplanation, error) {
	// firedIDs is a small lookup table to skip the rules-that-did-fire
	// when we build the Checked list below. It's a map instead of a
	// linear search because the Checked loop runs len(allRules) times and
	// we want this to be cheap even on large registries.
	firedIDs := make(map[string]struct{}, len(v.Reasons))
	for _, r := range v.Reasons {
		firedIDs[r.RuleID] = struct{}{}
	}

	// Findings: one entry per rule that fired. v.Reasons is already
	// sorted by RuleID (see classifier.Classify) so we preserve that
	// order downstream.
	findings := make([]schema.RuleFinding, 0, len(v.Reasons))
	for _, reason := range v.Reasons {
		findings = append(findings, schema.RuleFinding{
			RuleID:         reason.RuleID,
			Severity:       reason.Severity,
			Title:          ruleTitle(reason.RuleID),
			WhyMarkdown:    LoadExplanationProse(reason.RuleID),
			Evidence:       extractEvidence(w, reason.RuleID),
			RemediationURL: reason.RemediationURL,
		})
	}

	// Checked: one entry per rule that did NOT fire, in the input order.
	// Populated for every verdict (not just Compatible) so even on a
	// Review verdict the operator can see "we checked 14 rules, 1 fired,
	// here are the 13 that didn't".
	checked := make([]schema.RuleCheck, 0, len(allRules))
	for _, r := range allRules {
		if _, fired := firedIDs[r.ID]; fired {
			continue
		}
		checked = append(checked, schema.RuleCheck{
			RuleID:  r.ID,
			Outcome: "did-not-fire",
		})
	}

	return schema.WorkloadExplanation{
		Kind:           w.Kind,
		Namespace:      w.Namespace,
		Name:           w.Name,
		Compatibility:  v.Compatibility,
		Recommendation: v.Recommendation,
		Overhead:       v.Overhead,
		Findings:       findings,
		Checked:        checked,
	}, nil
}

// LoadExplanationProse returns the embedded markdown prose for a rule
// ID. Returns the empty string (no error) when the rule has no prose
// file, so callers can fall back to the rule's Reason.Description.
//
// The lookup is exact: rule IDs are stable, lowercased, dash-separated
// strings (see pkg/classifier/builtin_rules.go), and the prose files
// follow the same convention (`docs/explanations/<ruleID>.md`).
func LoadExplanationProse(ruleID string) string {
	if ruleID == "" {
		return ""
	}
	data, err := docs.FS.ReadFile("explanations/" + ruleID + ".md")
	if err != nil {
		// Soft fallback: a missing prose file is not fatal. Callers can
		// substitute the Reason.Description string into the operator
		// output instead.
		return ""
	}
	return string(data)
}

// ruleTitle returns the short, human-readable header for a rule ID. The
// strings are hardcoded (rather than derived from rule.Description)
// because Description is a sentence ("Container requests CAP_NET_RAW;
// gVisor ...") while Title is meant to fit as a section heading
// ("Raw socket capability requested").
//
// An unknown rule ID falls back to the ID itself, which gives forward-
// compatibility: a future rule that ships before its title is added
// here will still render, just less prettily.
func ruleTitle(ruleID string) string {
	switch ruleID {
	case "host-network":
		return "Host network namespace shared"
	case "host-pid":
		return "Host PID namespace shared"
	case "host-ipc":
		return "Host IPC namespace shared"
	case "privileged":
		return "Privileged container"
	case "raw-socket":
		return "Raw socket capability requested"
	case "ebpf":
		return "eBPF workload detected"
	case "kvm-nested":
		return "Nested KVM virtualization"
	case "host-path-mount":
		return "Host path volume mounted"
	case "gpu-passthrough":
		return "GPU passthrough requested"
	case "fuse-mount":
		return "FUSE filesystem in use"
	case "io-uring":
		return "io_uring opt-in declared"
	case "perf-events":
		return "Performance event capability"
	case "network-throughput":
		return "Network-throughput-bound workload"
	case "syscall-heavy":
		return "Syscall-heavy workload"
	default:
		return ruleID
	}
}

// extractEvidence pulls the concrete PodSpec facts that triggered (or
// could have triggered) the named rule. The dispatcher mirrors the rule
// IDs registered in pkg/classifier/builtin_rules.go; new rules added
// there should add a corresponding case here.
//
// Unknown rule IDs return a zero Evidence value (forward-compatible:
// the finding still shows up with title + prose, just without
// structured evidence).
func extractEvidence(w scanner.Workload, ruleID string) schema.Evidence {
	switch ruleID {
	case "host-network":
		return schema.Evidence{HostNamespaces: []string{"hostNetwork"}}
	case "host-pid":
		return schema.Evidence{HostNamespaces: []string{"hostPID"}}
	case "host-ipc":
		return schema.Evidence{HostNamespaces: []string{"hostIPC"}}
	case "privileged":
		return evidenceForPrivileged(w)
	case "raw-socket":
		return evidenceForRawSocket(w)
	case "ebpf":
		return evidenceForEBPF(w)
	case "kvm-nested":
		return evidenceForKVMNested(w)
	case "host-path-mount":
		return evidenceForHostPathMount(w)
	case "gpu-passthrough":
		return evidenceForGPUPassthrough(w)
	case "fuse-mount":
		return evidenceForFUSEMount(w)
	case "io-uring":
		return evidenceForIOUring(w)
	case "perf-events":
		return evidenceForPerfEvents(w)
	case "network-throughput":
		return evidenceForImageHints(w, []string{"nginx", "envoy", "haproxy", "traefik"})
	case "syscall-heavy":
		return evidenceForImageHints(w, []string{"redis", "memcached"})
	default:
		return schema.Evidence{}
	}
}

// evidenceForPrivileged collects the names of every container running
// in privileged mode. Mirrors matchPrivileged in builtin_rules.go.
func evidenceForPrivileged(w scanner.Workload) schema.Evidence {
	var names []string
	for _, c := range allContainersOf(w.PodSpec) {
		if c.SecurityContext != nil &&
			c.SecurityContext.Privileged != nil &&
			*c.SecurityContext.Privileged {
			names = append(names, c.Name)
		}
	}
	return schema.Evidence{PrivilegedContainers: names}
}

// evidenceForRawSocket lists every container with CAP_NET_RAW and, if
// present, the operator-declared annotation. Either path is enough to
// fire the rule; mirroring matchRawSocket exactly.
func evidenceForRawSocket(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	for _, c := range allContainersOf(w.PodSpec) {
		if containerHasCapability(c, "NET_RAW") {
			ev.Capabilities = append(ev.Capabilities, schema.CapabilityHit{
				Container:  c.Name,
				Capability: "CAP_NET_RAW",
			})
		}
	}
	// Operator-declared override: surface the annotation so the operator
	// can see why the rule fired even when no container has the cap set.
	if val, ok := w.Annotations["agentmoat.io/needs-raw-socket"]; ok && val == "true" {
		ev.Annotations = append(ev.Annotations, schema.AnnotationHit{
			Key:   "agentmoat.io/needs-raw-socket",
			Value: val,
		})
	}
	return ev
}

// evidenceForEBPF lists matching image hints AND CAP_BPF holders. The
// classifier fires the rule if either path matches; we report both
// paths in the evidence so the operator sees the full picture.
func evidenceForEBPF(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	hints := []string{"cilium", "tetragon", "falco"}
	for _, c := range w.PodSpec.Containers {
		if pat := imageMatchHint(c.Image, hints); pat != "" {
			ev.ImageMatches = append(ev.ImageMatches, schema.ImageMatch{
				Container:   c.Name,
				Image:       c.Image,
				HintPattern: pat,
			})
		}
	}
	for _, c := range w.PodSpec.InitContainers {
		if pat := imageMatchHint(c.Image, hints); pat != "" {
			ev.ImageMatches = append(ev.ImageMatches, schema.ImageMatch{
				Container:   c.Name,
				Image:       c.Image,
				HintPattern: pat,
			})
		}
	}
	for _, c := range allContainersOf(w.PodSpec) {
		if containerHasCapability(c, "BPF") {
			ev.Capabilities = append(ev.Capabilities, schema.CapabilityHit{
				Container:  c.Name,
				Capability: "CAP_BPF",
			})
		}
	}
	return ev
}

// evidenceForKVMNested surfaces the /dev/kvm hostPath volume(s) and the
// containers that mount each. Mirrors matchKVMNested in builtin_rules.go.
func evidenceForKVMNested(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	for _, v := range w.PodSpec.Volumes {
		if v.HostPath != nil && v.HostPath.Path == "/dev/kvm" {
			ev.HostPaths = append(ev.HostPaths, schema.HostPathHit{
				Volume:     v.Name,
				Path:       v.HostPath.Path,
				Containers: containersUsingVolume(w.PodSpec, v.Name),
			})
		}
	}
	return ev
}

// evidenceForHostPathMount surfaces every hostPath volume on the pod
// spec along with the containers that mount it. Distinct from
// evidenceForKVMNested in that we don't filter by path: any hostPath
// fires the rule.
func evidenceForHostPathMount(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	for _, v := range w.PodSpec.Volumes {
		if v.HostPath != nil {
			ev.HostPaths = append(ev.HostPaths, schema.HostPathHit{
				Volume:     v.Name,
				Path:       v.HostPath.Path,
				Containers: containersUsingVolume(w.PodSpec, v.Name),
			})
		}
	}
	return ev
}

// evidenceForGPUPassthrough lists every nvidia.com/* resource each
// container requests, with the actual resource name (nvidia.com/gpu,
// nvidia.com/gpu.shared, nvidia.com/mig-1g.5gb, ...) and the quantity.
// Limits come first because that is the canonical place for extended
// resources; a name present in both lists is reported once, from Limits.
// Mirrors matchGPUPassthrough in builtin_rules.go.
func evidenceForGPUPassthrough(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	for _, c := range allContainersOf(w.PodSpec) {
		seen := map[string]bool{}
		for _, list := range []corev1.ResourceList{c.Resources.Limits, c.Resources.Requests} {
			for _, name := range nvidiaResourceNames(list) {
				if seen[name] {
					continue
				}
				seen[name] = true
				qty := list[corev1.ResourceName(name)]
				ev.GPURequests = append(ev.GPURequests, schema.GPURequest{
					Container: c.Name,
					Resource:  name,
					Quantity:  qty.String(),
				})
			}
		}
	}
	return ev
}

// evidenceForFUSEMount surfaces CSI volumes with a fuse-named driver
// AND containers that set the AGENTMOAT_USES_FUSE=true env var
// (the operator-declared escape hatch). Mirrors matchFUSEMount.
func evidenceForFUSEMount(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	for _, v := range w.PodSpec.Volumes {
		if v.CSI != nil && strings.Contains(strings.ToLower(v.CSI.Driver), "fuse") {
			ev.CSIDrivers = append(ev.CSIDrivers, schema.CSIDriverHit{
				Volume: v.Name,
				Driver: v.CSI.Driver,
			})
		}
	}
	for _, c := range allContainersOf(w.PodSpec) {
		for _, env := range c.Env {
			if env.Name == "AGENTMOAT_USES_FUSE" && env.Value == "true" {
				ev.EnvVars = append(ev.EnvVars, schema.EnvVarHit{
					Container: c.Name,
					Name:      env.Name,
					Value:     env.Value,
				})
			}
		}
	}
	return ev
}

// evidenceForIOUring surfaces the io_uring opt-in annotation. The rule
// only fires when the annotation is exactly "true", so by definition
// that's the value we report.
func evidenceForIOUring(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	if val, ok := w.Annotations["agentmoat.io/uses-iouring"]; ok && val == "true" {
		ev.Annotations = append(ev.Annotations, schema.AnnotationHit{
			Key:   "agentmoat.io/uses-iouring",
			Value: val,
		})
	}
	return ev
}

// evidenceForPerfEvents lists every container holding CAP_PERFMON or
// CAP_SYS_ADMIN. Both caps fire the rule; we report whichever (or
// both) the container actually requested. PERFMON is reported first
// when present because it is the more specific signal.
func evidenceForPerfEvents(w scanner.Workload) schema.Evidence {
	ev := schema.Evidence{}
	for _, c := range allContainersOf(w.PodSpec) {
		if containerHasCapability(c, "PERFMON") {
			ev.Capabilities = append(ev.Capabilities, schema.CapabilityHit{
				Container:  c.Name,
				Capability: "CAP_PERFMON",
			})
		}
		if containerHasCapability(c, "SYS_ADMIN") {
			ev.Capabilities = append(ev.Capabilities, schema.CapabilityHit{
				Container:  c.Name,
				Capability: "CAP_SYS_ADMIN",
			})
		}
	}
	return ev
}

// evidenceForImageHints lists every container whose image matches one
// of the given hint patterns. Used by both the network-throughput rule
// (nginx, envoy, haproxy, traefik) and the syscall-heavy rule (redis,
// memcached). The HintPattern recorded is the first pattern that
// matched, mirroring imageMatchHint's first-match semantics.
func evidenceForImageHints(w scanner.Workload, hints []string) schema.Evidence {
	ev := schema.Evidence{}
	for _, c := range w.PodSpec.Containers {
		if pat := imageMatchHint(c.Image, hints); pat != "" {
			ev.ImageMatches = append(ev.ImageMatches, schema.ImageMatch{
				Container:   c.Name,
				Image:       c.Image,
				HintPattern: pat,
			})
		}
	}
	for _, c := range w.PodSpec.InitContainers {
		if pat := imageMatchHint(c.Image, hints); pat != "" {
			ev.ImageMatches = append(ev.ImageMatches, schema.ImageMatch{
				Container:   c.Name,
				Image:       c.Image,
				HintPattern: pat,
			})
		}
	}
	return ev
}

// allContainersOf returns init + main containers as a flat slice. Same
// shape as classifier's internal helper; duplicated here so the
// explainer doesn't reach across package boundaries for a one-liner.
// (Init containers count: the same rules apply to them, and an operator
// reading the evidence wants to know which init container triggered
// the rule.)
func allContainersOf(spec corev1.PodSpec) []corev1.Container {
	out := make([]corev1.Container, 0, len(spec.InitContainers)+len(spec.Containers))
	out = append(out, spec.InitContainers...)
	out = append(out, spec.Containers...)
	return out
}

// containerHasCapability reports whether the container's
// SecurityContext.Capabilities.Add list includes the given cap name.
// The match is case-insensitive on the cap name itself, because some
// fixtures spell it "net_raw" while K8s canonicalises to "NET_RAW".
// Note the cap name passed in must NOT include the "CAP_" prefix:
// Kubernetes stores capability names without it ("NET_RAW", not
// "CAP_NET_RAW"), matching the existing classifier helper.
func containerHasCapability(c corev1.Container, capName string) bool {
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return false
	}
	want := strings.ToUpper(capName)
	for _, added := range c.SecurityContext.Capabilities.Add {
		if strings.ToUpper(string(added)) == want {
			return true
		}
	}
	return false
}

// imageMatchHint returns the first hint that appears (case-insensitively)
// as a substring of the image reference, or "" when no hint matches.
// First-match semantics keep the reported pattern stable when multiple
// hints could apply.
func imageMatchHint(image string, hints []string) string {
	lower := strings.ToLower(image)
	for _, h := range hints {
		if strings.Contains(lower, strings.ToLower(h)) {
			return h
		}
	}
	return ""
}

// containersUsingVolume walks all containers (init + main) and returns
// the names of those whose VolumeMounts reference the given volume
// name. Order: init containers first, then main, matching the
// allContainersOf ordering used elsewhere in this file.
func containersUsingVolume(spec corev1.PodSpec, volumeName string) []string {
	var names []string
	for _, c := range allContainersOf(spec) {
		for _, m := range c.VolumeMounts {
			if m.Name == volumeName {
				names = append(names, c.Name)
				break
			}
		}
	}
	return names
}

// nvidiaResourceNames returns the nvidia.com/* resource names in the
// list, sorted, so evidence order does not depend on map iteration.
func nvidiaResourceNames(list corev1.ResourceList) []string {
	var names []string
	for name := range list {
		if strings.HasPrefix(string(name), schema.NvidiaResourcePrefix) {
			names = append(names, string(name))
		}
	}
	sort.Strings(names)
	return names
}
