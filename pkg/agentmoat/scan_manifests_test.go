// Tests for the manifest source of Scan, the scan gate, and the rule that a
// plan made from manifests is a preview.
//
// The fixtures are small inline manifests written to a temp directory. The
// one shared fixture on disk is test/e2e/manifests/workloads.yaml: it is the
// file `make e2e` applies to a real cluster, so the totals a file scan gives
// for it must be the totals the cluster scan gives.
package agentmoat

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// e2eWorkloads is the manifest file the kind e2e run applies.
const e2eWorkloads = "../../test/e2e/manifests/workloads.yaml"

// Manifest fragments for the tests below.
const (
	// A workload that asks for the sandbox and cannot run in it.
	sandboxedHostNet = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: gateway
  namespace: edge
spec:
  template:
    spec:
      runtimeClassName: gvisor
      hostNetwork: true
      containers:
        - name: gateway
          image: example.test/gateway:1
`
	// A workload that asks for the sandbox and can run in it.
	sandboxedClean = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: agent
  namespace: agents
spec:
  template:
    spec:
      runtimeClassName: gvisor
      containers:
        - name: agent
          image: example.test/agent:1
`
	// A workload that will never move, and does not ask to.
	privilegedDaemonSet = `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: cni
  namespace: kube-system
spec:
  template:
    spec:
      hostNetwork: true
      containers:
        - name: cni
          image: example.test/cni:1
          securityContext:
            privileged: true
`
	plainService = `apiVersion: v1
kind: Service
metadata:
  name: agent
  namespace: agents
`
)

// writeManifests writes files (slash-separated relative path -> content)
// under a new temp directory and returns the directory.
func writeManifests(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	return root
}

// scanFiles runs a manifest scan with progress output captured.
func scanFiles(t *testing.T, opts ScanOptions) (*schema.ScanReport, string) {
	t.Helper()
	var stderr bytes.Buffer
	opts.Stderr = &stderr
	report, err := Scan(context.Background(), opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	return report, stderr.String()
}

// TestScanManifests_E2EInventory: the file the kind run applies gives the
// totals its own header documents (14 workloads: 8 compatible, 3 review, 3
// incompatible), and the report says it came from manifests and from no
// cluster.
func TestScanManifests_E2EInventory(t *testing.T) {
	t.Parallel()
	report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{e2eWorkloads}, Namespaces: []string{"agentmoat-e2e"}})

	want := schema.Summary{Total: 14, Compatible: 8, NeedsReview: 3, Incompatible: 3, Sandboxed: 0}
	if report.Spec.Summary != want {
		t.Errorf("summary = %+v, want %+v", report.Spec.Summary, want)
	}
	meta := report.Metadata
	if meta.Cluster != "" || meta.ClusterFacts != nil || meta.Namespaces != nil {
		t.Errorf("a manifest scan must not claim a cluster: cluster=%q facts=%v namespaces=%v", meta.Cluster, meta.ClusterFacts, meta.Namespaces)
	}
	if meta.Source == nil || meta.Source.Type != schema.SourceManifests || meta.Source.Inputs != 1 {
		t.Errorf("source = %+v, want manifests with 1 input", meta.Source)
	}
	if meta.RuntimeClassName != schema.DefaultRuntimeClassName {
		t.Errorf("runtimeClassName = %q, want the default", meta.RuntimeClassName)
	}
	for _, w := range report.Spec.Workloads {
		if w.Namespace != "agentmoat-e2e" {
			t.Errorf("%s %s: namespace = %q, want the -n default", w.Kind, w.Name, w.Namespace)
		}
		if w.Origin == nil || w.Origin.File != e2eWorkloads || w.Origin.Line == 0 {
			t.Errorf("%s %s: origin = %+v, want the file and a line", w.Kind, w.Name, w.Origin)
		}
	}
}

