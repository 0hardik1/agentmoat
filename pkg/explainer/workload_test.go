// Tests for the deep, per-workload explainer.
//
// Strategy:
//
//   - One table-driven test (TestExplainWorkload_PerRuleEvidence) covers
//     every built-in rule with a minimal fixture that should trigger
//     exactly that rule. We synthesize a Verdict carrying one Reason so
//     ExplainWorkload's per-rule extraction is what's under test, not
//     classifier.Classify (which has its own tests).
//
//   - Separate top-level tests cover the "Compatible verdict with all
//     rules in Checked" case, the LoadExplanationProse happy path, and
//     the LoadExplanationProse missing-file fallback. Keeping these out
//     of the main table makes the failure messages crisper.
//
// Fixtures borrow the workloadOpts convention from
// pkg/classifier/classifier_test.go, but we keep the helper local
// because the classifier package's helper is unexported.
package explainer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// makeReason builds a synthetic Reason with predictable fields. The
// description and remediation URL strings are not load-bearing for the
// extractor (it only switches on RuleID), but we set them so callers can
// also verify they pass through to RuleFinding.
func makeReason(id string, sev classifier.Severity) classifier.Reason {
	return classifier.Reason{
		RuleID:         id,
		Severity:       sev,
		Description:    "synthetic description for " + id,
		RemediationURL: "https://example.test/" + id,
	}
}

// makeVerdict wraps a single Reason in a Verdict shaped like what
// classifier.Classify would emit (correct Compatibility based on the
// reason's severity). Used by every per-rule case in the table.
func makeVerdict(reason classifier.Reason) classifier.Verdict {
	var compat classifier.Compatibility
	switch reason.Severity {
	case classifier.SeverityError:
		compat = classifier.Incompatible
	case classifier.SeverityWarn:
		compat = classifier.Review
	default:
		compat = classifier.Compatible
	}
	return classifier.Verdict{
		Compatibility:  compat,
		Reasons:        []classifier.Reason{reason},
		Recommendation: "synthetic recommendation",
		Overhead:       "",
	}
}

// withVolumeMount returns a container with the given name and one
// VolumeMount referencing the given volume name. Used by host-path and
// kvm fixtures so we exercise the containersUsingVolume helper too.
func withVolumeMount(name, vol string) corev1.Container {
	return corev1.Container{
		Name: name,
		VolumeMounts: []corev1.VolumeMount{
			{Name: vol, MountPath: "/mnt/" + vol},
		},
	}
}

