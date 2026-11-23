// Tests for `agentmoat patch`, driven through the root command as a user
// would. The edit and the selection are tested in pkg/patcher and
// pkg/agentmoat; here the questions are the flags, the dry-run default, and
// the exit code.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// A Deployment that can run in the sandbox and does not ask for it.
const plainWeb = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  template:
    spec:
      containers:
        - name: web
          image: example.test/web:1
`

// The same workload in a form that patch does not edit.
const flowPod = "apiVersion: v1\nkind: Pod\nmetadata: {name: flow, namespace: shop}\nspec: {containers: [{name: c, image: example.test/c:1}]}\n"

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a path under t.TempDir().
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// patchedWeb is plainWeb after the patch: one new line.
var patchedWeb = strings.Replace(plainWeb, "    spec:\n", "    spec:\n      runtimeClassName: gvisor\n", 1)

// patchJSON runs `agentmoat patch ... -o json`, requires the exit code, and
// decodes the result.
func patchJSON(t *testing.T, wantCode int, args ...string) schema.PatchResult {
	t.Helper()
	stdout, stderr, code, err := runScanCmd(nil, append([]string{"patch", "-o", "json"}, args...)...)
	if err != nil || code != wantCode {
		t.Fatalf("patch %v: code %d, err %v; want code %d\n%s", args, code, err, wantCode, stderr)
	}
	var res schema.PatchResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("stdout is not a PatchResult: %v\n%s", err, stdout)
	}
	return res
}

func TestPatchHelp(t *testing.T) {
	stdout, _, _, err := runScanCmd(nil, "patch", "--help")
	if err != nil {
		t.Fatalf("patch --help: %v", err)
	}
	for _, want := range []string{"--filename", "--dry-run", "--include-review", "--runtime-class", "writes no file", "git revert"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the help lacks %q", want)
		}
	}
}

// TestPatch_DryRunIsTheDefault: with no --dry-run flag the command shows the
// diff and writes nothing. This default is the convention of every command
// that changes something.
func TestPatch_DryRunIsTheDefault(t *testing.T) {
	// A kubeconfig that points at a file that is not there: patch reads no
	// cluster, so it must not matter.
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))

	root := writeScanFiles(t, map[string]string{"web.yaml": plainWeb})
	stdout, stderr, code, err := runScanCmd(nil, "patch", "-f", root, "--no-color")
	if err != nil || code != 0 {
		t.Fatalf("patch: code %d, err %v\n%s", code, err, stderr)
	}
	if got := mustRead(t, filepath.Join(root, "web.yaml")); got != plainWeb {
		t.Errorf("the default run changed the file:\n%s", got)
	}
	for _, want := range []string{"dry-run: true", "+      runtimeClassName: gvisor", "Diff (dry-run: no file was written):"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if !strings.Contains(stderr, "run again with --dry-run=false") {
		t.Errorf("stderr does not say how to write:\n%s", stderr)
	}
}

// TestPatch_WritesWithDryRunFalse: --dry-run=false writes the one line, and
// a second run has nothing to do and exits 0.
func TestPatch_WritesWithDryRunFalse(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))

	root := writeScanFiles(t, map[string]string{"web.yaml": plainWeb})
	res := patchJSON(t, 0, "-f", root, "--dry-run=false")
	if res.Kind != schema.KindPatchResult || res.Metadata.DryRun || res.Spec.Summary.Patched != 1 || !res.Spec.Files[0].Written {
		t.Errorf("result = %+v", res)
	}
	if got := mustRead(t, filepath.Join(root, "web.yaml")); got != patchedWeb {
		t.Errorf("the file is\n%s\nwant\n%s", got, patchedWeb)
	}

	_, stderr, code, err := runScanCmd(nil, "patch", "-f", root, "--dry-run=false")
	if err != nil || code != 0 || !strings.Contains(stderr, "nothing to change") {
		t.Errorf("second run: code %d, err %v\n%s", code, err, stderr)
	}
	if got := mustRead(t, filepath.Join(root, "web.yaml")); got != patchedWeb {
		t.Errorf("the second run changed the file:\n%s", got)
	}
}

func TestPatch_ExitCodes(t *testing.T) {
	both := writeScanFiles(t, map[string]string{"web.yaml": plainWeb, "flow.yaml": flowPod})
	if _, _, code, err := runScanCmd(nil, "patch", "-f", both); err != nil || code != 3 {
		t.Errorf("one patched and one not edited: code %d, err %v; want 3", code, err)
	}

	onlyFlow := writeScanFiles(t, map[string]string{"flow.yaml": flowPod})
	stdout, _, code, err := runScanCmd(nil, "patch", "-f", onlyFlow, "--no-color")
	if err != nil || code != 1 {
		t.Errorf("nothing could be patched: code %d, err %v; want 1", code, err)
	}
	if !strings.Contains(stdout, "flow-style YAML") {
		t.Errorf("the report must have the reason:\n%s", stdout)
	}

	nothing := writeScanFiles(t, map[string]string{"gateway.yaml": sandboxedHostNetwork})
	if _, _, code, err := runScanCmd(nil, "patch", "-f", nothing); err != nil || code != 0 {
		t.Errorf("nothing selected: code %d, err %v; want 0", code, err)
	}
}

// TestPatch_RefusesBadInput: each of these is an error (exit 1) before a
// file is read or written, and the message names the cause.
func TestPatch_RefusesBadInput(t *testing.T) {
	root := writeScanFiles(t, map[string]string{"web.yaml": plainWeb})
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"no -f", []string{"patch"}, "-f <file or directory>"},
		{"standard input", []string{"patch", "-f", "-"}, "standard input"},
		{"two namespaces", []string{"patch", "-f", root, "-n", "a", "-n", "b"}, "one namespace"},
		{"a class name that is not a name", []string{"patch", "-f", root, "--runtime-class", "Not A Name", "--dry-run=false"}, "not a valid RuntimeClass name"},
		// A format that patch cannot render fails before a file is read:
		// the message is about the format and not about the missing path.
		{"a format that patch does not have", []string{"patch", "-f", filepath.Join(root, "none"), "-o", "sarif"}, "sarif"},
	}
	for _, tc := range cases {
		_, _, code, err := runScanCmd(nil, tc.args...)
		if err == nil || code != 1 || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: code %d, err %v; want exit 1 with %q", tc.name, code, err, tc.want)
		}
	}
	if _, _, _, err := runScanCmd(nil, "patch", "-f", filepath.Join(root, "none"), "-o", "sarif"); err == nil || strings.Contains(err.Error(), "none") {
		t.Errorf("-o sarif: err = %v; the format error must come before the read of the path", err)
	}
	if got := mustRead(t, filepath.Join(root, "web.yaml")); got != plainWeb {
		t.Errorf("a refused patch changed the file:\n%s", got)
	}
}

// TestPatch_ClusterFlagsAreNotUsed: patch reads no cluster. A cluster flag is
// not an error (a wrapper script can pass --context always); the command
// says that it did not use it.
func TestPatch_ClusterFlagsAreNotUsed(t *testing.T) {
	root := writeScanFiles(t, map[string]string{"web.yaml": plainWeb})
	_, stderr, code, err := runScanCmd(nil, "patch", "-f", root, "--context", "prod", "--kubeconfig", "/nowhere")
	if err != nil || code != 0 || !strings.Contains(stderr, "reads no cluster; not used: --kubeconfig, --context") {
		t.Errorf("cluster flags: code %d, err %v\n%s", code, err, stderr)
	}
}

// TestPatch_Markdown: Markdown is a format of patch, and its first line says
// what happened.
func TestPatch_Markdown(t *testing.T) {
	root := writeScanFiles(t, map[string]string{"web.yaml": plainWeb})
	stdout, _, _, err := runScanCmd(nil, "patch", "-f", root, "-o", "markdown")
	if err != nil || !strings.HasPrefix(stdout, "## agentmoat patch (dry run)\n\n➖ **Dry run.**") {
		t.Errorf("-o markdown: err %v\n%s", err, stdout)
	}
}

// TestPatch_IncludeReviewAndRuntimeClass: the two flags reach the library.
func TestPatch_IncludeReviewAndRuntimeClass(t *testing.T) {
	review := writeScanFiles(t, map[string]string{"logs.yaml": "apiVersion: v1\nkind: Pod\nmetadata:\n  name: logs\nspec:\n  containers:\n    - name: c\n      image: example.test/c:1\n      volumeMounts: [{name: h, mountPath: /h}]\n  volumes:\n    - name: h\n      hostPath: {path: /var/log}\n"})

	without := patchJSON(t, 0, "-f", review)
	if without.Spec.Summary.Selected != 0 || without.Spec.Summary.Excluded != 1 {
		t.Errorf("without --include-review: summary = %+v, want the workload excluded", without.Spec.Summary)
	}

	with := patchJSON(t, 0, "-f", review, "--include-review", "--runtime-class", "sandbox")
	if with.Spec.Summary.Patched != 1 || with.Metadata.RuntimeClassName != "sandbox" || !with.Metadata.IncludeReview {
		t.Errorf("with --include-review: result = %+v", with)
	}
}
