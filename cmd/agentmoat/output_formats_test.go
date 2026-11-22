// Tests for the output formats at the command level: which command takes
// which format, and what `scan` puts in the Markdown and SARIF forms.
//
// None of these tests has a cluster. The commands that need one are given a
// kubeconfig that points at a dead server; a test that reaches it fails
// with a connection error, which is how "the format was refused before any
// work" is checked.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

// kustomizeTree is a base and an overlay that are not rendered. The
// sandboxed gate refuses it.
const kustomizeTree = "../../test/testdata/manifests/kustomize"

// deadCluster sets KUBECONFIG to a file whose server is a closed port.
func deadCluster(t *testing.T) {
	t.Helper()
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
}

// TestCommandsRefuseAFormatBeforeAnyWork: a format that a command cannot
// render is an error before the command reads a file or a cluster.
//
// The case that matters is a command that changes the cluster. If the
// format were checked when the result is rendered, `apply --dry-run=false
// -o sarif` would patch every workload and then print an error where the
// result belongs.
func TestCommandsRefuseAFormatBeforeAnyWork(t *testing.T) {
	deadCluster(t)
	missing := filepath.Join(t.TempDir(), "no-such-file.json")

	tests := []struct {
		name string
		args []string
		// formats the command must refuse. Every other format must get
		// past the format check (and then fail on the missing file or
		// the dead cluster, which is fine: it is a later error).
		refused []string
	}{
		{"plan", []string{"plan", "--scan", missing}, []string{"sarif"}},
		{"apply", []string{"apply", "--plan", missing, "--no-audit"}, []string{"sarif"}},
		{"apply, live", []string{"apply", "--plan", missing, "--no-audit", "--dry-run=false"}, []string{"sarif"}},
		{"rollback", []string{"rollback", "--plan", missing, "--no-audit"}, []string{"sarif"}},
		{"verify", []string{"verify", "--plan", missing}, []string{"sarif"}},
		{"preflight", []string{"preflight"}, []string{"sarif"}},
		{"probe nvproxy", []string{"probe", "nvproxy"}, []string{"sarif"}},
		{"explain", []string{"explain", "gvisor"}, []string{"sarif", "markdown"}},
		{"explain namespace", []string{"explain", "namespace", "default"}, []string{"sarif", "markdown"}},
		{"explain workload", []string{"explain", "workload", "default/web"}, []string{"sarif", "markdown"}},
		// scan takes every format.
		{"scan", []string{"scan", "-f", missing}, nil},
	}
	const formatError = "is not available for"
	for _, tc := range tests {
		for _, format := range []string{"table", "json", "yaml", "markdown", "sarif"} {
			wantRefused := false
			for _, r := range tc.refused {
				if r == format {
					wantRefused = true
				}
			}
			t.Run(tc.name+" -o "+format, func(t *testing.T) {
				args := append(append([]string{}, tc.args...), "-o", format)
				stdout, _, _, err := runScanCmd(nil, args...)
				refused := err != nil && strings.Contains(err.Error(), formatError)
				switch {
				case wantRefused && !refused:
					t.Errorf("want the format refused; got error %v", err)
				case !wantRefused && refused:
					t.Errorf("the format must be accepted; got %v", err)
				}
				if wantRefused && stdout != "" {
					t.Errorf("a refused format must print nothing on stdout; got %q", stdout)
				}
				if wantRefused && err != nil && !strings.Contains(err.Error(), "use one of: table, json, yaml") {
					t.Errorf("the error must list the formats that work: %v", err)
				}
			})
		}
	}
}