// TestExplainWorkload_PerRuleEvidence is the per-rule table. Each row
// builds a minimal PodSpec that the named rule would fire on, synthesizes
// a Verdict with one Reason for that rule, calls ExplainWorkload, and
// then runs a row-specific assertion via the `check` closure on the
// resulting WorkloadExplanation.
//
//nolint:gocyclo // table-driven test; complexity comes from many fixture rows, not branching logic
func TestExplainWorkload_PerRuleEvidence(t *testing.T) {
	t.Parallel()

	allRules := buildAllRules()

	cases := []struct {
		name   string
		w      scanner.Workload
		reason classifier.Reason
		check  func(t *testing.T, got schema.WorkloadExplanation)
	}{
		{
			name: "host-network",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{HostNetwork: true},
			},
			reason: makeReason("host-network", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []string{"hostNetwork"}
				if !reflect.DeepEqual(got.Findings[0].Evidence.HostNamespaces, want) {
					t.Errorf("HostNamespaces: got %v want %v",
						got.Findings[0].Evidence.HostNamespaces, want)
				}
			},
		},
		{
			name: "host-pid",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{HostPID: true},
			},
			reason: makeReason("host-pid", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []string{"hostPID"}
				if !reflect.DeepEqual(got.Findings[0].Evidence.HostNamespaces, want) {
					t.Errorf("HostNamespaces: got %v want %v",
						got.Findings[0].Evidence.HostNamespaces, want)
				}
			},
		},
		{
			name: "host-ipc",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{HostIPC: true},
			},
			reason: makeReason("host-ipc", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []string{"hostIPC"}
				if !reflect.DeepEqual(got.Findings[0].Evidence.HostNamespaces, want) {
					t.Errorf("HostNamespaces: got %v want %v",
						got.Findings[0].Evidence.HostNamespaces, want)
				}
			},
		},
		{
			name: "privileged",
			w: func() scanner.Workload {
				priv := true
				return scanner.Workload{
					Kind: "Pod", Namespace: "ns", Name: "n",
					PodSpec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "main",
								SecurityContext: &corev1.SecurityContext{
									Privileged: &priv,
								},
							},
						},
					},
				}
			}(),
			reason: makeReason("privileged", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []string{"main"}
				if !reflect.DeepEqual(got.Findings[0].Evidence.PrivilegedContainers, want) {
					t.Errorf("PrivilegedContainers: got %v want %v",
						got.Findings[0].Evidence.PrivilegedContainers, want)
				}
			},
		},
		{
			name: "raw-socket",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "sniffer",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"NET_RAW"},
								},
							},
						},
					},
				},
			},
			reason: makeReason("raw-socket", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.CapabilityHit{
					{Container: "sniffer", Capability: "CAP_NET_RAW"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.Capabilities, want) {
					t.Errorf("Capabilities: got %+v want %+v",
						got.Findings[0].Evidence.Capabilities, want)
				}
			},
		},
		{
			name: "raw-socket via annotation",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				Annotations: map[string]string{
					"agentmoat.io/needs-raw-socket": "true",
				},
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
			reason: makeReason("raw-socket", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.AnnotationHit{
					{Key: "agentmoat.io/needs-raw-socket", Value: "true"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.Annotations, want) {
					t.Errorf("Annotations: got %+v want %+v",
						got.Findings[0].Evidence.Annotations, want)
				}
			},
		},
		{
			name: "ebpf image hint",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "agent", Image: "quay.io/cilium/cilium:v1.14"},
					},
				},
				ImageRefs: []string{"quay.io/cilium/cilium:v1.14"},
			},
			reason: makeReason("ebpf", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				ims := got.Findings[0].Evidence.ImageMatches
				if len(ims) != 1 {
					t.Fatalf("ImageMatches len: got %d want 1: %+v", len(ims), ims)
				}
				if ims[0].Container != "agent" ||
					ims[0].HintPattern != "cilium" {
					t.Errorf("ImageMatches[0]: got %+v want container=agent hint=cilium", ims[0])
				}
			},
		},
		{
			name: "ebpf CAP_BPF",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "loader",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"BPF"},
								},
							},
						},
					},
				},
			},
			reason: makeReason("ebpf", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.CapabilityHit{
					{Container: "loader", Capability: "CAP_BPF"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.Capabilities, want) {
					t.Errorf("Capabilities: got %+v want %+v",
						got.Findings[0].Evidence.Capabilities, want)
				}
			},
		},
		{
			name: "kvm-nested",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "kvm",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm"},
							},
						},
					},
					Containers: []corev1.Container{withVolumeMount("vm", "kvm")},
				},
			},
			reason: makeReason("kvm-nested", classifier.SeverityError),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.HostPathHit{
					{Volume: "kvm", Path: "/dev/kvm", Containers: []string{"vm"}},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.HostPaths, want) {
					t.Errorf("HostPaths: got %+v want %+v",
						got.Findings[0].Evidence.HostPaths, want)
				}
			},
		},
		{
			name: "host-path-mount",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "varlog",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"},
							},
						},
					},
					Containers: []corev1.Container{withVolumeMount("collector", "varlog")},
				},
			},
			reason: makeReason("host-path-mount", classifier.SeverityWarn),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.HostPathHit{
					{Volume: "varlog", Path: "/var/log", Containers: []string{"collector"}},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.HostPaths, want) {
					t.Errorf("HostPaths: got %+v want %+v",
						got.Findings[0].Evidence.HostPaths, want)
				}
			},
		},
		{
			name: "gpu-passthrough",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "trainer",
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									"nvidia.com/gpu": resource.MustParse("2"),
								},
							},
						},
					},
				},
			},
			reason: makeReason("gpu-passthrough", classifier.SeverityWarn),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.GPURequest{
					{Container: "trainer", Resource: "nvidia.com/gpu", Quantity: "2"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.GPURequests, want) {
					t.Errorf("GPURequests: got %+v want %+v",
						got.Findings[0].Evidence.GPURequests, want)
				}
			},
		},
		{
			name: "fuse-mount CSI",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Volumes: []corev1.Volume{
						{
							Name: "s3",
							VolumeSource: corev1.VolumeSource{
								CSI: &corev1.CSIVolumeSource{
									Driver: "s3.csi.fuse.example.com",
								},
							},
						},
					},
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
			reason: makeReason("fuse-mount", classifier.SeverityWarn),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.CSIDriverHit{
					{Volume: "s3", Driver: "s3.csi.fuse.example.com"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.CSIDrivers, want) {
					t.Errorf("CSIDrivers: got %+v want %+v",
						got.Findings[0].Evidence.CSIDrivers, want)
				}
			},
		},
		{
			name: "fuse-mount env-var",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "main",
							Env: []corev1.EnvVar{
								{Name: "AGENTMOAT_USES_FUSE", Value: "true"},
							},
						},
					},
				},
			},
			reason: makeReason("fuse-mount", classifier.SeverityWarn),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.EnvVarHit{
					{Container: "main", Name: "AGENTMOAT_USES_FUSE", Value: "true"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.EnvVars, want) {
					t.Errorf("EnvVars: got %+v want %+v",
						got.Findings[0].Evidence.EnvVars, want)
				}
			},
		},
		{
			name: "io-uring",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				Annotations: map[string]string{
					"agentmoat.io/uses-iouring": "true",
				},
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
			reason: makeReason("io-uring", classifier.SeverityWarn),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.AnnotationHit{
					{Key: "agentmoat.io/uses-iouring", Value: "true"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.Annotations, want) {
					t.Errorf("Annotations: got %+v want %+v",
						got.Findings[0].Evidence.Annotations, want)
				}
			},
		},
		{
			name: "perf-events PERFMON",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "profiler",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"PERFMON"},
								},
							},
						},
					},
				},
			},
			reason: makeReason("perf-events", classifier.SeverityWarn),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.CapabilityHit{
					{Container: "profiler", Capability: "CAP_PERFMON"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.Capabilities, want) {
					t.Errorf("Capabilities: got %+v want %+v",
						got.Findings[0].Evidence.Capabilities, want)
				}
			},
		},
		{
			name: "perf-events SYS_ADMIN",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name: "admin",
							SecurityContext: &corev1.SecurityContext{
								Capabilities: &corev1.Capabilities{
									Add: []corev1.Capability{"SYS_ADMIN"},
								},
							},
						},
					},
				},
			},
			reason: makeReason("perf-events", classifier.SeverityWarn),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				want := []schema.CapabilityHit{
					{Container: "admin", Capability: "CAP_SYS_ADMIN"},
				}
				if !reflect.DeepEqual(got.Findings[0].Evidence.Capabilities, want) {
					t.Errorf("Capabilities: got %+v want %+v",
						got.Findings[0].Evidence.Capabilities, want)
				}
			},
		},
		{
			name: "network-throughput",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "lb", Image: "envoyproxy/envoy:v1.28"},
					},
				},
				ImageRefs: []string{"envoyproxy/envoy:v1.28"},
			},
			reason: makeReason("network-throughput", classifier.SeverityInfo),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				ims := got.Findings[0].Evidence.ImageMatches
				if len(ims) != 1 {
					t.Fatalf("ImageMatches len: got %d want 1: %+v", len(ims), ims)
				}
				if ims[0].HintPattern != "envoy" {
					t.Errorf("ImageMatches[0].HintPattern: got %q want %q",
						ims[0].HintPattern, "envoy")
				}
			},
		},
		{
			name: "syscall-heavy",
			w: scanner.Workload{
				Kind: "Pod", Namespace: "ns", Name: "n",
				PodSpec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "cache", Image: "redis:7-alpine"},
					},
				},
				ImageRefs: []string{"redis:7-alpine"},
			},
			reason: makeReason("syscall-heavy", classifier.SeverityInfo),
			check: func(t *testing.T, got schema.WorkloadExplanation) {
				ims := got.Findings[0].Evidence.ImageMatches
				if len(ims) != 1 {
					t.Fatalf("ImageMatches len: got %d want 1: %+v", len(ims), ims)
				}
				if ims[0].HintPattern != "redis" {
					t.Errorf("ImageMatches[0].HintPattern: got %q want %q",
						ims[0].HintPattern, "redis")
				}
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			v := makeVerdict(c.reason)
			got, err := ExplainWorkload(c.w, v, allRules)
			if err != nil {
				t.Fatalf("ExplainWorkload: unexpected error %v", err)
			}
			if got.Compatibility != v.Compatibility {
				t.Errorf("Compatibility: got %q want %q",
					got.Compatibility, v.Compatibility)
			}
			if len(got.Findings) != 1 {
				t.Fatalf("Findings: got %d want 1", len(got.Findings))
			}
			f := got.Findings[0]
			if f.RuleID != c.reason.RuleID {
				t.Errorf("Findings[0].RuleID: got %q want %q",
					f.RuleID, c.reason.RuleID)
			}
			if f.Severity != c.reason.Severity {
				t.Errorf("Findings[0].Severity: got %q want %q",
					f.Severity, c.reason.Severity)
			}
			if f.RemediationURL != c.reason.RemediationURL {
				t.Errorf("Findings[0].RemediationURL: got %q want %q",
					f.RemediationURL, c.reason.RemediationURL)
			}
			// Title is hardcoded per ruleID; for the rules in this table
			// every ruleID is known, so the fallback ("ruleID") path is
			// never taken. We only spot-check that Title is non-empty
			// here, and the title-matrix is exercised by
			// TestRuleTitle_KnownAndUnknown below.
			if f.Title == "" {
				t.Errorf("Findings[0].Title is empty for %q", c.reason.RuleID)
			}
			c.check(t, got)
		})
	}
}