// TestScanManifests_NeverBuildsAClient is the invariant test for the
// manifest source. Two traps are set, and both must stay untouched:
//
//   - KUBECONFIG names a context "canary-must-not-appear" on a server that
//     does not exist. (An empty kubeconfig would not do: its context name is
//     empty, so a leak into the report would not show.)
//   - opts.KubeClient is a fake client that fails the test on any call. The
//     MCP server always passes its client, so "ignore it" must be real.
func TestScanManifests_NeverBuildsAClient(t *testing.T) {
	kubeconfig := writeFile(t, "kubeconfig", `apiVersion: v1
kind: Config
current-context: canary-must-not-appear
clusters: [{name: canary, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: canary-must-not-appear, context: {cluster: canary, user: canary}}]
users: [{name: canary, user: {}}]
`)
	t.Setenv("KUBECONFIG", kubeconfig) // no t.Parallel: Setenv forbids it

	client := fake.NewSimpleClientset()
	client.PrependReactor("*", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
		t.Errorf("a manifest scan called the Kubernetes API: %s %s", action.GetVerb(), action.GetResource().Resource)
		return true, nil, errors.New("no API calls in a manifest scan")
	})

	report, stderr := scanFiles(t, ScanOptions{
		ManifestPaths:  []string{e2eWorkloads},
		KubeClient:     client,
		KubeconfigPath: kubeconfig,
		Context:        "canary-must-not-appear",
	})

	data, err := yaml.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, text := range []string{string(data), stderr} {
		if strings.Contains(text, "canary-must-not-appear") {
			t.Errorf("the kubeconfig leaked into the output:\n%s", text)
		}
	}
	if n := len(client.Actions()); n != 0 {
		t.Errorf("%d API actions recorded, want 0", n)
	}
	if strings.Contains(stderr, "scanning cluster") {
		t.Errorf("stderr talks about a cluster:\n%s", stderr)
	}
}

// mixedTree is four files: two sandboxed Deployments (one clean, one with
// host networking), a privileged DaemonSet in kube-system, an Argo Rollout,
// and a Service.
func mixedTree(t *testing.T) string {
	t.Helper()
	return writeManifests(t, map[string]string{
		"apps/gateway.yaml": sandboxedHostNet,
		"apps/agent.yaml":   sandboxedClean + "---\n" + plainService,
		"system/cni.yaml":   privilegedDaemonSet,
		"rollout.yaml": "apiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata: {name: canary, namespace: edge}\n" +
			"spec: {template: {spec: {containers: [{name: c, image: busybox, securityContext: {privileged: true}}]}}}\n",
	})
}

