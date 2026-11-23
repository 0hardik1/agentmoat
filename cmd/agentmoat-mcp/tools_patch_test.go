// Unit tests for the patch_manifests tool. The edit and the selection are
// tested in pkg/patcher and pkg/agentmoat; here the questions are the safety
// gate (no write without "dry_run": false), the mapping of the inputs, and
// that the tool never calls the Kubernetes API.
package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// A Deployment that can run in the sandbox and does not ask for it.
const patchWeb = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  labels: {tier: front}
spec:
  template:
    spec:
      containers:
        - name: web
          image: example.test/web:1
`

// A Pod that needs review (a hostPath volume).
const patchLogs = `apiVersion: v1
kind: Pod
metadata:
  name: logs
spec:
  containers:
    - name: logs
      image: example.test/logs:1
      volumeMounts:
        - {name: host, mountPath: /host}
  volumes:
    - name: host
      hostPath: {path: /var/log}
`

// writePatchFiles writes the two manifests into a new directory.
func writePatchFiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range map[string]string{"web.yaml": patchWeb, "logs.yaml": patchLogs} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readPatchFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name)) // #nosec G304 -- a path under t.TempDir().
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// noAPIClient returns a client that fails the test on any call.
func noAPIClient(t *testing.T) *fake.Clientset {
	t.Helper()
	client := fake.NewSimpleClientset()
	client.PrependReactor("*", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		t.Errorf("patch_manifests called the Kubernetes API: %s %s", action.GetVerb(), action.GetResource().Resource)
		return true, nil, errors.New("no API calls in a patch")
	})
	return client
}

// patchedPatchWeb is patchWeb after the patch: one new line.
var patchedPatchWeb = strings.Replace(patchWeb, "    spec:\n", "    spec:\n      runtimeClassName: gvisor\n", 1)

// callPatch calls patch_manifests and decodes the result. The client of the
// server fails the test on any call, and the helper also checks that no call
// was recorded.
func callPatch(t *testing.T, args map[string]any) schema.PatchResult {
	t.Helper()
	client := noAPIClient(t)
	result, err := callTool(t, newTestServer(client), "patch_manifests", args)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var res schema.PatchResult
	readTool(t, result, &res)
	if n := len(client.Actions()); n != 0 {
		t.Errorf("%d API actions, want 0", n)
	}
	return res
}

// TestPatchManifestsHandler_DryRunIsTheDefault: without "dry_run": false the
// tool writes nothing, for an absent field and for true. The result has the
// diff.
func TestPatchManifestsHandler_DryRunIsTheDefault(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		"dry_run is absent": {},
		"dry_run is true":   {"dry_run": true},
	} {
		dir := writePatchFiles(t)
		args["manifest_paths"] = []any{dir}
		res := callPatch(t, args)
		if res.Kind != schema.KindPatchResult || !res.Metadata.DryRun {
			t.Errorf("%s: kind %q, dryRun %v; want a dry-run PatchResult", name, res.Kind, res.Metadata.DryRun)
		}
		if got, want := res.Spec.Summary, (schema.PatchSummary{Total: 2, Selected: 1, Patched: 1, Excluded: 1, Files: 1}); got != want {
			t.Errorf("%s: summary = %+v, want %+v", name, got, want)
		}
		if len(res.Spec.Files) != 1 || res.Spec.Files[0].Written || !strings.Contains(res.Spec.Files[0].Diff, "+      runtimeClassName: gvisor\n") {
			t.Errorf("%s: files = %+v, want one diff that is not written", name, res.Spec.Files)
		}
		if got := readPatchFile(t, dir, "web.yaml"); got != patchWeb {
			t.Errorf("%s: the file changed:\n%s", name, got)
		}
	}
}

// TestPatchManifestsHandler_WritesWithDryRunFalse: with "dry_run": false the
// file has the one new line, the file of a workload that was not selected is
// as it was, and a second call has nothing to do.
func TestPatchManifestsHandler_WritesWithDryRunFalse(t *testing.T) {
	t.Parallel()
	dir := writePatchFiles(t)
	args := map[string]any{"manifest_paths": []any{dir}, "dry_run": false}

	res := callPatch(t, args)
	if res.Metadata.DryRun || !res.Spec.Files[0].Written {
		t.Errorf("result = %+v, want a written file", res.Spec.Files)
	}
	if got := readPatchFile(t, dir, "web.yaml"); got != patchedPatchWeb {
		t.Errorf("the file is\n%s\nwant\n%s", got, patchedPatchWeb)
	}
	if got := readPatchFile(t, dir, "logs.yaml"); got != patchLogs {
		t.Errorf("the file of a workload that was not selected changed")
	}

	again := callPatch(t, args)
	if again.Spec.Summary.Selected != 0 || len(again.Spec.Files) != 0 {
		t.Errorf("second call: summary = %+v, want nothing selected", again.Spec.Summary)
	}
	if got := readPatchFile(t, dir, "web.yaml"); got != patchedPatchWeb {
		t.Errorf("the second call changed the file")
	}
}

// TestPatchManifestsHandler_Inputs: each input reaches the library.
func TestPatchManifestsHandler_Inputs(t *testing.T) {
	t.Parallel()
	dir := writePatchFiles(t)
	res := callPatch(t, map[string]any{
		"manifest_paths":     []any{dir},
		"include_review":     true,
		"runtime_class_name": "sandbox",
		"namespace":          "team-a",
	})
	if res.Spec.Summary.Selected != 2 || !res.Metadata.IncludeReview || res.Metadata.RuntimeClassName != "sandbox" {
		t.Fatalf("metadata = %+v, summary = %+v", res.Metadata, res.Spec.Summary)
	}
	for _, c := range res.Spec.Changes {
		if c.Target.Namespace != "team-a" {
			t.Errorf("%s: namespace = %q, want the default", c.Target.Name, c.Target.Namespace)
		}
	}
	for _, f := range res.Spec.Files {
		if !strings.Contains(f.Diff, "runtimeClassName: sandbox") {
			t.Errorf("the diff of %s does not have the class name:\n%s", f.Path, f.Diff)
		}
	}

	selected := callPatch(t, map[string]any{
		"manifest_paths": []any{dir},
		"label_selector": "tier=front",
	})
	if selected.Spec.Summary.Total != 1 || selected.Spec.Changes[0].Target.Name != "web" {
		t.Errorf("with a selector: %+v, want web only", selected.Spec)
	}
}

// TestPatchManifestsHandler_Errors: bad input is a tool error, and nothing
// is written.
func TestPatchManifestsHandler_Errors(t *testing.T) {
	t.Parallel()
	dir := writePatchFiles(t)
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no paths", map[string]any{}, "manifest_paths"},
		{"an empty list", map[string]any{"manifest_paths": []any{}}, "no manifest paths"},
		{"standard input", map[string]any{"manifest_paths": []any{"-"}, "dry_run": false}, "standard input"},
		// With a component directory the CLI reads standard input. This
		// server cannot: its standard input is the protocol channel. The
		// call must be an error and must not wait for input.
		{"standard input with a component", map[string]any{"manifest_paths": []any{"-"}, "kustomize_dir": filepath.Join(dir, "g"), "dry_run": false}, "standard input"},
		{"a component in a directory of manifests", map[string]any{"manifest_paths": []any{dir}, "kustomize_dir": dir, "dry_run": false}, "has files, and no kustomization.yaml that agentmoat wrote"},
		{"a path that is not there", map[string]any{"manifest_paths": []any{"/no/such/manifests"}}, "/no/such/manifests"},
		// A value that is not a boolean must not become a write.
		{"dry_run is a string", map[string]any{"manifest_paths": []any{dir}, "dry_run": "false"}, "dry_run must be a boolean"},
		{"a class name that is not a name", map[string]any{"manifest_paths": []any{dir}, "dry_run": false, "runtime_class_name": "a\nb"}, "not a valid RuntimeClass name"},
	}
	for _, tc := range tests {
		result, err := callTool(t, newTestServer(noAPIClient(t)), "patch_manifests", tc.args)
		if err != nil {
			t.Fatalf("%s: unexpected go-level error: %v", tc.name, err)
		}
		expectToolError(t, result, tc.want)
	}
	if got := readPatchFile(t, dir, "web.yaml"); got != patchWeb {
		t.Errorf("a refused call changed the file:\n%s", got)
	}
}

// wantKustomizeDryRun checks the result of a dry-run with kustomize_dir: one
// workload with its patch file, and two new files that are not written (the
// patch file and the list).
func wantKustomizeDryRun(t *testing.T, dry schema.PatchResult, component, patchFile string) {
	t.Helper()
	if !dry.Metadata.DryRun || dry.Metadata.KustomizeDir != component {
		t.Errorf("metadata = %+v", dry.Metadata)
	}
	if got, want := dry.Spec.Summary, (schema.PatchSummary{Total: 2, Selected: 1, Patched: 1, Excluded: 1, Files: 2}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if len(dry.Spec.Changes) != 1 || dry.Spec.Changes[0].PatchFile != patchFile {
		t.Errorf("changes = %+v", dry.Spec.Changes)
	}
	for _, f := range dry.Spec.Files {
		if f.Written || !f.Created || !strings.HasPrefix(f.Diff, "--- /dev/null\n") {
			t.Errorf("the file %s = %+v; want a new file that is not written", f.Path, f)
		}
	}
}

// TestPatchManifestsHandler_Kustomize: with kustomize_dir the tool edits no
// source file. It has the same safety gate: no write without "dry_run":
// false.
func TestPatchManifestsHandler_Kustomize(t *testing.T) {
	t.Parallel()
	dir := writePatchFiles(t)
	component := filepath.Join(t.TempDir(), "overlay", "gvisor")
	patchFile := filepath.Join(component, "deployment.apps_web.yaml")

	dry := callPatch(t, map[string]any{"manifest_paths": []any{dir}, "kustomize_dir": component})
	wantKustomizeDryRun(t, dry, component, patchFile)
	if _, err := os.Stat(component); !os.IsNotExist(err) {
		t.Errorf("a dry-run made the directory: %v", err)
	}

	callPatch(t, map[string]any{"manifest_paths": []any{dir}, "kustomize_dir": component, "dry_run": false})
	data, err := os.ReadFile(patchFile) // #nosec G304 -- a path under t.TempDir().
	if err != nil || !strings.HasSuffix(string(data), "kind: Deployment\nmetadata:\n  name: web\nspec:\n  template:\n    spec:\n      runtimeClassName: gvisor\n") {
		t.Errorf("the patch file: %v\n%s", err, data)
	}
	if _, err := os.Stat(filepath.Join(component, "kustomization.yaml")); err != nil {
		t.Errorf("the list: %v", err)
	}
	if got := readPatchFile(t, dir, "web.yaml"); got != patchWeb {
		t.Errorf("a source file changed:\n%s", got)
	}

	again := callPatch(t, map[string]any{"manifest_paths": []any{dir}, "kustomize_dir": component, "dry_run": false})
	if again.Spec.Summary.Selected != 0 || len(again.Spec.Files) != 0 {
		t.Errorf("second call: summary = %+v, want nothing selected", again.Spec.Summary)
	}
}