// TestExplainWorkload_CompatibleEmptyFindings asserts that a Compatible
// verdict (no Reasons) produces an empty Findings slice and a Checked
// entry per rule in allRules. This is the path the table doesn't cover.
func TestExplainWorkload_CompatibleEmptyFindings(t *testing.T) {
	t.Parallel()

	allRules := buildAllRules()
	w := scanner.Workload{
		Kind: "Deployment", Namespace: "demo", Name: "hello",
		PodSpec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "alpine:3.19"},
			},
		},
	}
	v := classifier.Verdict{
		Compatibility:  classifier.Compatible,
		Reasons:        nil,
		Recommendation: "Safe to migrate.",
	}

	got, err := ExplainWorkload(w, v, allRules)
	if err != nil {
		t.Fatalf("ExplainWorkload: unexpected error %v", err)
	}
	if got.Compatibility != classifier.Compatible {
		t.Errorf("Compatibility: got %q want %q",
			got.Compatibility, classifier.Compatible)
	}
	if len(got.Findings) != 0 {
		t.Errorf("Findings: got %d entries, want 0: %+v",
			len(got.Findings), got.Findings)
	}
	if len(got.Checked) != len(allRules) {
		t.Errorf("Checked: got %d entries, want %d",
			len(got.Checked), len(allRules))
	}
	for i, ch := range got.Checked {
		if ch.Outcome != "did-not-fire" {
			t.Errorf("Checked[%d].Outcome: got %q want %q",
				i, ch.Outcome, "did-not-fire")
		}
		if ch.RuleID != allRules[i].ID {
			t.Errorf("Checked[%d].RuleID: got %q want %q (order should match allRules)",
				i, ch.RuleID, allRules[i].ID)
		}
	}
}

