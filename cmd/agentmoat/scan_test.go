// Tests for `agentmoat scan -f`: the manifest form of scan and the
// --fail-on gate, driven through the root command as a user would.
//
// None of these tests has a cluster. That is the point of -f, and the first
// test sets a kubeconfig trap to prove the command never looks for one.
package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// e2eWorkloads is the manifest file `make e2e` applies to the kind cluster.
// Its header documents 14 workloads: 8 compatible, 3 review, 3 incompatible.
const e2eWorkloads = "../../test/e2e/manifests/workloads.yaml"

// runScan drives the root command like runRoot, with a reader for standard
// input, and also returns the exit code the command chose.
func runScanCmd(stdin io.Reader, args ...string) (stdout, stderr string, code int, err error) {
	exitCode = 0
	cmd := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	if stdin != nil {
		cmd.SetIn(stdin)
	}
	cmd.SetArgs(args)
	err = cmd.Execute()
	code = exitCode
	if err != nil {
		code = 1 // what Execute() returns for any error
	}
	return outBuf.String(), errBuf.String(), code, err
}

// writeScanFiles writes files (slash-separated relative path -> content)
// under a new temp directory and returns the directory.
func writeScanFiles(t *testing.T, files map[string]string) string {
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

// A Deployment that asks for the sandbox and cannot run in it.
const sandboxedHostNetwork = `apiVersion: apps/v1
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

func TestScanHelpListsManifestFlags(t *testing.T) {
	stdout, _, _, err := runScanCmd(nil, "scan", "--help")
	if err != nil {
		t.Fatalf("scan --help: %v", err)
	}
	for _, want := range []string{"-f, --filename", "--fail-on", "incompatible | sandboxed | none", "helm template ./chart | agentmoat scan -f -"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// TestScanFile_NoCluster: the e2e manifests scan to the documented totals
// with a kubeconfig that would fail (and would leave a mark) if it were read.
func TestScanFile_NoCluster(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
current-context: canary-must-not-appear
clusters: [{name: canary, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: canary-must-not-appear, context: {cluster: canary, user: canary}}]
users: [{name: canary, user: {}}]
`), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	stdout, stderr, code, err := runScanCmd(nil, "scan", "-f", e2eWorkloads, "-o", "json")
	if err != nil {
		t.Fatalf("scan -f: %v\nstderr: %s", err, stderr)
	}
	// The default gate: 3 workloads are incompatible, so exit 2.
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	var report schema.ScanReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not a ScanReport: %v\n%s", err, stdout)
	}
	want := schema.Summary{Total: 14, Compatible: 8, NeedsReview: 3, Incompatible: 3}
	if report.Spec.Summary != want {
		t.Errorf("summary = %+v, want %+v", report.Spec.Summary, want)
	}
	if report.Metadata.Source == nil || report.Metadata.Source.Type != schema.SourceManifests {
		t.Errorf("source = %+v, want manifests", report.Metadata.Source)
	}
	for _, text := range []string{stdout, stderr} {
		if strings.Contains(text, "canary-must-not-appear") || strings.Contains(text, `"cluster"`) || strings.Contains(text, "clusterFacts") {
			t.Errorf("the output mentions a cluster:\n%s", text)
		}
	}
}

func TestScanFile_FailOn(t *testing.T) {
	sandboxed := writeScanFiles(t, map[string]string{"gateway.yaml": sandboxedHostNetwork})

	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string
	}{
		{name: "default: an incompatible workload fails", args: []string{"-f", e2eWorkloads}, wantCode: 2},
		{name: "none: never fails", args: []string{"-f", e2eWorkloads, "--fail-on", "none"}, wantCode: 0},
		{
			// 3 incompatible workloads, none of which asks for gvisor.
			name: "sandboxed: incompatible workloads outside the sandbox pass, and the gate says it checked nothing",
			args: []string{"-f", e2eWorkloads, "--fail-on", "sandboxed"}, wantCode: 0,
			wantStderr: `no workload asks for RuntimeClass "gvisor"`,
		},
		{
			name: "sandboxed: a sandboxed workload that is incompatible fails",
			args: []string{"-f", sandboxed, "--fail-on", "sandboxed"}, wantCode: 2,
			wantStderr: "Deployment edge/gateway: host-network",
		},
		{
			name: "sandboxed: under another class name the same workload is not sandboxed",
			args: []string{"-f", sandboxed, "--fail-on", "sandboxed", "--runtime-class", "kata"}, wantCode: 0,
			wantStderr: "1 gvisor",
		},
		{
			name: "quiet hides the gate lines and keeps the exit code",
			args: []string{"-f", sandboxed, "--fail-on", "sandboxed", "-q"}, wantCode: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code, err := runScanCmd(nil, append([]string{"scan", "-o", "json"}, tc.args...)...)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\nstderr: %s", code, tc.wantCode, stderr)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr lacks %q:\n%s", tc.wantStderr, stderr)
			}
			if tc.wantStderr == "" && strings.Contains(stderr, "gate") {
				t.Errorf("stderr has a gate line where none is expected:\n%s", stderr)
			}
		})
	}
}