func TestScanFile_Markdown(t *testing.T) {
	stdout, stderr, code, err := runScanCmd(nil, "scan", "-f", e2eWorkloads, "-o", "markdown")
	if err != nil {
		t.Fatalf("scan -o markdown: %v\nstderr: %s", err, stderr)
	}
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	// The gate is the first thing under the title, and it is in stdout:
	// a job summary gets stdout only.
	want := "## agentmoat scan\n\n❌ **Gate failed** (`--fail-on incompatible`). 3 of 14 workload(s) cannot run in gVisor.\n"
	if !strings.HasPrefix(stdout, want) {
		t.Errorf("stdout must start with\n%s\ngot\n%s", want, stdout[:min(len(stdout), 300)])
	}
	for _, want := range []string{"| 14 | 8 | 3 | 3 | 0 |", "### Incompatible (3)", "### Review (3)", "<summary>Compatible (8)</summary>"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q", want)
		}
	}
	if strings.Contains(stdout, "\x1b[") {
		t.Errorf("Markdown must have no ANSI escape codes")
	}

	// "md" is the same format.
	short, _, _, err := runScanCmd(nil, "scan", "-f", e2eWorkloads, "-o", "md")
	if err != nil || short != stdout {
		t.Errorf("-o md must give the same output as -o markdown (err %v)", err)
	}
}

// TestScanFile_MarkdownGateOutcomes: each gate outcome is in the report.
// The one that needs care is the refusal: the command returns an error
// (exit 1), and the report it printed before that must say that the gate
// could not answer. A report with no gate line reads as a pass.
func TestScanFile_MarkdownGateOutcomes(t *testing.T) {
	dir := writeScanFiles(t, map[string]string{"gateway.yaml": sandboxedHostNetwork})
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantLine string
	}{
		{"sandboxed, failed", []string{"scan", "-f", dir, "--fail-on", "sandboxed"}, 2,
			"❌ **Gate failed** (`--fail-on sandboxed`). 1 workload(s) ask for RuntimeClass \"gvisor\" and cannot run in it."},
		{"sandboxed, nothing asks for the class", []string{"scan", "-f", e2eWorkloads, "--fail-on", "sandboxed"}, 0,
			"⚠️ **Gate checked nothing** (`--fail-on sandboxed`). No workload asks for RuntimeClass \"gvisor\""},
		{"none", []string{"scan", "-f", dir, "--fail-on", "none"}, 0,
			"➖ **No gate** (`--fail-on none`). This report does not pass or fail."},
		{"sandboxed, refused", []string{"scan", "-f", kustomizeTree, "--fail-on", "sandboxed"}, 1,
			"❌ **Gate could not answer** (`--fail-on sandboxed`). The input is not rendered"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stdout, _, code, _ := runScanCmd(nil, append(tc.args, "-o", "markdown")...)
			if code != tc.wantCode {
				t.Errorf("exit code = %d, want %d", code, tc.wantCode)
			}
			lines := strings.Split(stdout, "\n")
			if len(lines) < 3 || !strings.HasPrefix(lines[2], tc.wantLine) {
				t.Errorf("the gate line must start with\n%s\ngot\n%s", tc.wantLine, stdout[:min(len(stdout), 400)])
			}
			if strings.Contains(stdout, "Gate passed") {
				t.Errorf("this report must not say \"Gate passed\"")
			}
		})
	}

	// And the pass.
	clean := writeScanFiles(t, map[string]string{"agent.yaml": strings.Replace(sandboxedHostNetwork, "      hostNetwork: true\n", "", 1)})
	stdout, _, code, err := runScanCmd(nil, "scan", "-f", clean, "--fail-on", "sandboxed", "-o", "markdown")
	if err != nil || code != 0 || !strings.Contains(stdout, "✅ **Gate passed** (`--fail-on sandboxed`). 1 workload(s) ask for RuntimeClass \"gvisor\" and none is incompatible.") {
		t.Errorf("a passing gate must say so (code %d, err %v):\n%s", code, err, stdout[:min(len(stdout), 300)])
	}
}