// TestExplainWorkload_ReviewIncludesChecked asserts that even a Review
// verdict (one rule fired) gets Checked entries for the rules that did
// not fire. This is the "audit log" property: the operator should see
// what was inspected, not just what tripped.
func TestExplainWorkload_ReviewIncludesChecked(t *testing.T) {
	t.Parallel()

	allRules := buildAllRules()
	w := scanner.Workload{
		Kind: "Pod", Namespace: "ns", Name: "n",
		PodSpec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "varlog",
					VolumeSource: corev1.VolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"},
					},
				},
			},
			Containers: []corev1.Container{withVolumeMount("c", "varlog")},
		},
	}
	v := makeVerdict(makeReason("host-path-mount", classifier.SeverityWarn))

	got, err := ExplainWorkload(w, v, allRules)
	if err != nil {
		t.Fatalf("ExplainWorkload: unexpected error %v", err)
	}
	if len(got.Findings) != 1 {
		t.Fatalf("Findings: got %d want 1", len(got.Findings))
	}
	// Checked must contain every rule except host-path-mount.
	if got, want := len(got.Checked), len(allRules)-1; got != want {
		t.Errorf("Checked: got %d entries, want %d", got, want)
	}
	for _, ch := range got.Checked {
		if ch.RuleID == "host-path-mount" {
			t.Errorf("Checked: includes %q, which fired; should be in Findings instead",
				ch.RuleID)
		}
	}
}

