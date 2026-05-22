// Tests for the classifier package. Table-driven for the verdict
// resolution; a separate test exercises Registry.LoadYAML (severity
// override).
//
// We use only the standard testing package: no testify, no gomock. The
// test helper newWorkload builds a minimal scanner.Workload to keep each
// case row short and readable.
package classifier

import (
	"reflect"
	"testing"

	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// newWorkload constructs a Workload from a few options. Callers pass in
// only the fields they need; the rest are zero-valued.
type workloadOpts struct {
	hostNetwork  bool
	hostPID      bool
	hostIPC      bool
	privileged   bool
	addCaps      []corev1.Capability
	volumes      []corev1.Volume
	gpuLimit     bool
	images       []string
	annotations  map[string]string
}

func newWorkload(o workloadOpts) scanner.Workload {
	container := corev1.Container{
		Name:  "main",
		Image: "test/image:latest",
	}
	if o.privileged {
		priv := true
		container.SecurityContext = &corev1.SecurityContext{
			Privileged: &priv,
		}
	}
	if len(o.addCaps) > 0 {
		if container.SecurityContext == nil {
			container.SecurityContext = &corev1.SecurityContext{}
		}
		container.SecurityContext.Capabilities = &corev1.Capabilities{
			Add: o.addCaps,
		}
	}
	if o.gpuLimit {
		container.Resources = corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu": resource.MustParse("1"),
			},
		}
	}

	images := o.images
	if len(images) == 0 {
		images = []string{"test/image:latest"}
	}
	// If caller supplied images, override the container image so the
	// PodSpec and ImageRefs are consistent (useful when a rule checks
	// only ImageRefs anyway, but keeps the fixture honest).
	container.Image = images[0]

	return scanner.Workload{
		Kind:      "Pod",
		Namespace: "default",
		Name:      "test",
		PodSpec: corev1.PodSpec{
			HostNetwork: o.hostNetwork,
			HostPID:     o.hostPID,
			HostIPC:     o.hostIPC,
			Volumes:     o.volumes,
			Containers:  []corev1.Container{container},
		},
		Annotations: o.annotations,
		ImageRefs:   images,
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name        string
		w           scanner.Workload
		wantCompat  Compatibility
		wantRuleIDs []string // sorted by RuleID, matches Verdict.Reasons order
	}{
		{
			name:        "empty pod spec is compatible",
			w:           newWorkload(workloadOpts{}),
			wantCompat:  Compatible,
			wantRuleIDs: []string{},
		},
		{
			name:        "host network is incompatible",
			w:           newWorkload(workloadOpts{hostNetwork: true}),
			wantCompat:  Incompatible,
			wantRuleIDs: []string{"host-network"},
		},
		{
			name:        "host pid is incompatible",
			w:           newWorkload(workloadOpts{hostPID: true}),
			wantCompat:  Incompatible,
			wantRuleIDs: []string{"host-pid"},
		},
		{
			name:        "privileged container is incompatible",
			w:           newWorkload(workloadOpts{privileged: true}),
			wantCompat:  Incompatible,
			wantRuleIDs: []string{"privileged"},
		},
		{
			name:        "NET_RAW capability is incompatible",
			w:           newWorkload(workloadOpts{addCaps: []corev1.Capability{"NET_RAW"}}),
			wantCompat:  Incompatible,
			wantRuleIDs: []string{"raw-socket"},
		},
		{
			name:        "nvidia gpu resource is review",
			w:           newWorkload(workloadOpts{gpuLimit: true}),
			wantCompat:  Review,
			wantRuleIDs: []string{"gpu-passthrough"},
		},
		{
			name: "host path volume is review",
			w: newWorkload(workloadOpts{
				volumes: []corev1.Volume{
					{
						Name: "varlog",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"},
						},
					},
				},
			}),
			wantCompat:  Review,
			wantRuleIDs: []string{"host-path-mount"},
		},
		{
			name: "host path /dev/kvm is incompatible (both kvm-nested and host-path-mount fire)",
			w: newWorkload(workloadOpts{
				volumes: []corev1.Volume{
					{
						Name: "kvm",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm"},
						},
					},
				},
			}),
			wantCompat:  Incompatible,
			wantRuleIDs: []string{"host-path-mount", "kvm-nested"},
		},
		{
			name:        "nginx image is compatible with info-only network-throughput",
			w:           newWorkload(workloadOpts{images: []string{"nginx:alpine"}}),
			wantCompat:  Compatible,
			wantRuleIDs: []string{"network-throughput"},
		},
		{
			name:        "cilium image is incompatible (ebpf)",
			w:           newWorkload(workloadOpts{images: []string{"cilium/cilium:v1.14.4"}}),
			wantCompat:  Incompatible,
			wantRuleIDs: []string{"ebpf"},
		},
		{
			name: "host network + GPU is incompatible (both reasons sorted)",
			w: newWorkload(workloadOpts{
				hostNetwork: true,
				gpuLimit:    true,
			}),
			wantCompat:  Incompatible,
			wantRuleIDs: []string{"gpu-passthrough", "host-network"},
		},
		{
			name: "io-uring annotation is review",
			w: newWorkload(workloadOpts{
				annotations: map[string]string{"agentmoat.io/uses-iouring": "true"},
			}),
			wantCompat:  Review,
			wantRuleIDs: []string{"io-uring"},
		},
	}

	registry := NewRegistry()
	RegisterBuiltins(registry)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := Classify(c.w, registry)
			if v.Compatibility != c.wantCompat {
				t.Fatalf("Compatibility: got %q want %q", v.Compatibility, c.wantCompat)
			}
			gotIDs := []string{}
			for _, r := range v.Reasons {
				gotIDs = append(gotIDs, r.RuleID)
			}
			if !reflect.DeepEqual(gotIDs, c.wantRuleIDs) {
				t.Fatalf("Reasons: got %v want %v", gotIDs, c.wantRuleIDs)
			}
			// Sanity check: Recommendation must be non-empty for every
			// verdict, since that field is rendered into operator-facing
			// output and an empty string would look like a bug.
			if v.Recommendation == "" {
				t.Errorf("Recommendation is empty for case %q", c.name)
			}
		})
	}
}

