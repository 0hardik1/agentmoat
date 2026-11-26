package preflight

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func seccompFacts(ws ...schema.SeccompWorkload) *schema.ClusterFacts {
	return &schema.ClusterFacts{Seccomp: &schema.SeccompFacts{Workloads: ws}}
}

func ran(name, version string, profile string, loaded, ignored int, blocked ...schema.BlockedSyscall) schema.SeccompWorkload {
	return schema.SeccompWorkload{
		Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: name}, Status: schema.SeccompTrialRan,
		Profiles:     []schema.ContainerSeccompProfile{{Container: "app", Type: profile}},
		RunscVersion: version, Node: "gv-a", FiltersLoaded: loaded, FiltersIgnored: ignored, Blocked: blocked,
	}
}

func ids(fs []schema.PreflightFinding) string {
	var out []string
	for _, f := range fs {
		out = append(out, string(f.Severity)+":"+f.ID)
	}
	return strings.Join(out, ",")
}

func TestSeccompFindings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		ws   []schema.SeccompWorkload
		want string
	}{
		{"nil facts", nil, ""},
		{"clean", []schema.SeccompWorkload{ran("web", "release-20261123.1", schema.SeccompProfileRuntimeDefault, 2, 0)}, "info:seccomp-probe-clean"},
		{"blocked", []schema.SeccompWorkload{ran("web", "release-20261123.1", schema.SeccompProfileRuntimeDefault, 2, 0,
			schema.BlockedSyscall{Name: "unshare", Number: 272, Action: "denied", Count: 3})}, "warn:seccomp-blocked-syscalls"},
		{"filter ignored", []schema.SeccompWorkload{ran("web", "release-20261123.1", schema.SeccompProfileRuntimeDefault, 0, 2)}, "warn:seccomp-filter-not-loaded"},
		{"old runsc", []schema.SeccompWorkload{ran("web", "release-20261116.0", schema.SeccompProfileRuntimeDefault, 2, 0)},
			"warn:seccomp-runsc-before-default,info:seccomp-probe-clean"},
		{"unconfined", []schema.SeccompWorkload{ran("web", "release-20261123.1", schema.SeccompProfileUnconfined, 0, 0)},
			"info:seccomp-probe-clean,info:seccomp-unconfined"},
		{"failed and skipped", []schema.SeccompWorkload{
			{Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "a"}, Status: schema.SeccompTrialFailed, Reason: "x"},
			{Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "b"}, Status: schema.SeccompTrialSkipped, Reason: "y"},
		}, "warn:seccomp-probe-failed,warn:seccomp-probe-skipped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := &schema.ClusterFacts{}
			if tc.ws != nil {
				facts = seccompFacts(tc.ws...)
			}
			got := seccompFindings(facts)
			SortFindings(got)
			if ids(got) != tc.want {
				t.Fatalf("findings = %s, want %s", ids(got), tc.want)
			}
		})
	}
}

func TestRunscBefore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		a, b         string
		before, okay bool
	}{
		{"release-20261123.0", "20261123.1", true, true},
		{"release-20261123.1", "20261123.1", false, true},
		{"release-20261201.0", "20261123.1", false, true},
		{"0.0.0-dev", "20261123.1", false, false},
	} {
		before, ok := runscBefore(tc.a, tc.b)
		if before != tc.before || ok != tc.okay {
			t.Errorf("runscBefore(%q, %q) = %v, %v", tc.a, tc.b, before, ok)
		}
	}
}
