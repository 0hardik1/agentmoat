// Tests for the systemd-init rule (systemd_init.go): which containers the
// match takes to run systemd as PID 1, and how the installed runsc release
// in the cluster facts moves the verdict. The refinement cases go through
// the public ClassifyWithFacts entry point, like gpu_refine_test.go, so
// they also pin the plumbing (severity replaced, note appended).
package classifier

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
)

func TestContainerRunsSystemd(t *testing.T) {
	cases := []struct {
		name      string
		c         corev1.Container
		wantOK    bool
		wantField string
		wantValue string
	}{
		{name: "command /sbin/init", c: corev1.Container{Command: []string{"/sbin/init"}},
			wantOK: true, wantField: "command", wantValue: "/sbin/init"},
		{name: "command /usr/sbin/init", c: corev1.Container{Command: []string{"/usr/sbin/init"}},
			wantOK: true, wantField: "command", wantValue: "/usr/sbin/init"},
		{name: "command systemd with flags", c: corev1.Container{Command: []string{"/usr/lib/systemd/systemd", "--system"}},
			wantOK: true, wantField: "command", wantValue: "/usr/lib/systemd/systemd"},
		{name: "command /lib/systemd/systemd", c: corev1.Container{Command: []string{"/lib/systemd/systemd"}},
			wantOK: true, wantField: "command", wantValue: "/lib/systemd/systemd"},
		{name: "args[0] when command is empty", c: corev1.Container{Args: []string{"/sbin/init"}},
			wantOK: true, wantField: "args", wantValue: "/sbin/init"},
		{name: "UBI init image, no override", c: corev1.Container{Image: "registry.access.redhat.com/ubi9/ubi-init:latest"},
			wantOK: true, wantField: "image", wantValue: "ubi-init"},
		{name: "older UBI init image name", c: corev1.Container{Image: "registry.access.redhat.com/ubi8-init"},
			wantOK: true, wantField: "image", wantValue: "ubi8-init"},
		// A command replaces the image's own ENTRYPOINT/CMD, so the image
		// hint no longer says anything about PID 1.
		{name: "UBI init image with a shell command", c: corev1.Container{Image: "ubi9/ubi-init", Command: []string{"/bin/bash"}}},
		{name: "UBI init image with args", c: corev1.Container{Image: "ubi9/ubi-init", Args: []string{"/usr/bin/sleep", "infinity"}}},
		{name: "tini", c: corev1.Container{Command: []string{"/sbin/tini", "--", "/app"}}},
		{name: "dumb-init", c: corev1.Container{Command: []string{"/usr/bin/dumb-init", "--", "/app"}}},
		{name: "catatonit", c: corev1.Container{Command: []string{"/usr/bin/catatonit", "--", "/app"}}},
		{name: "bare init, not a path", c: corev1.Container{Command: []string{"init"}}},
		{name: "systemd in a later argument", c: corev1.Container{Command: []string{"/bin/sh", "-c", "/sbin/init"}}},
		{name: "istio-init image", c: corev1.Container{Image: "docker.io/istio/proxyv2:1.24.0-istio-init"}},
		{name: "UBI minimal image", c: corev1.Container{Image: "registry.access.redhat.com/ubi9/ubi-minimal"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := ContainerRunsSystemd(tc.c)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (match %+v)", ok, tc.wantOK, m)
			}
			if ok && (m.Field != tc.wantField || m.Value != tc.wantValue) {
				t.Fatalf("match = %+v, want {%s %s}", m, tc.wantField, tc.wantValue)
			}
		})
	}
}

// TestMatchSystemdInit_SkipsInitContainers pins that only main containers
// count: an init container must exit, and systemd as PID 1 never does.
func TestMatchSystemdInit_SkipsInitContainers(t *testing.T) {
	w := scanner.Workload{PodSpec: corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "setup", Command: []string{"/sbin/init"}}},
		Containers:     []corev1.Container{{Name: "app", Image: "test/app"}},
	}}
	if matchSystemdInit(w) {
		t.Fatal("init container running /sbin/init must not fire systemd-init")
	}
	w.PodSpec.Containers = append(w.PodSpec.Containers, corev1.Container{Name: "os", Command: []string{"/sbin/init"}})
	if !matchSystemdInit(w) {
		t.Fatal("second main container running /sbin/init must fire systemd-init")
	}
}

func runscFacts(version string) *schema.ClusterFacts {
	return &schema.ClusterFacts{Runsc: &schema.RunscFacts{Version: version, Node: "gv-1"}}
}

func systemdReason(t *testing.T, v Verdict) Reason {
	t.Helper()
	for _, r := range v.Reasons {
		if r.RuleID == "systemd-init" {
			return r
		}
	}
	t.Fatalf("systemd-init did not fire: %+v", v.Reasons)
	return Reason{}
}