func TestScanManifests_Workloads(t *testing.T) {
	t.Parallel()
	report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{mixedTree(t)}})

	type row struct {
		kind, namespace, name, runtime string
		verdict                        schema.Compatibility
	}
	var got []row
	for _, w := range report.Spec.Workloads {
		got = append(got, row{w.Kind, w.Namespace, w.Name, w.RuntimeClassName, w.Compatibility})
	}
	want := []row{
		{"Deployment", "agents", "agent", "gvisor", schema.CompatibilityCompatible},
		{"Deployment", "edge", "gateway", "gvisor", schema.CompatibilityIncompatible},
		// A kind the cluster scan does not list, classified from its pod
		// template and named with its API group.
		{"Rollout.argoproj.io", "edge", "canary", "", schema.CompatibilityIncompatible},
		// A system namespace is not skipped: the user pointed at the file.
		{"DaemonSet", "kube-system", "cni", "", schema.CompatibilityIncompatible},
	}
	if len(got) != len(want) {
		t.Fatalf("workloads = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("workload %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if s := report.Spec.Summary; s.Sandboxed != 2 || s.Total != 4 {
		t.Errorf("summary = %+v, want 4 total and 2 sandboxed", s)
	}
}

// TestScanManifests_SourceAndOrigin: what the report says about its input.
func TestScanManifests_SourceAndOrigin(t *testing.T) {
	t.Parallel()
	report, stderr := scanFiles(t, ScanOptions{ManifestPaths: []string{mixedTree(t)}})

	src := report.Metadata.Source
	if src.Inputs != 4 || len(src.Skipped) != 1 || src.Skipped["Service"] != 1 {
		t.Errorf("source = %+v, want 4 inputs and one skipped Service", src)
	}
	if src.Duplicates != nil || src.Unrendered != nil {
		t.Errorf("source has duplicates or unrendered signs for clean input: %+v", src)
	}

	// The origin is the path as given, with the line of the document.
	agent := report.Spec.Workloads[0]
	if agent.Origin == nil || !strings.HasSuffix(agent.Origin.File, "apps/agent.yaml") || agent.Origin.Line != 1 {
		t.Errorf("origin of agent = %+v", agent.Origin)
	}
	if !strings.Contains(stderr, "scanned 4 workloads from 4 manifest input(s)") {
		t.Errorf("stderr lacks the summary line:\n%s", stderr)
	}
}

func TestScanManifests_Stdin(t *testing.T) {
	t.Parallel()
	// The shape of `helm template` output: a "# Source:" comment above
	// each document.
	rendered := "---\n# Source: shop/templates/gateway.yaml\n" + sandboxedHostNet +
		"---\n" + sandboxedClean // no Source comment on this one

	report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{"-"}, Stdin: strings.NewReader(rendered)})
	if len(report.Spec.Workloads) != 2 {
		t.Fatalf("workloads = %+v, want 2", report.Spec.Workloads)
	}
	// Sorted by namespace: agents/agent, then edge/gateway.
	agent, gateway := report.Spec.Workloads[0], report.Spec.Workloads[1]

	// With a Helm source path, the origin is that file and has no line:
	// the line would count in the rendered stream, not in the template.
	if gateway.Origin == nil || gateway.Origin.File != "shop/templates/gateway.yaml" || gateway.Origin.Line != 0 {
		t.Errorf("origin of gateway = %+v, want the Helm source path and no line", gateway.Origin)
	}
	if agent.Origin == nil || agent.Origin.File != "<stdin>" || agent.Origin.Line == 0 {
		t.Errorf("origin of agent = %+v, want <stdin> and a line", agent.Origin)
	}

	t.Run("a dash with no stdin is an error", func(t *testing.T) {
		t.Parallel()
		_, err := Scan(context.Background(), ScanOptions{ManifestPaths: []string{"-"}, Stderr: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "standard input") {
			t.Errorf("err = %v, want an error about standard input", err)
		}
	})
}

