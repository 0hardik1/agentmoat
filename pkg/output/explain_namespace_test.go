// Tests for the deep namespace-explanation renderer
// (explain_namespace.go). Like the rest of this package we assert at
// the "shape" level: substring checks on title, summary keywords,
// evidence lines, prose headings, etc. Golden files are deliberately
// avoided because lipgloss padding and glamour width-aware wrapping
// make byte-exact snapshots noisy.
//
// All tests use renderToString (defined in table_test.go), which runs
// Render with NoColor=true. That guarantees plain text on every CI and
// dev machine regardless of the host terminal's color profile.

package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// makeNamespaceDoc builds an ExplainDocument wrapping the given
// NamespaceExplanation so we can drive renderExplainDocumentTable
// through the public Render entry point and exercise the branch in
// explain.go at the same time.
func makeNamespaceDoc(ns *schema.NamespaceExplanation) *schema.ExplainDocument {
	d := schema.NewExplainDocument()
	d.Spec.Namespace = ns
	return d
}

// TestRenderNamespaceExplanation_Incompatible asserts that an
// incompatible workload renders every load-bearing element: badge,
// every fired rule, the concrete evidence strings, the prose's first
// heading, and the remediation link.
func TestRenderNamespaceExplanation_Incompatible(t *testing.T) {
	t.Parallel()
	ns := &schema.NamespaceExplanation{
		Name: "kube-system",
		Summary: schema.Summary{
			Total:        1,
			Incompatible: 1,
		},
		Workloads: []schema.WorkloadExplanation{
			{
				Kind:           "DaemonSet",
				Namespace:      "kube-system",
				Name:           "node-exporter",
				Compatibility:  schema.CompatibilityIncompatible,
				Recommendation: "do not migrate",
				Findings: []schema.RuleFinding{
					{
						RuleID:      "host-network",
						Severity:    schema.SeverityError,
						Title:       "Pod uses host network namespace",
						WhyMarkdown: "## Why incompatible\n\nThe pod requests `spec.hostNetwork: true`, which asks the kubelet to place the pod's network namespace inside the host's.\n",
						Evidence: schema.Evidence{
							HostNamespaces: []string{"hostNetwork"},
						},
						RemediationURL: "https://gvisor.dev/docs/architecture_guide/networking/",
					},
					{
						RuleID:      "raw-socket",
						Severity:    schema.SeverityError,
						Title:       "Container requests CAP_NET_RAW",
						WhyMarkdown: "## Why incompatible\n\nThe container requests `CAP_NET_RAW`, which lets it open raw sockets.\n",
						Evidence: schema.Evidence{
							Capabilities: []schema.CapabilityHit{
								{Container: "exporter", Capability: "CAP_NET_RAW"},
							},
						},
						RemediationURL: "https://gvisor.dev/docs/user_guide/networking/",
					},
				},
			},
		},
	}

	out := renderToString(t, makeNamespaceDoc(ns))

	wantSubs := []string{
		// Title.
		"agentmoat explain namespace kube-system",
		// SUMMARY block.
		"SUMMARY",
		"1 workloads",
		// Per-workload header: badge + identity + uppercase tag.
		"✗ incompatible",
		"DaemonSet kube-system/node-exporter",
		"INCOMPATIBLE",
		// Recommendation.
		"Recommendation: do not migrate",
		// Both finding rule IDs and severities show up.
		"host-network",
		"raw-socket",
		"(error)",
		// Evidence lines render the concrete shapes.
		"pod.spec.hostNetwork = true",
		`Container "exporter" has capability CAP_NET_RAW`,
		// Prose heading from at least one finding.
		"## Why incompatible",
		// Remediation link.
		"https://gvisor.dev/docs/architecture_guide/networking/",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(out, sub) {
			t.Errorf("output missing %q\nfull output:\n%s", sub, out)
		}
	}
}

// TestRenderNamespaceExplanation_Compatible asserts that a compatible
// workload renders the COMPATIBLE badge, no Findings section, and the
// full "what was checked" block with every bucket.
func TestRenderNamespaceExplanation_Compatible(t *testing.T) {
	t.Parallel()
	ns := &schema.NamespaceExplanation{
		Name: "app",
		Summary: schema.Summary{
			Total:      1,
			Compatible: 1,
		},
		Workloads: []schema.WorkloadExplanation{
			{
				Kind:           "Deployment",
				Namespace:      "app",
				Name:           "web-server",
				Compatibility:  schema.CompatibilityCompatible,
				Recommendation: "set runtimeClassName: gvisor",
				Findings:       nil,
				// Non-empty Checked: the renderer currently ignores the
				// per-rule content and prints the fixed buckets, but
				// passing a non-empty slice mirrors what the orchestrator
				// emits and keeps the test honest.
				Checked: []schema.RuleCheck{
					{RuleID: "host-network", Outcome: "did-not-fire"},
					{RuleID: "raw-socket", Outcome: "did-not-fire"},
				},
			},
		},
	}

	out := renderToString(t, makeNamespaceDoc(ns))

	// Verdict badge in upper-case tag form.
	if !strings.Contains(out, "COMPATIBLE") {
		t.Errorf("missing COMPATIBLE tag in output:\n%s", out)
	}
	// Badge + identity row.
	if !strings.Contains(out, "✓ compatible") {
		t.Errorf("missing ✓ compatible badge in output:\n%s", out)
	}
	if !strings.Contains(out, "Deployment app/web-server") {
		t.Errorf("missing identity row in output:\n%s", out)
	}

	// No Findings rendered: the finding bullet ▸ must not appear at
	// all for this fixture.
	if strings.ContainsRune(out, '▸') {
		t.Errorf("compatible workload should have no finding bullets, got:\n%s", out)
	}

	// "What was checked" header plus every bucket line.
	wantChecked := []string{
		"What was checked (no blocking rules fired):",
		"host namespaces: none used",
		"capabilities: none risky",
		"privileged: no privileged containers",
		"volumes: no hostPath, no FUSE, no /dev/kvm",
		"images: no risky hints",
		"annotations: none flagged",
	}
	for _, sub := range wantChecked {
		if !strings.Contains(out, sub) {
			t.Errorf("output missing checked-bucket line %q\nfull output:\n%s", sub, out)
		}
	}
}