// TestLoadExplanationProse_Known verifies the embed wiring: a known rule
// ID produces a non-empty markdown string containing the expected
// "## Why incompatible" heading that the prose files share.
func TestLoadExplanationProse_Known(t *testing.T) {
	t.Parallel()
	prose := LoadExplanationProse("host-network")
	if prose == "" {
		t.Fatal("LoadExplanationProse(\"host-network\"): got empty string, want non-empty")
	}
	if !strings.Contains(prose, "## Why incompatible") {
		t.Errorf("LoadExplanationProse(\"host-network\"): missing expected heading; first 100 chars: %q",
			truncate(prose, 100))
	}
}

// TestLoadExplanationProse_Missing verifies the soft-fallback path: a
// rule ID with no .md file in the embed returns "" and not an error.
func TestLoadExplanationProse_Missing(t *testing.T) {
	t.Parallel()
	got := LoadExplanationProse("nonexistent-rule")
	if got != "" {
		t.Errorf("LoadExplanationProse(\"nonexistent-rule\"): got %q, want \"\"", got)
	}
}

// TestRuleTitle_KnownAndUnknown spot-checks the title mapping. Known IDs
// must produce the documented strings; unknown IDs fall back to the
// raw ID (forward-compatibility for rules added before titles).
func TestRuleTitle_KnownAndUnknown(t *testing.T) {
	t.Parallel()
	if got := ruleTitle("host-network"); got != "Host network namespace shared" {
		t.Errorf("ruleTitle(host-network): got %q", got)
	}
	if got := ruleTitle("host-path-mount"); got != "Host path volume mounted" {
		t.Errorf("ruleTitle(host-path-mount): got %q", got)
	}
	if got := ruleTitle("future-rule-id"); got != "future-rule-id" {
		t.Errorf("ruleTitle(future-rule-id): got %q, want fallback to ID", got)
	}
}

// buildAllRules registers the built-in rule set and returns the sorted
// rule slice the same way the orchestrator would. Used by tests that
// need a realistic allRules input.
func buildAllRules() []classifier.Rule {
	reg := classifier.NewRegistry()
	classifier.RegisterBuiltins(reg)
	return reg.Rules()
}

// truncate is a small helper for error messages so we don't dump 2KB of
// markdown into the test output when an assertion fails.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
