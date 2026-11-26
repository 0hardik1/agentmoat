// Package preflight: findings from the seccomp probe.
//
// `agentmoat probe seccomp` (pkg/seccompprobe) runs a trial copy of each
// migrated workload in a gVisor sandbox with its own seccomp profile and
// stores what it saw in ClusterFacts.Seccomp. This file turns those facts
// into findings. Like the rest of Evaluate it is pure, so a saved probe
// report fed to `scan --facts` or `plan --facts` gives the same findings
// (and, for the warn ones, the same plan warnings) without a cluster.
//
// Why this matters now: from gVisor release 20261123.1, runsc loads the
// container's OCI seccomp filter inside the sandbox by default
// (--oci-seccomp defaults to true). A workload that ran fine under an older
// runsc with a RuntimeDefault profile was not filtered at all; after the
// upgrade the same pod can get EPERM from syscalls the profile refuses.
package preflight

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// Seccomp probe finding IDs. Stable; documented in docs/preflight.md.
const (
	// FindingSeccompBlockedSyscalls (warn): during its trial, the
	// workload's own seccomp profile refused at least one syscall inside
	// the sandbox. One finding per workload.
	FindingSeccompBlockedSyscalls = "seccomp-blocked-syscalls"

	// FindingSeccompFilterNotLoaded (warn): the workload asks for a
	// RuntimeDefault or Localhost profile, the trial ran, and the Sentry
	// installed no filter, so the profile did not apply.
	FindingSeccompFilterNotLoaded = "seccomp-filter-not-loaded"

	// FindingSeccompRunscBeforeDefault (warn): the trial sandbox ran a
	// runsc older than the release that enforces OCI seccomp by default,
	// so the trial is not a run on that release.
	FindingSeccompRunscBeforeDefault = "seccomp-runsc-before-default"

	// FindingSeccompUnconfined (info): a probed workload has a container
	// whose profile is Unconfined, so no filter applies to it.
	FindingSeccompUnconfined = "seccomp-unconfined"

	// FindingSeccompProbeClean (info): every trial that ran hit no filter.
	FindingSeccompProbeClean = "seccomp-probe-clean"

	// FindingSeccompProbeDryRun (info): no pod was created.
	FindingSeccompProbeDryRun = "seccomp-probe-dry-run"

	// FindingSeccompProbeFailed (warn): a trial or its log read did not
	// complete. One finding per workload.
	FindingSeccompProbeFailed = "seccomp-probe-failed"

	// FindingSeccompProbeSkipped (warn): the probe did not run a workload
	// (or any), and says why.
	FindingSeccompProbeSkipped = "seccomp-probe-skipped"
)

// seccompFindings derives the findings for facts.Seccomp. Nil facts yield
// nothing: the probe has not run.
func seccompFindings(f *schema.ClusterFacts) []schema.PreflightFinding {
	if f.Seccomp == nil {
		return nil
	}
	var out []schema.PreflightFinding
	ran, clean := 0, 0
	var unconfined []string
	oldRunsc := map[string][]string{}
	for i := range f.Seccomp.Workloads {
		w := &f.Seccomp.Workloads[i]
		ref := refString(w.Target)
		if names := unconfinedNames(w.Profiles); len(names) > 0 {
			unconfined = append(unconfined, fmt.Sprintf("%s (%s)", ref, strings.Join(names, ", ")))
		}
		switch w.Status {
		case schema.SeccompTrialFailed:
			out = append(out, schema.PreflightFinding{
				ID: FindingSeccompProbeFailed, Severity: schema.SeverityWarn,
				Message:     fmt.Sprintf("seccomp trial of %s did not complete: %s", ref, w.Reason),
				Remediation: "fix the cause above and re-run 'agentmoat probe seccomp'; the trial pod was deleted",
			})
			continue
		case schema.SeccompTrialSkipped:
			out = append(out, schema.PreflightFinding{
				ID: FindingSeccompProbeSkipped, Severity: schema.SeverityWarn,
				Message:     fmt.Sprintf("seccomp trial of %s not run: %s", ref, w.Reason),
				Remediation: "fix the cause above and re-run 'agentmoat probe seccomp'",
			})
			continue
		case schema.SeccompTrialRan:
		default:
			continue
		}
		ran++
		if before, ok := runscBefore(w.RunscVersion, schema.OCISeccompDefaultRelease); ok && before {
			oldRunsc[w.RunscVersion] = append(oldRunsc[w.RunscVersion], ref)
		}
		if len(w.Blocked) > 0 {
			out = append(out, blockedFinding(w, ref))
			continue
		}
		if filtered := filteredContainers(w.Profiles); len(filtered) > 0 && w.FiltersLoaded == 0 {
			why := "the Sentry installed no seccomp filter"
			if w.FiltersIgnored > 0 {
				why = "runsc ignored the profile because oci-seccomp was off"
			}
			out = append(out, schema.PreflightFinding{
				ID: FindingSeccompFilterNotLoaded, Severity: schema.SeverityWarn,
				Message: fmt.Sprintf("%s asks for a seccomp profile (%s) but %s during its trial on node %s, so the trial says nothing about what the profile blocks",
					ref, strings.Join(filtered, ", "), why, w.Node),
				Remediation: fmt.Sprintf("check that the audit RuntimeClass's handler on node %s runs runsc with oci-seccomp = \"true\" (kind/runsc-seccomp-audit.toml, packer/files/runsc-seccomp-audit.toml)", w.Node),
			})
			continue
		}
		clean++
	}
	out = append(out, runscFindings(oldRunsc)...)
	if len(unconfined) > 0 {
		out = append(out, schema.PreflightFinding{
			ID: FindingSeccompUnconfined, Severity: schema.SeverityInfo,
			Message: fmt.Sprintf("%d probed workload(s) have a container with seccompProfile Unconfined, which no seccomp filter covers inside the sandbox: %s",
				len(unconfined), strings.Join(unconfined, "; ")),
			Remediation: "see docs/explanations/seccomp-unconfined.md; switch to RuntimeDefault or a Localhost profile unless Unconfined is a deliberate choice",
		})
	}
	if ran > 0 && clean == ran {
		out = append(out, schema.PreflightFinding{
			ID: FindingSeccompProbeClean, Severity: schema.SeverityInfo,
			Message: fmt.Sprintf("%d seccomp trial(s) ran and no seccomp filter refused a syscall; the result covers only what each workload did during its trial", ran),
		})
	}
	return out
}