// TestScanFile_BadFailOnFailsFirst: a wrong --fail-on value is reported
// before any file is read (the path here does not exist).
func TestScanFile_BadFailOnFailsFirst(t *testing.T) {
	_, _, _, err := runScanCmd(nil, "scan", "-f", "/no/such/dir", "--fail-on", "review")
	if err == nil || !strings.Contains(err.Error(), "unknown --fail-on value") {
		t.Errorf("err = %v, want the --fail-on error", err)
	}
}

// TestScanFile_UnrenderedInput: a Kustomize tree whose overlay patch puts an
// incompatible base in the sandbox. The report is printed, the default gate
// answers for what it can see, and --fail-on sandboxed refuses (exit 1).
func TestScanFile_UnrenderedInput(t *testing.T) {
	dir := writeScanFiles(t, map[string]string{
		"base/web.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n" +
			"spec: {template: {spec: {hostNetwork: true, containers: [{name: web, image: nginx}]}}}\n",
		"base/kustomization.yaml": "resources:\n  - web.yaml\n",
		"overlay/patch.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n" +
			"spec: {template: {spec: {runtimeClassName: gvisor}}}\n",
		"overlay/kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [../base]\n",
	})

	stdout, stderr, code, err := runScanCmd(nil, "scan", "-f", dir, "--no-color")
	if err != nil || code != 2 {
		t.Fatalf("default gate: code = %d err = %v, want 2 and no error", code, err)
	}
	for _, want := range []string{"not rendered", "partial object: Deployment -/web", "kustomize build <dir> | agentmoat scan -f -"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the table lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "warning: this input is not rendered") {
		t.Errorf("stderr lacks the warning:\n%s", stderr)
	}

	stdout, _, code, err = runScanCmd(nil, "scan", "-f", dir, "--fail-on", "sandboxed", "-o", "json")
	if err == nil || code != 1 {
		t.Fatalf("sandboxed gate: code = %d err = %v, want exit 1 and a refusal", code, err)
	}
	if !strings.Contains(err.Error(), "not rendered") || !strings.Contains(err.Error(), "kustomize build") {
		t.Errorf("the refusal does not explain itself: %v", err)
	}
	// The report was still written: it is correct about the files.
	var report schema.ScanReport
	if jsonErr := json.Unmarshal([]byte(stdout), &report); jsonErr != nil || len(report.Metadata.Source.Unrendered) == 0 {
		t.Errorf("stdout is not a report with unrendered signs (%v):\n%s", jsonErr, stdout)
	}
}

// TestScanFile_ParseErrorPrintsNoReport: input that cannot be read is exit 1
// with nothing on stdout. A partial report would look like a clean one.
func TestScanFile_ParseErrorPrintsNoReport(t *testing.T) {
	dir := writeScanFiles(t, map[string]string{
		"good.yaml":            sandboxedHostNetwork,
		"templates/chart.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ include \"x.fullname\" . }}\n",
	})
	stdout, _, code, err := runScanCmd(nil, "scan", "-f", dir, "-o", "json")
	if err == nil || code != 1 {
		t.Fatalf("code = %d err = %v, want exit 1", code, err)
	}
	if stdout != "" {
		t.Errorf("stdout is not empty:\n%s", stdout)
	}
	if !strings.Contains(err.Error(), "helm template") {
		t.Errorf("the error does not point to helm template: %v", err)
	}
}