func TestScanManifests_Options(t *testing.T) {
	t.Parallel()
	const bare = "apiVersion: v1\nkind: Pod\nmetadata: {name: bare, labels: {tier: web}}\nspec: {containers: [{name: c, image: busybox}]}\n"
	dir := writeManifests(t, map[string]string{"pod.yaml": bare, "gateway.yaml": sandboxedHostNet})

	t.Run("one namespace is the default for objects that name none, not a filter", func(t *testing.T) {
		t.Parallel()
		report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{dir}, Namespaces: []string{"team-a"}})
		var got []string
		for _, w := range report.Spec.Workloads {
			got = append(got, w.Namespace+"/"+w.Name)
		}
		if strings.Join(got, " ") != "edge/gateway team-a/bare" {
			t.Errorf("workloads = %v, want edge/gateway (kept) and team-a/bare (filled)", got)
		}
		if report.Metadata.Namespaces != nil {
			t.Errorf("metadata.namespaces = %v, want nil: it is not a filter here", report.Metadata.Namespaces)
		}
	})

	t.Run("no namespace leaves it empty and says so", func(t *testing.T) {
		t.Parallel()
		report, stderr := scanFiles(t, ScanOptions{ManifestPaths: []string{dir}})
		if report.Spec.Workloads[0].Namespace != "" {
			t.Errorf("first workload = %+v, want the Pod with an empty namespace", report.Spec.Workloads[0])
		}
		if !strings.Contains(stderr, "1 workload(s) name no namespace") {
			t.Errorf("stderr lacks the namespace note:\n%s", stderr)
		}
	})

	t.Run("two namespaces are an error", func(t *testing.T) {
		t.Parallel()
		_, err := Scan(context.Background(), ScanOptions{ManifestPaths: []string{dir}, Namespaces: []string{"a", "b"}, Stderr: io.Discard})
		if err == nil || !strings.Contains(err.Error(), "one namespace") {
			t.Errorf("err = %v, want the one-namespace error", err)
		}
	})

	t.Run("a label selector filters on the workload's own labels", func(t *testing.T) {
		t.Parallel()
		report, stderr := scanFiles(t, ScanOptions{ManifestPaths: []string{dir}, LabelSelector: "tier=web"})
		if len(report.Spec.Workloads) != 1 || report.Spec.Workloads[0].Name != "bare" {
			t.Errorf("workloads = %+v, want only the Pod with the label", report.Spec.Workloads)
		}
		if !strings.Contains(stderr, "1 workload(s) that do not match the label selector") {
			t.Errorf("stderr lacks the filter note:\n%s", stderr)
		}
	})

	t.Run("another runtime class name moves the sandboxed count", func(t *testing.T) {
		t.Parallel()
		report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{dir}, RuntimeClassName: "kata"})
		if report.Metadata.RuntimeClassName != "kata" || report.Spec.Summary.Sandboxed != 0 {
			t.Errorf("class = %q sandboxed = %d, want kata and 0", report.Metadata.RuntimeClassName, report.Spec.Summary.Sandboxed)
		}
	})

	t.Run("a rules override applies", func(t *testing.T) {
		t.Parallel()
		rules := writeFile(t, "rules.yaml", "overrides:\n  - id: host-network\n    severity: warn\n")
		report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{filepath.Join(dir, "gateway.yaml")}, RulesYAMLPath: rules})
		if got := report.Spec.Workloads[0].Compatibility; got != schema.CompatibilityReview {
			t.Errorf("gateway = %s, want review after the override", got)
		}
	})
}

func TestScanManifests_Errors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   map[string]string
		wantErr []string
	}{
		{
			name:    "an unrendered Helm template names the file and the fix",
			files:   map[string]string{"templates/deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ .Release.Name }}\n"},
			wantErr: []string{"templates/deploy.yaml:1: a mapping key is itself a mapping", "helm template"},
		},
		{
			name:    "every broken file is listed",
			files:   map[string]string{"a.yaml": "apiVersion: v1\nkind: [bad\n", "b.yaml": "apiVersion: v1\nkind: {bad\n"},
			wantErr: []string{"2 manifest problem(s)", "a.yaml", "b.yaml"},
		},
		{
			name:    "a directory with no manifest file",
			files:   map[string]string{"README.md": "nothing here\n"},
			wantErr: []string{"no manifests in"},
		},
		{
			name:    "YAML files with no apiVersion",
			files:   map[string]string{"values.yaml": "replicas: 2\n"},
			wantErr: []string{"none has an apiVersion"},
		},
		{
			name:    "only a kustomization file",
			files:   map[string]string{"kustomization.yaml": "resources:\n  - github.com/example/base\n"},
			wantErr: []string{"only a kustomization file", "kustomize build"},
		},
		{
			name:    "a file of comments only",
			files:   map[string]string{"empty.yaml": "# apiVersion: v1 (commented out)\n"},
			wantErr: []string{"no Kubernetes objects"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := writeManifests(t, tc.files)
			report, err := Scan(context.Background(), ScanOptions{ManifestPaths: []string{dir}, Stderr: io.Discard})
			if err == nil {
				t.Fatalf("Scan succeeded with %+v, want an error", report.Spec.Summary)
			}
			for _, sub := range tc.wantErr {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error lacks %q:\n%s", sub, err)
				}
			}
		})
	}

	t.Run("config only is a result, not an error", func(t *testing.T) {
		t.Parallel()
		dir := writeManifests(t, map[string]string{"svc.yaml": plainService})
		report, stderr := scanFiles(t, ScanOptions{ManifestPaths: []string{dir}})
		if report.Spec.Summary.Total != 0 || report.Metadata.Source.Skipped["Service"] != 1 {
			t.Errorf("summary = %+v source = %+v, want 0 workloads and one skipped Service", report.Spec.Summary, report.Metadata.Source)
		}
		if !strings.Contains(stderr, "the input has no workloads") {
			t.Errorf("stderr lacks the no-workloads note:\n%s", stderr)
		}
	})
}