// sarifDoc is the part of a SARIF document these tests read.
type sarifDoc struct {
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Rules []struct {
					ID string `json:"id"`
				} `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID    string `json:"ruleId"`
			Level     string `json:"level"`
			Locations []struct {
				PhysicalLocation *struct {
					ArtifactLocation struct {
						URI string `json:"uri"`
					} `json:"artifactLocation"`
					Region *struct {
						StartLine int `json:"startLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
			} `json:"locations"`
			Properties struct {
				Name string `json:"name"`
			} `json:"properties"`
		} `json:"results"`
	} `json:"runs"`
}

// A privileged DaemonSet with no runtimeClassName: the workload that never
// moves to gVisor, and that a pull-request check must not fail on.
const cniDaemonSet = `apiVersion: apps/v1
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

// TestScanFile_SARIFLevelsFollowTheGate: the same files, two gates. Under
// the default gate both workloads fail, so both have "error" results. Under
// --fail-on sandboxed only the sandboxed one fails; the DaemonSet's results
// are "warning", so a code scanning check does not fail where the gate
// passed.
func TestScanFile_SARIFLevelsFollowTheGate(t *testing.T) {
	dir := writeScanFiles(t, map[string]string{"gateway.yaml": sandboxedHostNetwork, "cni.yaml": cniDaemonSet})

	tests := []struct {
		name string
		args []string
		want map[string]string // "workload/rule" -> level
	}{
		{
			name: "the default gate: both workloads fail it",
			want: map[string]string{"gateway/host-network": "error", "cni/host-network": "error", "cni/privileged": "error"},
		},
		{
			name: "--fail-on sandboxed: only the sandboxed workload fails it",
			args: []string{"--fail-on", "sandboxed"},
			want: map[string]string{"gateway/host-network": "error", "cni/host-network": "warning", "cni/privileged": "warning"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := scanSARIF(t, 2, append([]string{"scan", "-f", dir}, tc.args...)...)
			got := map[string]string{}
			for _, r := range doc.Runs[0].Results {
				got[r.Properties.Name+"/"+r.RuleID] = r.Level
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("levels = %v\nwant     %v", got, tc.want)
			}
		})
	}
}

// scanSARIF runs a scan of files with -o sarif, checks the exit code, and
// returns the decoded document. A scan of files gives every result a file,
// so the "no file location" note on stderr is a failure here.
func scanSARIF(t *testing.T, wantCode int, args ...string) sarifDoc {
	t.Helper()
	stdout, stderr, code, err := runScanCmd(nil, append(args, "-o", "sarif")...)
	if err != nil || code != wantCode {
		t.Fatalf("%v -o sarif: code %d (want %d), err %v\nstderr: %s", args, code, wantCode, err, stderr)
	}
	if strings.Contains(stderr, "no file location") {
		t.Errorf("a scan of files must not print the no-file note:\n%s", stderr)
	}
	var doc sarifDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}
	if doc.Version != "2.1.0" || len(doc.Runs) != 1 {
		t.Fatalf("version = %q, runs = %d; want 2.1.0 and one run", doc.Version, len(doc.Runs))
	}
	return doc
}

// TestScanFile_SARIFLocations: each result from a file has that file and the
// first line of the object.
func TestScanFile_SARIFLocations(t *testing.T) {
	dir := writeScanFiles(t, map[string]string{"gateway.yaml": sandboxedHostNetwork, "cni.yaml": cniDaemonSet})
	doc := scanSARIF(t, 2, "scan", "-f", dir)
	if len(doc.Runs[0].Results) != 3 {
		t.Fatalf("results = %d, want 3", len(doc.Runs[0].Results))
	}
	for _, r := range doc.Runs[0].Results {
		if len(r.Locations) != 1 || r.Locations[0].PhysicalLocation == nil {
			t.Fatalf("%s: no file location", r.RuleID)
		}
		phys := r.Locations[0].PhysicalLocation
		wantFile := r.Properties.Name + ".yaml"
		if !strings.HasSuffix(phys.ArtifactLocation.URI, "/"+wantFile) || phys.Region == nil || phys.Region.StartLine != 1 {
			t.Errorf("%s: location = %+v, want %s and line 1", r.RuleID, phys, wantFile)
		}
	}
}

// TestScanFile_SARIFFromStdinSaysThereIsNoFile: results from standard input
// have no file. GitHub does not show such a result, and says nothing. The
// command must say it.
func TestScanFile_SARIFFromStdinSaysThereIsNoFile(t *testing.T) {
	stdout, stderr, _, err := runScanCmd(strings.NewReader(sandboxedHostNetwork), "scan", "-f", "-", "-o", "sarif")
	if err != nil {
		t.Fatalf("scan -f - -o sarif: %v", err)
	}
	if !strings.Contains(stderr, "note: 1 of 1 SARIF result(s) have no file location") {
		t.Errorf("stderr lacks the no-file note:\n%s", stderr)
	}
	var doc sarifDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	if res := doc.Runs[0].Results; len(res) != 1 || len(res[0].Locations) != 1 || res[0].Locations[0].PhysicalLocation != nil {
		t.Errorf("the result must be there, with no file location: %+v", res)
	}
}

// TestScanFile_SARIFWithNoFindings: the passing run. Both arrays are there
// and empty.
func TestScanFile_SARIFWithNoFindings(t *testing.T) {
	clean := writeScanFiles(t, map[string]string{"agent.yaml": strings.Replace(sandboxedHostNetwork, "      hostNetwork: true\n", "", 1)})
	stdout, _, code, err := runScanCmd(nil, "scan", "-f", clean, "-o", "sarif")
	if err != nil || code != 0 {
		t.Fatalf("scan -o sarif: code %d, err %v", code, err)
	}
	for _, want := range []string{`"results": []`, `"rules": []`} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %s:\n%s", want, stdout)
		}
	}
}

// TestPlanMarkdown_FromAManifestScan: plan -o markdown, with no cluster.
// The preview warning must be in the summary an approver reads.
func TestPlanMarkdown_FromAManifestScan(t *testing.T) {
	scan, _, _, err := runScanCmd(nil, "scan", "-f", e2eWorkloads, "-n", "agentmoat-e2e", "--fail-on", "none", "-o", "json")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	scanPath := filepath.Join(t.TempDir(), "scan.json")
	if err := os.WriteFile(scanPath, []byte(scan), 0o600); err != nil {
		t.Fatalf("write scan: %v", err)
	}

	stdout, stderr, code, err := runScanCmd(nil, "plan", "--scan", scanPath, "-o", "markdown")
	if err != nil || code != 0 {
		t.Fatalf("plan -o markdown: code %d, err %v\nstderr: %s", code, err, stderr)
	}
	for _, want := range []string{"## agentmoat plan\n", "Plan hash: `", "- ⚠️ `manifest-source`: this plan was made from a scan of manifests", "### Steps (8)"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout[:min(len(stdout), 600)])
		}
	}
}

// TestOutputGate: the two packages each have the outcome constants, because
// neither imports the other. The texts must be the same, or the renderer
// would show a known outcome as "unknown".
func TestOutputGate(t *testing.T) {
	pairs := map[agentmoat.GateOutcome]output.GateOutcome{
		agentmoat.GateOff:            output.GateOff,
		agentmoat.GatePassed:         output.GatePassed,
		agentmoat.GateFailed:         output.GateFailed,
		agentmoat.GateNothingChecked: output.GateNothingChecked,
		agentmoat.GateRefused:        output.GateRefused,
	}
	for from, want := range pairs {
		got := outputGate(agentmoat.GateResult{
			FailOn: agentmoat.FailOnSandboxed, Outcome: from, Summary: "s", Details: []string{"d"},
		})
		if got.Outcome != want || string(got.Outcome) != string(from) {
			t.Errorf("outcome %q became %q, want %q", from, got.Outcome, want)
		}
		if got.FailOn != "sandboxed" || got.Summary != "s" || len(got.Details) != 1 {
			t.Errorf("fields were not copied: %+v", got)
		}
	}
}