// TestRegistry_Override verifies that an operator-supplied YAML override
// dialing host-path-mount down to info turns a workload-with-hostPath
// verdict from Review into Compatible.
func TestRegistry_Override(t *testing.T) {
	registry := NewRegistry()
	RegisterBuiltins(registry)

	// Workload mounts /var/log via hostPath: under defaults this is a
	// warn-class finding (Review). With the override below it becomes
	// info-class and Compatibility falls back to Compatible.
	w := newWorkload(workloadOpts{
		volumes: []corev1.Volume{
			{
				Name: "varlog",
				VolumeSource: corev1.VolumeSource{
					HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"},
				},
			},
		},
	})

	// Baseline: Review.
	if got := Classify(w, registry).Compatibility; got != Review {
		t.Fatalf("baseline Compatibility: got %q want %q", got, Review)
	}

	// Apply override.
	yamlBlob := []byte(`overrides:
  - id: host-path-mount
    severity: info
`)
	warnings, err := registry.LoadYAML(yamlBlob)
	if err != nil {
		t.Fatalf("LoadYAML: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}

	v := Classify(w, registry)
	if v.Compatibility != Compatible {
		t.Fatalf("after override Compatibility: got %q want %q", v.Compatibility, Compatible)
	}
	// The reason itself should still be in the Reasons list, just at
	// info severity (operators want a paper trail, not silent removal).
	if len(v.Reasons) != 1 || v.Reasons[0].RuleID != "host-path-mount" {
		t.Fatalf("after override Reasons: got %+v want [host-path-mount]", v.Reasons)
	}
	if v.Reasons[0].Severity != SeverityInfo {
		t.Fatalf("after override severity: got %q want %q", v.Reasons[0].Severity, SeverityInfo)
	}
}

// TestRegistry_LoadYAML_UnknownID verifies that LoadYAML reports unknown
// rule IDs as warnings and does not return an error.
func TestRegistry_LoadYAML_UnknownID(t *testing.T) {
	registry := NewRegistry()
	RegisterBuiltins(registry)

	yamlBlob := []byte(`overrides:
  - id: nonexistent-rule
    severity: error
`)
	warnings, err := registry.LoadYAML(yamlBlob)
	if err != nil {
		t.Fatalf("LoadYAML: unexpected error %v", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
}