// TestRenderNamespaceExplanation_Review asserts that a review-class
// workload renders the REVIEW tag plus the firing rule's finding and
// the brief "Other checks did not fire" line (rather than the full
// buckets summary that the compatible branch prints).
func TestRenderNamespaceExplanation_Review(t *testing.T) {
	t.Parallel()
	ns := &schema.NamespaceExplanation{
		Name: "edge",
		Summary: schema.Summary{
			Total:       1,
			NeedsReview: 1,
		},
		Workloads: []schema.WorkloadExplanation{
			{
				Kind:           "Deployment",
				Namespace:      "edge",
				Name:           "proxy",
				Compatibility:  schema.CompatibilityReview,
				Recommendation: "review network throughput before opting in",
				Overhead:       "Network throughput: 20-40%",
				Findings: []schema.RuleFinding{
					{
						RuleID:      "network-throughput",
						Severity:    schema.SeverityInfo,
						Title:       "Workload is network-bound",
						WhyMarkdown: "## Why review\n\nNetstack is slower than the host kernel's stack for high-throughput traffic.\n",
						Evidence: schema.Evidence{
							ImageMatches: []schema.ImageMatch{
								{Container: "proxy", Image: "haproxy:2.8", HintPattern: "haproxy"},
							},
						},
					},
				},
				Checked: []schema.RuleCheck{
					{RuleID: "host-network", Outcome: "did-not-fire"},
				},
			},
		},
	}

	out := renderToString(t, makeNamespaceDoc(ns))

	wantSubs := []string{
		"REVIEW",
		"⚠ review",
		"Deployment edge/proxy",
		"Recommendation: review network throughput before opting in",
		"network-throughput",
		"(info)",
		"## Why review",
		`Container "proxy" image "haproxy:2.8" matches hint "haproxy"`,
		// Review path prints the brief "Other checks" line, not the
		// full buckets summary.
		"Other checks (did not fire): 1 rules",
		// Overhead line gets printed when populated.
		"Expected overhead: Network throughput: 20-40%",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(out, sub) {
			t.Errorf("output missing %q\nfull output:\n%s", sub, out)
		}
	}

	// The compatible-only "What was checked" block must NOT appear here.
	if strings.Contains(out, "What was checked (no blocking rules fired):") {
		t.Errorf("review path should not render the compatible buckets header:\n%s", out)
	}
}

// TestRenderNamespaceExplanation_Empty asserts the placeholder behavior
// when Summary.Total is zero: title + SUMMARY headline + placeholder
// line, no per-workload sections.
func TestRenderNamespaceExplanation_Empty(t *testing.T) {
	t.Parallel()
	ns := &schema.NamespaceExplanation{
		Name:    "empty-ns",
		Summary: schema.Summary{Total: 0},
	}
	out := renderToString(t, makeNamespaceDoc(ns))

	if !strings.Contains(out, "agentmoat explain namespace empty-ns") {
		t.Errorf("missing title line:\n%s", out)
	}
	if !strings.Contains(out, "(no workloads in this namespace)") {
		t.Errorf("missing empty-namespace placeholder:\n%s", out)
	}
	// No badges, no finding bullets, no checked block.
	for _, banned := range []string{"✓ compatible", "⚠ review", "✗ incompatible", "▸", "What was checked"} {
		if strings.Contains(out, banned) {
			t.Errorf("empty-namespace output should not contain %q, got:\n%s", banned, out)
		}
	}
}

// TestRenderNamespaceExplanation_NoColorClean asserts no ANSI escapes
// leak into the output when the writer is non-TTY (bytes.Buffer) and
// NoColor=true is set. Mirrors the contract the rest of the package
// honors: piped or captured output is plain text.
func TestRenderNamespaceExplanation_NoColorClean(t *testing.T) {
	t.Parallel()
	ns := &schema.NamespaceExplanation{
		Name: "kube-system",
		Summary: schema.Summary{
			Total:        1,
			Incompatible: 1,
		},
		Workloads: []schema.WorkloadExplanation{
			{
				Kind:          "DaemonSet",
				Namespace:     "kube-system",
				Name:          "node-exporter",
				Compatibility: schema.CompatibilityIncompatible,
				Findings: []schema.RuleFinding{
					{
						RuleID:         "host-network",
						Severity:       schema.SeverityError,
						Title:          "host network",
						WhyMarkdown:    "## Why incompatible\n\nbody.\n",
						Evidence:       schema.Evidence{HostNamespaces: []string{"hostNetwork"}},
						RemediationURL: "https://example.test/",
					},
				},
			},
		},
	}

	var buf bytes.Buffer
	if err := Render(makeNamespaceDoc(ns), FormatTable, &buf, RenderOptions{NoColor: true}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("expected no ANSI escapes in NoColor output, got:\n%q", out)
	}
}