// kustomizeTree is a base that is incompatible and not sandboxed, and an
// overlay patch that puts it in the sandbox. What gets deployed is a
// sandboxed workload with host networking. Read file by file, no single
// file says so.
func kustomizeTree(t *testing.T) string {
	t.Helper()
	return writeManifests(t, map[string]string{
		"base/web.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n" +
			"spec: {template: {spec: {hostNetwork: true, containers: [{name: web, image: nginx}]}}}\n",
		"base/kustomization.yaml": "resources:\n  - web.yaml\n",
		"overlay/patch.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n" +
			"spec: {template: {spec: {runtimeClassName: gvisor}}}\n",
		"overlay/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources: [../base]\npatches: [{path: patch.yaml}]\n",
	})
}

// TestScanManifests_KustomizeTreeIsNotAPass is the gate-soundness test.
// Without the "partial object" rule, the patch is a compatible, sandboxed
// Deployment, the base is not sandboxed, and --fail-on sandboxed exits 0 for
// a change that deploys a sandboxed workload with host networking.
func TestScanManifests_KustomizeTreeIsNotAPass(t *testing.T) {
	t.Parallel()
	report, stderr := scanFiles(t, ScanOptions{ManifestPaths: []string{kustomizeTree(t)}})

	// One workload, the base. The patch is not a second one.
	if len(report.Spec.Workloads) != 1 || report.Spec.Workloads[0].RuntimeClassName != "" ||
		report.Spec.Workloads[0].Compatibility != schema.CompatibilityIncompatible {
		t.Fatalf("workloads = %+v, want only the incompatible base with no runtime class", report.Spec.Workloads)
	}
	if report.Spec.Summary.Sandboxed != 0 {
		t.Errorf("sandboxed = %d, want 0: the patch must not count as a sandboxed workload", report.Spec.Summary.Sandboxed)
	}

	signs := strings.Join(report.Metadata.Source.Unrendered, "\n")
	for _, want := range []string{
		"kustomization file: ", "base/kustomization.yaml", "overlay/kustomization.yaml",
		"partial object: Deployment -/web", `sets runtimeClassName "gvisor"`,
	} {
		if !strings.Contains(signs, want) {
			t.Errorf("unrendered signs lack %q:\n%s", want, signs)
		}
	}
	if len(report.Metadata.Source.Unrendered) != 3 {
		t.Errorf("unrendered = %v, want 3 entries (a kustomization file found by name and by kind is one entry)", report.Metadata.Source.Unrendered)
	}
	if !strings.Contains(stderr, "warning: this input is not rendered") {
		t.Errorf("stderr lacks the warning:\n%s", stderr)
	}

	// The sandboxed gate refuses to answer; it does not pass.
	gate, err := EvaluateGate(report, FailOnSandboxed)
	if !errors.Is(err, ErrUnrenderedInput) {
		t.Fatalf("EvaluateGate(sandboxed) = %+v, %v; want ErrUnrenderedInput", gate, err)
	}
	if !strings.Contains(err.Error(), "kustomize build") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}

	// The default gate still works on what it can see (the base).
	gate, err = EvaluateGate(report, FailOnIncompatible)
	if err != nil || !gate.Failed {
		t.Errorf("EvaluateGate(incompatible) = %+v, %v; want a failure and no error", gate, err)
	}
	if gate, err = EvaluateGate(report, FailOnNone); err != nil || gate.Failed {
		t.Errorf("EvaluateGate(none) = %+v, %v; want a pass", gate, err)
	}
}