func TestClassifyWithFacts_SystemdInit(t *testing.T) {
	cases := []struct {
		name       string
		facts      *schema.ClusterFacts
		wantCompat Compatibility
		wantSev    Severity
		wantNote   string // "" means the description must be the bare rule text
	}{
		{name: "no facts: unchanged", facts: nil, wantCompat: Review, wantSev: SeverityWarn},
		{name: "facts without a probe: unchanged with advice",
			facts: &schema.ClusterFacts{}, wantCompat: Review, wantSev: SeverityWarn,
			wantNote: "the installed runsc release is unknown; run 'agentmoat probe nvproxy --dry-run=false'"},
		{name: "runsc before systemd support: error",
			facts: runscFacts("release-20260817.0"), wantCompat: Incompatible, wantSev: SeverityError,
			wantNote: "runsc release-20260817.0 predates systemd support (needs release-20260831.0 or newer)."},
		{name: "same day, earlier point release: error",
			facts: runscFacts("release-20260830.9"), wantCompat: Incompatible, wantSev: SeverityError,
			wantNote: "predates systemd support"},
		{name: "first release with systemd support: unchanged with flag advice",
			facts: runscFacts("release-20260831.0"), wantCompat: Review, wantSev: SeverityWarn,
			wantNote: "runsc release-20260831.0 supports systemd; confirm the sandbox runs with in-sandbox-cgroup=v2"},
		{name: "newer release: unchanged with flag advice",
			facts: runscFacts("release-20260914.0"), wantCompat: Review, wantSev: SeverityWarn,
			wantNote: "runsc release-20260914.0 supports systemd"},
		{name: "tag without the release- prefix parses too",
			facts: runscFacts("20260817.0"), wantCompat: Incompatible, wantSev: SeverityError,
			wantNote: "predates systemd support"},
		{name: "development build: unchanged with note",
			facts: runscFacts("VERSION_MISSING"), wantCompat: Review, wantSev: SeverityWarn,
			wantNote: `runsc reports version "VERSION_MISSING", which is not a release-yyyymmdd.N tag`},
		{name: "older probe report: falls back to the nvproxy facts",
			facts:      &schema.ClusterFacts{GPU: &schema.GPUFacts{Nvproxy: &schema.NvproxyFacts{RunscVersion: "release-20260817.0"}}},
			wantCompat: Incompatible, wantSev: SeverityError, wantNote: "runsc release-20260817.0 predates systemd support"},
	}
	reg := NewRegistry()
	RegisterBuiltins(reg)
	w := newWorkload(workloadOpts{command: []string{"/sbin/init"}})
	bareDesc := systemdReason(t, Classify(w, reg)).Description

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ClassifyWithFacts(w, reg, tc.facts)
			if v.Compatibility != tc.wantCompat {
				t.Fatalf("compat = %s, want %s (reasons %+v)", v.Compatibility, tc.wantCompat, v.Reasons)
			}
			r := systemdReason(t, v)
			if r.Severity != tc.wantSev {
				t.Fatalf("severity = %s, want %s", r.Severity, tc.wantSev)
			}
			if tc.wantNote == "" {
				if r.Description != bareDesc {
					t.Fatalf("description changed without facts: %q", r.Description)
				}
				return
			}
			if !strings.HasPrefix(r.Description, bareDesc+" Cluster facts: ") || !strings.Contains(r.Description, tc.wantNote) {
				t.Fatalf("description = %q\nwant prefix %q and %q", r.Description, bareDesc+" Cluster facts: ", tc.wantNote)
			}
		})
	}
}

// TestClassifyWithFacts_SystemdInitOverride pins the operator contract: an
// operator who has confirmed the runsc flags lowers systemd-init to info
// with --rules, and that holds on a new enough runsc. A runsc that is too
// old still raises the rule to error, whatever the override says.
func TestClassifyWithFacts_SystemdInitOverride(t *testing.T) {
	reg := NewRegistry()
	RegisterBuiltins(reg)
	if err := reg.Override("systemd-init", SeverityInfo); err != nil {
		t.Fatal(err)
	}
	w := newWorkload(workloadOpts{command: []string{"/sbin/init"}})
	if v := ClassifyWithFacts(w, reg, runscFacts("release-20260914.0")); v.Compatibility != Compatible {
		t.Fatalf("new runsc with info override = %s, want compatible", v.Compatibility)
	}
	if v := ClassifyWithFacts(w, reg, runscFacts("release-20260817.0")); v.Compatibility != Incompatible {
		t.Fatalf("old runsc with info override = %s, want incompatible", v.Compatibility)
	}
}

func TestRunscOlderThan(t *testing.T) {
	cases := []struct {
		a, b          string
		older, wantOK bool
	}{
		{a: "release-20260817.0", b: "20260831.0", older: true, wantOK: true},
		{a: "release-20260831.0", b: "20260831.0", older: false, wantOK: true},
		{a: "release-20260831.1", b: "20260831.0", older: false, wantOK: true},
		{a: "release-20260830.12", b: "20260831.0", older: true, wantOK: true},
		{a: "20261201.0", b: "20260831.0", older: false, wantOK: true},
		{a: "release-2026083.0", b: "20260831.0"},
		{a: "", b: "20260831.0"},
		{a: "0.0.0", b: "20260831.0"},
		{a: "release-20260914.0-rc1", b: "20260831.0"},
	}
	for _, tc := range cases {
		older, ok := runscOlderThan(tc.a, tc.b)
		if older != tc.older || ok != tc.wantOK {
			t.Errorf("runscOlderThan(%q, %q) = (%v, %v), want (%v, %v)", tc.a, tc.b, older, ok, tc.older, tc.wantOK)
		}
	}
}
