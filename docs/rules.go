// Per-rule documentation: a title and the explanation prose for each
// classifier rule ID.
//
// These live next to the embedded files, in this package, because two
// packages need them and neither should import the other: pkg/explainer
// (the `explain namespace` and `explain workload` output) and pkg/output
// (the rule metadata of the SARIF output). This package has no imports
// outside the standard library, so both can depend on it.

package docs

import "sort"

// ruleTitles maps each built-in rule ID to a short heading. The strings are
// written by hand (not derived from the rule's description) because a
// description is a sentence ("Container requests CAP_NET_RAW; gVisor ...")
// and a title must fit in a heading ("Raw socket capability requested").
//
// A table, not a switch: one line for each rule, and a new rule does not
// add to a function's cyclomatic complexity.
var ruleTitles = map[string]string{
	"host-network":       "Host network namespace shared",
	"host-pid":           "Host PID namespace shared",
	"host-ipc":           "Host IPC namespace shared",
	"privileged":         "Privileged container",
	"raw-socket":         "Raw socket capability requested",
	"ebpf":               "eBPF workload detected",
	"kvm-nested":         "Nested KVM virtualization",
	"host-path-mount":    "Host path volume mounted",
	"gpu-passthrough":    "GPU passthrough requested",
	"fuse-mount":         "FUSE filesystem in use",
	"io-uring":           "io_uring opt-in declared",
	"perf-events":        "Performance event capability",
	"network-throughput": "Network-throughput-bound workload",
	"seccomp-unconfined": "Seccomp profile set to Unconfined",
	"syscall-heavy":      "Syscall-heavy workload",
	"systemd-init":       "systemd runs as PID 1",
}

// RuleTitle returns the short heading for a rule ID.
//
// An unknown rule ID falls back to the ID. That keeps a rule that ships
// before its title is added here readable.
func RuleTitle(ruleID string) string {
	if t, ok := ruleTitles[ruleID]; ok {
		return t
	}
	return ruleID
}

// RuleExplanation returns the embedded Markdown prose for a rule
// (explanations/<ruleID>.md): why gVisor cannot run the workload, and what
// to do instead. It returns "" when no file exists for the ID, so a caller
// can fall back to the description of the finding.
//
// The lookup is exact. Rule IDs are stable, lowercase, dash-separated
// strings (see pkg/classifier/builtin_rules.go), and the files follow the
// same convention.
func RuleExplanation(ruleID string) string {
	if ruleID == "" {
		return ""
	}
	data, err := FS.ReadFile("explanations/" + ruleID + ".md")
	if err != nil {
		return ""
	}
	return string(data)
}

// RuleIDs returns the IDs of the rules that have a title here, sorted.
func RuleIDs() []string {
	ids := make([]string, 0, len(ruleTitles))
	for id := range ruleTitles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