func blockedFinding(w *schema.SeccompWorkload, ref string) schema.PreflightFinding {
	parts := make([]string, 0, len(w.Blocked))
	for _, b := range w.Blocked {
		parts = append(parts, fmt.Sprintf("%s (%s, %dx)", b.Name, b.Action, b.Count))
	}
	return schema.PreflightFinding{
		ID: FindingSeccompBlockedSyscalls, Severity: schema.SeverityWarn,
		Message: fmt.Sprintf("%s: its seccomp profile (%s) refused %s inside the gVisor sandbox (runsc %s, node %s)",
			ref, describeProfiles(w.Profiles), strings.Join(parts, ", "), orUnknown(w.RunscVersion), w.Node),
		Remediation: "check whether the workload needs these calls; if it does, allow them in a Localhost profile, or keep the workload on a runsc older than " +
			schema.OCISeccompDefaultRelease + " only as a stopgap; see docs/preflight.md#seccomp-probe",
	}
}

func runscFindings(old map[string][]string) []schema.PreflightFinding {
	versions := make([]string, 0, len(old))
	for v := range old {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	out := make([]schema.PreflightFinding, 0, len(versions))
	for _, v := range versions {
		out = append(out, schema.PreflightFinding{
			ID: FindingSeccompRunscBeforeDefault, Severity: schema.SeverityWarn,
			Message: fmt.Sprintf("the trial sandbox of %s ran runsc %s, older than %s; the audit handler forces oci-seccomp on, so the filter applied, but the trial was not a run on %s",
				strings.Join(old[v], ", "), v, schema.OCISeccompDefaultRelease, schema.OCISeccompDefaultRelease),
			Remediation: "upgrade runsc on the gVisor nodes (docs/gvisor-version.md) and re-run the probe",
		})
	}
	return out
}

// filteredContainers names the containers whose profile asks for a filter.
func filteredContainers(ps []schema.ContainerSeccompProfile) []string {
	var out []string
	for _, p := range ps {
		if p.Type == schema.SeccompProfileRuntimeDefault || p.Type == schema.SeccompProfileLocalhost {
			out = append(out, p.Container+": "+p.Type)
		}
	}
	return out
}

func unconfinedNames(ps []schema.ContainerSeccompProfile) []string {
	var out []string
	for _, p := range ps {
		if p.Type == schema.SeccompProfileUnconfined {
			out = append(out, p.Container)
		}
	}
	return out
}

// describeProfiles renders "app: RuntimeDefault, sidecar: Unset".
func describeProfiles(ps []schema.ContainerSeccompProfile) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		s := p.Container + ": " + p.Type
		if p.LocalhostProfile != "" {
			s += " " + p.LocalhostProfile
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

func refString(r schema.WorkloadRef) string {
	return fmt.Sprintf("%s %s/%s", r.Kind, r.Namespace, r.Name)
}

func orUnknown(s string) string {
	if s == "" {
		return "version unknown"
	}
	return s
}

var releaseTagRe = regexp.MustCompile(`^(?:release-)?(\d{8})\.(\d+)$`)

// runscBefore reports whether release a is older than release b. ok is
// false when either does not parse.
func runscBefore(a, b string) (before, ok bool) {
	ad, an, okA := parseRelease(a)
	bd, bn, okB := parseRelease(b)
	if !okA || !okB {
		return false, false
	}
	if ad != bd {
		return ad < bd, true
	}
	return an < bn, true
}

func parseRelease(s string) (date, point int, ok bool) {
	m := releaseTagRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, false
	}
	d, err1 := strconv.Atoi(m[1])
	p, err2 := strconv.Atoi(m[2])
	return d, p, err1 == nil && err2 == nil
}
