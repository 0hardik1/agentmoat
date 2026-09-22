// Package classifier: the systemd-init rule (match and refinement).
//
// Some images run systemd as PID 1: RHEL-style "init" images, test images
// for systemd units, VM-like containers that start several services. Until
// September 2026 gVisor could not run systemd at all. Release 20260831.0
// added what systemd needs (in-sandbox cgroups v2, the new mount API,
// pidfds, openat2) behind runsc flags that are off by default:
//
//	--in-sandbox-cgroup=v2   systemd needs a cgroup v2 tree (required)
//	--allow-suid             setuid binaries (su, sudo) elevate IDs
//	                         (only for images that use them)
//
// See https://gvisor.dev/blog/2026/09/17/systemd-in-gvisor/. Tested on
// kind with runsc release-20260914.0 (docs/explanations/systemd-init.md):
// a ubi9/ubi-init pod reaches "running" with the cgroup flag plus
// CAP_SYS_ADMIN (systemd remounts the read-only cgroup tree), and without
// the flag gets only to "degraded".
//
// Detection is static, from the pod spec only. agentmoat does not read image
// config, so an image whose own ENTRYPOINT/CMD starts systemd is caught only
// through the image-name hints below or the operator annotation.
//
// The refinement turns the installed runsc release (facts.Runsc, filled by
// `agentmoat probe nvproxy`) into a verdict:
//
//	no cluster facts                          -> unchanged
//	facts, but runsc version unknown          -> unchanged, note: run the probe
//	runsc version not in release-yyyymmdd.N   -> unchanged, note
//	runsc older than 20260831.0               -> error
//	runsc 20260831.0 or newer                 -> unchanged, note: check the flags
//
// Even on a new enough runsc the rule stays at its configured severity:
// the flags live in the containerd runtime config (runsc.toml) or in pod
// annotations, and neither is visible through the Kubernetes API. An
// operator who has confirmed the flags lowers the rule to info with a
// --rules override; the rows that return the base severity keep it.
package classifier

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
)

// SystemdAnnotation is the operator escape hatch: set it to "true" on a
// workload whose image starts systemd by its own ENTRYPOINT/CMD, which the
// pod spec does not show. Mirrors agentmoat.io/needs-raw-socket.
const SystemdAnnotation = "agentmoat.io/runs-systemd"

// SystemdMinRunsc is the first gVisor release that can run systemd:
// it added --in-sandbox-cgroup (replacing the experimental
// --mount-cgroup-v2) and the openat2(2) syscall.
const SystemdMinRunsc = "20260831.0"

// systemdExecutables are the paths that start systemd as PID 1.
// /sbin/init and /usr/sbin/init are symlinks to systemd on every
// systemd-based distribution. Matching is exact on purpose: "tini",
// "dumb-init", and "/usr/bin/catatonit" are small init shims that reap
// zombies, not systemd, and they run fine under gVisor.
var systemdExecutables = map[string]bool{
	"/sbin/init":               true,
	"/usr/sbin/init":           true,
	"/lib/systemd/systemd":     true,
	"/usr/lib/systemd/systemd": true,
}

// systemdImageHints are image-name substrings of images that start systemd
// by default. They are the Red Hat UBI "init" images
// (registry.access.redhat.com/ubi9/ubi-init, .../ubi9-init, ...), whose
// default command is /sbin/init. The hints are specific on purpose: a bare
// "-init" would also match istio-init and vault-init, which are ordinary
// init containers.
var systemdImageHints = []string{"ubi-init", "ubi8-init", "ubi9-init", "ubi10-init"}

// SystemdMatch says why a container was taken to run systemd as PID 1.
type SystemdMatch struct {
	// Field is "command", "args", or "image".
	Field string
	// Value is the executable path (command/args) or the image hint (image).
	Value string
}