func TestParseFailOn(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]FailOn{
		"": FailOnIncompatible, "incompatible": FailOnIncompatible, "sandboxed": FailOnSandboxed, "none": FailOnNone,
	} {
		got, err := ParseFailOn(in)
		if err != nil || got != want {
			t.Errorf("ParseFailOn(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := ParseFailOn("review"); err == nil || !strings.Contains(err.Error(), "incompatible, sandboxed, none") {
		t.Errorf("ParseFailOn(review) err = %v, want the list of valid values", err)
	}
}

// gateReport builds a report with the given rows, for the gate tests.
func gateReport(runtimeClass string, rows ...schema.WorkloadResult) *schema.ScanReport {
	r := schema.NewScanReport()
	r.Metadata.RuntimeClassName = runtimeClass
	r.Spec.Workloads = rows
	return r
}

func gateRow(name, runtimeClass string, verdict schema.Compatibility) schema.WorkloadResult {
	w := schema.WorkloadResult{Kind: "Deployment", Namespace: "ns", Name: name, RuntimeClassName: runtimeClass, Compatibility: verdict}
	if verdict == schema.CompatibilityIncompatible {
		w.Reasons = []schema.Reason{
			{RuleID: "host-network", Severity: schema.SeverityError},
			{RuleID: "network-throughput", Severity: schema.SeverityInfo},
		}
	}
	return w
}

func TestEvaluateGate(t *testing.T) {
	t.Parallel()
	const (
		ok  = schema.CompatibilityCompatible
		rev = schema.CompatibilityReview
		bad = schema.CompatibilityIncompatible
	)
	tests := []struct {
		name       string
		report     *schema.ScanReport
		failOn     FailOn
		wantFailed bool
		wantText   []string // each must be in the joined messages
		wantNoText bool     // no messages at all
	}{
		{
			name:       "incompatible: any incompatible workload fails, with no message (as before)",
			report:     gateReport("gvisor", gateRow("a", "", ok), gateRow("b", "", bad)),
			failOn:     FailOnIncompatible,
			wantFailed: true, wantNoText: true,
		},
		{
			name:   "incompatible: review alone passes",
			report: gateReport("gvisor", gateRow("a", "", rev)),
			failOn: FailOnIncompatible, wantNoText: true,
		},
		{
			name:   "none: never fails",
			report: gateReport("gvisor", gateRow("a", "gvisor", bad)),
			failOn: FailOnNone, wantNoText: true,
		},
		{
			name:   "sandboxed: an incompatible workload outside the sandbox does not fail",
			report: gateReport("gvisor", gateRow("cni", "", bad), gateRow("agent", "gvisor", ok)),
			failOn: FailOnSandboxed,
			wantText: []string{
				`gate passed: 1 workload(s) ask for RuntimeClass "gvisor"`,
			},
		},
		{
			name:       "sandboxed: an incompatible workload in the sandbox fails and is named",
			report:     gateReport("gvisor", gateRow("cni", "", bad), gateRow("gateway", "gvisor", bad)),
			failOn:     FailOnSandboxed,
			wantFailed: true,
			// Error-severity rules only: the info rule is not why it failed.
			wantText: []string{"gate failed", "Deployment ns/gateway: host-network"},
		},
		{
			name:     "sandboxed: review in the sandbox passes",
			report:   gateReport("gvisor", gateRow("a", "gvisor", rev)),
			failOn:   FailOnSandboxed,
			wantText: []string{"gate passed"},
		},
		{
			name:   "sandboxed: nothing asks for the class, and the names in use are shown",
			report: gateReport("gvisor", gateRow("a", "gvisor-runsc", bad), gateRow("b", "gvisor-runsc", ok), gateRow("c", "", ok)),
			failOn: FailOnSandboxed,
			wantText: []string{
				`no workload asks for RuntimeClass "gvisor"`, "checked nothing", "2 gvisor-runsc", "--runtime-class",
			},
		},
		{
			name:       "sandboxed: the class name comes from the report",
			report:     gateReport("gvisor-runsc", gateRow("a", "gvisor-runsc", bad)),
			failOn:     FailOnSandboxed,
			wantFailed: true,
			wantText:   []string{`RuntimeClass "gvisor-runsc"`},
		},
		{
			name:       "sandboxed: a report with no class name (an old one) means the default",
			report:     gateReport("", gateRow("a", "gvisor", bad)),
			failOn:     FailOnSandboxed,
			wantFailed: true,
		},
		{
			// Two copies of one workload (two overlays with a full
			// Deployment each). One asks for the sandbox, the other is
			// incompatible. Which one is deployed is not knowable, so the
			// workload is both.
			name:       "sandboxed: copies of one workload are read as a union",
			report:     gateReport("gvisor", gateRow("web", "gvisor", ok), gateRow("web", "", bad)),
			failOn:     FailOnSandboxed,
			wantFailed: true,
			wantText:   []string{"Deployment ns/web: host-network"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gate, err := EvaluateGate(tc.report, tc.failOn)
			if err != nil {
				t.Fatalf("EvaluateGate: %v", err)
			}
			if gate.Failed != tc.wantFailed {
				t.Errorf("Failed = %v, want %v (messages: %v)", gate.Failed, tc.wantFailed, gate.Messages)
			}
			text := strings.Join(gate.Messages, "\n")
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Errorf("messages lack %q:\n%s", want, text)
				}
			}
			if tc.wantNoText && text != "" {
				t.Errorf("messages = %q, want none", text)
			}
		})
	}
}