func TestScanFile_Stdin(t *testing.T) {
	stdout, stderr, code, err := runScanCmd(strings.NewReader(sandboxedHostNetwork), "scan", "-f", "-", "-o", "json")
	if err != nil || code != 2 {
		t.Fatalf("code = %d err = %v\nstderr: %s", code, err, stderr)
	}
	var report schema.ScanReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("stdout is not a ScanReport: %v", err)
	}
	if len(report.Spec.Workloads) != 1 || report.Spec.Workloads[0].Origin == nil || report.Spec.Workloads[0].Origin.File != "<stdin>" {
		t.Errorf("workloads = %+v, want one from <stdin>", report.Spec.Workloads)
	}

	// Empty input is an error that names standard input, not "-".
	_, _, code, err = runScanCmd(strings.NewReader(""), "scan", "-f", "-")
	if err == nil || code != 1 || !strings.Contains(err.Error(), "standard input") {
		t.Errorf("empty stdin: code = %d err = %v, want exit 1 that names standard input", code, err)
	}
}

func TestScanFile_ClusterFlags(t *testing.T) {
	dir := writeScanFiles(t, map[string]string{"gateway.yaml": sandboxedHostNetwork})

	t.Run("cluster-only flags are named as not used", func(t *testing.T) {
		_, stderr, _, err := runScanCmd(nil, "scan", "-f", dir, "--context", "prod", "--include-system", "-A", "-o", "json")
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if !strings.Contains(stderr, "not used: --context, --all-namespaces, --include-system") {
			t.Errorf("stderr lacks the not-used note:\n%s", stderr)
		}
	})

	t.Run("no note without them", func(t *testing.T) {
		_, stderr, _, err := runScanCmd(nil, "scan", "-f", dir, "-n", "team-a", "-o", "json")
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		if strings.Contains(stderr, "not used") {
			t.Errorf("stderr has a not-used note:\n%s", stderr)
		}
	})

	t.Run("two namespaces are an error", func(t *testing.T) {
		_, _, code, err := runScanCmd(nil, "scan", "-f", dir, "-n", "a", "-n", "b")
		if err == nil || code != 1 || !strings.Contains(err.Error(), "one namespace") {
			t.Errorf("code = %d err = %v, want the one-namespace error", code, err)
		}
	})
}

// TestScanFile_Table checks the human view: the source line, the RUNTIME
// column, the "-" for a missing namespace, and the not-classified line.
func TestScanFile_Table(t *testing.T) {
	dir := writeScanFiles(t, map[string]string{
		"gateway.yaml": sandboxedHostNetwork,
		"bare.yaml":    "apiVersion: v1\nkind: Pod\nmetadata: {name: bare}\nspec: {containers: [{name: c, image: busybox}]}\n",
		"svc.yaml":     "apiVersion: v1\nkind: Service\nmetadata: {name: gateway, namespace: edge}\n",
	})
	stdout, _, _, err := runScanCmd(nil, "scan", "-f", dir, "--no-color")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, want := range []string{
		"source: manifests (3 input(s), no cluster)",
		"sandboxed (gvisor): 1",
		"RUNTIME",
		"not classified (no pod template): 1 Service",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the table lacks %q:\n%s", want, stdout)
		}
	}
	var bareRow, gatewayRow string
	for _, line := range strings.Split(stdout, "\n") {
		switch {
		case strings.Contains(line, " bare "):
			bareRow = line
		case strings.Contains(line, " gateway "):
			gatewayRow = line
		}
	}
	// NAMESPACE and RUNTIME are "-" for the bare Pod; the Deployment shows both.
	if fields := strings.Fields(bareRow); len(fields) < 4 || fields[0] != "-" || fields[3] != "-" {
		t.Errorf("row for the bare Pod = %q, want - for namespace and runtime", bareRow)
	}
	if fields := strings.Fields(gatewayRow); len(fields) < 4 || fields[0] != "edge" || fields[3] != "gvisor" {
		t.Errorf("row for the Deployment = %q, want edge and gvisor", gatewayRow)
	}
}