// ContainerRunsSystemd reports whether c starts systemd as PID 1, and why.
// The explainer calls it too, so the evidence it shows is exactly what made
// the rule fire.
//
// The order follows how the container runtime picks the process:
//
//  1. A non-empty command replaces the image ENTRYPOINT: check command[0].
//  2. With no command, args replace the image CMD. For an image with no
//     ENTRYPOINT (the usual case for systemd images) args[0] is the
//     program: check args[0].
//  3. With neither, the image's own ENTRYPOINT/CMD runs, which the pod spec
//     does not show: fall back to the image-name hints.
func ContainerRunsSystemd(c corev1.Container) (SystemdMatch, bool) {
	switch {
	case len(c.Command) > 0:
		if systemdExecutables[c.Command[0]] {
			return SystemdMatch{Field: "command", Value: c.Command[0]}, true
		}
	case len(c.Args) > 0:
		if systemdExecutables[c.Args[0]] {
			return SystemdMatch{Field: "args", Value: c.Args[0]}, true
		}
	default:
		for _, hint := range systemdImageHints {
			if imageContainsAny([]string{c.Image}, hint) {
				return SystemdMatch{Field: "image", Value: hint}, true
			}
		}
	}
	return SystemdMatch{}, false
}

// matchSystemdInit fires on the operator annotation or on any main
// container that runs systemd. Init containers are skipped: they must exit
// before the pod starts, and systemd as PID 1 never exits by design.
func matchSystemdInit(w scanner.Workload) bool {
	if w.Annotations[SystemdAnnotation] == "true" {
		return true
	}
	for _, c := range w.PodSpec.Containers {
		if _, ok := ContainerRunsSystemd(c); ok {
			return true
		}
	}
	return false
}

// refineSystemdInit is the Rule.Refine hook for systemd-init. It applies
// the decision table in the file header.
func refineSystemdInit(_ scanner.Workload, facts *schema.ClusterFacts, base Severity) (Refinement, bool) {
	if facts == nil {
		return Refinement{}, false
	}
	version := runscVersion(facts)
	if version == "" {
		return Refinement{Severity: base, Note: fmt.Sprintf(
			"Cluster facts: the installed runsc release is unknown; run 'agentmoat probe nvproxy --dry-run=false' and pass its report with --facts to check it against release-%s.",
			SystemdMinRunsc)}, true
	}
	older, ok := runscOlderThan(version, SystemdMinRunsc)
	switch {
	case !ok:
		return Refinement{Severity: base, Note: fmt.Sprintf(
			"Cluster facts: runsc reports version %q, which is not a release-yyyymmdd.N tag, so it cannot be checked against release-%s.",
			version, SystemdMinRunsc)}, true
	case older:
		return Refinement{Severity: SeverityError, Note: fmt.Sprintf(
			"Cluster facts: runsc %s predates systemd support (needs release-%s or newer).",
			version, SystemdMinRunsc)}, true
	default:
		return Refinement{Severity: base, Note: fmt.Sprintf(
			"Cluster facts: runsc %s supports systemd; confirm the sandbox runs with in-sandbox-cgroup=v2 (runsc.toml, or the pod annotation dev.gvisor.flag.in-sandbox-cgroup), which agentmoat cannot see.",
			version)}, true
	}
}

// runscVersion returns the probed runsc release string, or "". Reports
// saved by a probe from before facts.Runsc existed carry the same value
// under the GPU facts only, so fall back to it.
func runscVersion(facts *schema.ClusterFacts) string {
	if facts.Runsc != nil && facts.Runsc.Version != "" {
		return facts.Runsc.Version
	}
	if facts.GPU != nil && facts.GPU.Nvproxy != nil {
		return facts.GPU.Nvproxy.RunscVersion
	}
	return ""
}

// runscTagRe matches a gVisor release as `runsc --version` prints it
// ("release-20260914.0") or as the download URLs spell it ("20260914.0").
var runscTagRe = regexp.MustCompile(`^(?:release-)?(\d{8})\.(\d+)$`)

// runscOlderThan reports whether release a is older than release b,
// comparing the date and then the point number. ok is false when either
// does not parse (a development build prints no release tag).
func runscOlderThan(a, b string) (older, ok bool) {
	ad, an, okA := parseRunscTag(a)
	bd, bn, okB := parseRunscTag(b)
	if !okA || !okB {
		return false, false
	}
	if ad != bd {
		return ad < bd, true
	}
	return an < bn, true
}

func parseRunscTag(s string) (date, point int, ok bool) {
	m := runscTagRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	date, errD := strconv.Atoi(m[1])
	point, errP := strconv.Atoi(m[2])
	return date, point, errD == nil && errP == nil
}