// TestGateAndSummaryAgreeOnTheClass: the summary count and the gate resolve
// the sandbox class the same way, with and without an explicit name.
func TestGateAndSummaryAgreeOnTheClass(t *testing.T) {
	t.Parallel()
	dir := writeManifests(t, map[string]string{"gateway.yaml": sandboxedHostNet})
	for _, class := range []string{"", "gvisor"} {
		report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{dir}, RuntimeClassName: class})
		gate, err := EvaluateGate(report, FailOnSandboxed)
		if err != nil {
			t.Fatalf("class %q: EvaluateGate: %v", class, err)
		}
		if report.Spec.Summary.Sandboxed != 1 || !gate.Failed {
			t.Errorf("class %q: sandboxed = %d, gate failed = %v; want 1 and true", class, report.Spec.Summary.Sandboxed, gate.Failed)
		}
		// The failure names where the workload is defined.
		if text := strings.Join(gate.Messages, "\n"); !strings.Contains(text, "gateway.yaml:1") {
			t.Errorf("class %q: the gate message lacks the origin:\n%s", class, text)
		}
	}
}

// TestPlanFromManifests_IsAPreview: a plan made from a manifest scan is
// marked, warns first, and is refused by everything that acts on a cluster.
func TestPlanFromManifests_IsAPreview(t *testing.T) {
	t.Parallel()
	dir := writeManifests(t, map[string]string{"agent.yaml": sandboxedClean, "gateway.yaml": sandboxedHostNet})

	var stderr bytes.Buffer
	plan, err := Plan(context.Background(), PlanOptions{
		ScanOptions: ScanOptions{ManifestPaths: []string{dir}, Stderr: io.Discard},
		Stderr:      &stderr,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Metadata.Source == nil || plan.Metadata.Source.Type != schema.SourceManifests {
		t.Fatalf("plan source = %+v, want manifests", plan.Metadata.Source)
	}
	if plan.Metadata.Cluster != "" {
		t.Errorf("plan cluster = %q, want empty", plan.Metadata.Cluster)
	}
	if len(plan.Spec.Warnings) == 0 || plan.Spec.Warnings[0].ID != WarningManifestSource {
		t.Errorf("warnings = %+v, want %s first", plan.Spec.Warnings, WarningManifestSource)
	}
	if plan.Spec.Summary.Included != 1 {
		t.Errorf("included = %d, want 1 (the compatible Deployment)", plan.Spec.Summary.Included)
	}
	if !strings.Contains(stderr.String(), "it is a preview") {
		t.Errorf("stderr does not say the plan is a preview:\n%s", stderr.String())
	}

}

// TestPlanFromManifests_IsRefused: everything that acts on a cluster refuses
// a plan made from manifests, before it makes one API call.
func TestPlanFromManifests_IsRefused(t *testing.T) {
	t.Parallel()
	dir := writeManifests(t, map[string]string{"agent.yaml": sandboxedClean})
	plan, err := Plan(context.Background(), PlanOptions{
		ScanOptions: ScanOptions{ManifestPaths: []string{dir}, Stderr: io.Discard},
		Stderr:      io.Discard,
	})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	data, err := yaml.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	planPath := writeFile(t, "plan.yaml", string(data))

	client := fake.NewSimpleClientset()
	wantRefusal := []string{"cannot be used here", "made from a scan of manifests", "agentmoat plan"}
	check := func(verb string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s loaded a plan made from manifests", verb)
			return
		}
		for _, sub := range wantRefusal {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("%s: error lacks %q: %v", verb, sub, err)
			}
		}
	}
	_, err = Apply(context.Background(), ApplyOptions{PlanPath: planPath, DryRun: true, KubeClient: client, Stderr: io.Discard})
	check("apply", err)
	_, err = Rollback(context.Background(), RollbackOptions{PlanPath: planPath, DryRun: true, KubeClient: client, Stderr: io.Discard})
	check("rollback", err)
	_, err = Verify(context.Background(), VerifyOptions{PlanPath: planPath, KubeClient: client, Stderr: io.Discard})
	check("verify", err)
	if n := len(client.Actions()); n != 0 {
		t.Errorf("%d API actions on a refused plan, want 0", n)
	}

	// The mark is not in the hash: the same steps from a cluster scan hash
	// the same. Remove the mark and the plan must pass its integrity check
	// and load.
	unmarked := *plan
	unmarked.Metadata.Source = nil
	data, err = yaml.Marshal(&unmarked)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := loadPlan(writeFile(t, "unmarked.yaml", string(data))); err != nil {
		t.Errorf("the same plan without the mark does not load (is the mark in the hash?): %v", err)
	}
}

// TestPlanFromManifestReportFile: the same, through `plan --scan <file>`.
func TestPlanFromManifestReportFile(t *testing.T) {
	t.Parallel()
	dir := writeManifests(t, map[string]string{"agent.yaml": sandboxedClean})
	report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{dir}})
	data, err := yaml.Marshal(report)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	plan, err := Plan(context.Background(), PlanOptions{ScanReportPath: writeFile(t, "scan.yaml", string(data)), Stderr: io.Discard})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if plan.Metadata.Source == nil || plan.Metadata.Source.Type != schema.SourceManifests {
		t.Errorf("plan source = %+v, want manifests", plan.Metadata.Source)
	}
}

// TestClusterScan_RuntimeClassFields: the cluster path fills the new fields
// too, and has no source block.
func TestClusterScan_RuntimeClassFields(t *testing.T) {
	t.Parallel()
	pod, ok := privilegedPod("ns", "p").(*corev1.Pod)
	if !ok {
		t.Fatal("privilegedPod did not return a *corev1.Pod")
	}
	class := "gvisor"
	pod.Spec.RuntimeClassName = &class
	report, err := Scan(context.Background(), ScanOptions{
		KubeClient: fake.NewSimpleClientset(pod), SkipClusterFacts: true, Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Metadata.Source != nil || report.Metadata.RuntimeClassName != "gvisor" {
		t.Errorf("metadata = %+v, want no source and the default class", report.Metadata)
	}
	if len(report.Spec.Workloads) != 1 {
		t.Fatalf("workloads = %+v", report.Spec.Workloads)
	}
	w := report.Spec.Workloads[0]
	if w.RuntimeClassName != "gvisor" || w.Origin != nil || report.Spec.Summary.Sandboxed != 1 {
		t.Errorf("workload = %+v summary = %+v; want runtime gvisor, no origin, 1 sandboxed", w, report.Spec.Summary)
	}
	gate, err := EvaluateGate(report, FailOnSandboxed)
	if err != nil || !gate.Failed {
		t.Errorf("EvaluateGate = %+v, %v; want a failure: a privileged Pod asks for gvisor", gate, err)
	}
}
