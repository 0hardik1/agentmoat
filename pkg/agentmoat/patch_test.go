// Tests for Patch, the orchestrator of `agentmoat patch`. The edit itself
// is tested in pkg/patcher; here the questions are which workloads are
// selected, what a dry-run does, what gets written and how, and that no
// cluster is read.
package agentmoat

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/planner"
)

// patchFiles runs Patch with the progress output captured.
func patchFiles(t *testing.T, opts PatchOptions) (*schema.PatchResult, string) {
	t.Helper()
	var stderr bytes.Buffer
	opts.Stderr = &stderr
	res, err := Patch(context.Background(), opts)
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	return res, stderr.String()
}

// readFile returns the content of a file of a test.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) // #nosec G304 -- a path under t.TempDir().
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// wantFile checks the content of a file.
func wantFile(t *testing.T, path, want, what string) {
	t.Helper()
	if got := readFile(t, path); got != want {
		t.Errorf("%s is\n%s\nwant\n%s", what, got, want)
	}
}

// wantEntries checks the number of entries of a directory. A temporary file
// that a write left behind would be one more.
func wantEntries(t *testing.T, dir string, want int) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != want {
		t.Errorf("the directory has %d entries, want %d (the files, and no temporary file)", len(entries), want)
	}
}

// wantMode checks the permission bits of a file.
func wantMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("the file mode is %v, want %v as before", info.Mode().Perm(), want)
	}
}

// excludedReasons returns the reason of each excluded workload, by name.
func excludedReasons(res *schema.PatchResult) map[string]string {
	reasons := map[string]string{}
	for _, e := range res.Spec.Excluded {
		reasons[e.Target.Name] = e.Reason
	}
	return reasons
}

// changeOf returns the change for the workload with the name.
func changeOf(t *testing.T, res *schema.PatchResult, name string) schema.PatchChange {
	t.Helper()
	for _, c := range res.Spec.Changes {
		if c.Target.Name == name {
			return c
		}
	}
	t.Fatalf("the result has no change for %q: %+v", name, res.Spec.Changes)
	return schema.PatchChange{}
}

// patchedDeployment is plainDeployment after the patch: one new line.
var patchedDeployment = strings.Replace(plainDeployment, "    spec:\n", "    spec:\n      runtimeClassName: gvisor\n", 1)

// reviewPod is a workload that needs review (a hostPath volume).
const reviewPod = `apiVersion: v1
kind: Pod
metadata:
  name: logs
  namespace: agents
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

// otherClass is a compatible workload that has another RuntimeClass.
const otherClass = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: vm
  namespace: agents
spec:
  template:
    spec:
      runtimeClassName: kata
      containers:
        - name: vm
          image: example.test/vm:1
`

// patchTree is one file for each case of the selection.
func patchTree(t *testing.T) string {
	t.Helper()
	return writeManifests(t, map[string]string{
		"web.yaml":     plainDeployment,     // compatible, no class: selected
		"agent.yaml":   sandboxedClean,      // has the class already
		"gateway.yaml": sandboxedHostNet,    // incompatible
		"cni.yaml":     privilegedDaemonSet, // incompatible
		"logs.yaml":    reviewPod,           // needs review
		"vm.yaml":      otherClass,          // another class
		"svc.yaml":     plainService,        // not a workload
	})
}

func TestPatch_SelectsWhatAPlanSelects(t *testing.T) {
	t.Parallel()
	root := patchTree(t)
	res, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: true})

	if got, want := res.Spec.Summary, (schema.PatchSummary{Total: 6, Selected: 1, Patched: 1, Excluded: 5, Files: 1}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if len(res.Spec.Changes) != 1 || res.Spec.Changes[0].Target.Name != "web" || res.Spec.Changes[0].Status != schema.PatchStatusPatched {
		t.Fatalf("changes = %+v, want web patched", res.Spec.Changes)
	}
	reasons := excludedReasons(res)
	for name, want := range map[string]string{
		"agent":   `already has RuntimeClass "gvisor": nothing to change`,
		"gateway": "workload is incompatible: host-network",
		"cni":     "workload is incompatible:",
		"logs":    "workload needs review (use --include-review to include it):",
		"vm":      `has RuntimeClass "kata": patch does not replace another RuntimeClass`,
	} {
		if !strings.Contains(reasons[name], want) {
			t.Errorf("reason for %s = %q, want it to have %q", name, reasons[name], want)
		}
	}
	if !strings.Contains(stderr, "patch: 6 workloads in 7 manifest input(s): 1 selected, 5 excluded") {
		t.Errorf("stderr lacks the count line:\n%s", stderr)
	}

	// The same input as a scan and a plan: the plan selects the same
	// workload. (A plan from manifests is a preview, and that is enough
	// here: the question is the selection.)
	report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{root}})
	plan, err := planner.Plan(report, planner.Options{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Spec.Steps) != 1 || plan.Spec.Steps[0].Target.Name != "web" {
		t.Errorf("the plan has steps %+v; patch selected web only", plan.Spec.Steps)
	}
}

// TestPatch_APodAndAJobInAFileAreEdited: a plan has no step for a Pod or a
// Job (the API refuses the live change). A file of one can be edited, so
// patch selects it.
func TestPatch_APodAndAJobInAFileAreEdited(t *testing.T) {
	t.Parallel()
	const pod = "apiVersion: v1\nkind: Pod\nmetadata:\n  name: once\n  namespace: agents\nspec:\n  containers:\n    - {name: c, image: example.test/c:1}\n"
	const job = "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: migrate\n  namespace: agents\nspec:\n  template:\n    spec:\n      restartPolicy: Never\n      containers:\n        - {name: c, image: example.test/c:1}\n"
	root := writeManifests(t, map[string]string{"pod.yaml": pod, "job.yaml": job})
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: false})
	if res.Spec.Summary.Patched != 2 || res.Spec.Summary.Excluded != 0 {
		t.Fatalf("summary = %+v, want 2 patched", res.Spec.Summary)
	}

	report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{root}})
	if report.Spec.Summary.Sandboxed != 2 {
		t.Errorf("after the patch, the scan finds %d sandboxed workloads, want 2", report.Spec.Summary.Sandboxed)
	}
	plan, err := planner.Plan(report, planner.Options{})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Spec.Steps) != 0 {
		t.Errorf("plan steps = %+v, want none", plan.Spec.Steps)
	}
}

func TestPatch_DryRunWritesNothing(t *testing.T) {
	t.Parallel()
	root := patchTree(t)
	before := readFile(t, filepath.Join(root, "web.yaml"))
	res, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: true})

	if !res.Metadata.DryRun || len(res.Spec.Files) != 1 || res.Spec.Files[0].Written {
		t.Fatalf("metadata = %+v, files = %+v; want a dry-run with one file that is not written", res.Metadata, res.Spec.Files)
	}
	wantFile(t, filepath.Join(root, "web.yaml"), before, "the file after a dry-run")
	file := res.Spec.Files[0]
	if file.Added != 1 || !strings.Contains(file.Diff, "+      runtimeClassName: gvisor\n") || !strings.HasPrefix(file.Diff, "--- ") {
		t.Errorf("file = %+v, want one added line in a unified diff", file)
	}
	if !strings.Contains(stderr, "dry-run: 1 line(s) would be added to 1 file(s)") || !strings.Contains(stderr, "--dry-run=false") {
		t.Errorf("stderr does not say that this was a dry-run and how to write:\n%s", stderr)
	}
	wantEntries(t, root, 7)
}

// TestPatch_WritesOneLine is the oracle of the command: the file is the old
// file plus one line, the other files are as they were, and a scan of the
// new file finds the class.
func TestPatch_WritesOneLine(t *testing.T) {
	t.Parallel()
	root := patchTree(t)
	path := filepath.Join(root, "web.yaml")
	if err := os.Chmod(path, 0o640); err != nil { // #nosec G302 -- the test checks that the mode stays.
		t.Fatal(err)
	}
	others := map[string]string{}
	for _, name := range []string{"agent.yaml", "gateway.yaml", "cni.yaml", "logs.yaml", "vm.yaml", "svc.yaml"} {
		others[name] = readFile(t, filepath.Join(root, name))
	}

	res, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: false})
	if res.Metadata.DryRun || !res.Spec.Files[0].Written || res.Spec.Summary.Patched != 1 {
		t.Fatalf("result = %+v, want one file written", res.Spec)
	}
	wantFile(t, path, patchedDeployment, "the patched file")
	if res.Spec.Changes[0].Line != 9 {
		t.Errorf("the added line is reported on line %d, want 9", res.Spec.Changes[0].Line)
	}
	if !strings.Contains(stderr, "patch: 1 of 1 file(s) written") {
		t.Errorf("stderr:\n%s", stderr)
	}
	for name, text := range others {
		wantFile(t, filepath.Join(root, name), text, name+" (no workload in it was selected)")
	}
	wantMode(t, path, 0o640)
	wantEntries(t, root, 7)

	report, _ := scanFiles(t, ScanOptions{ManifestPaths: []string{path}})
	if w := report.Spec.Workloads[0]; w.RuntimeClassName != "gvisor" || w.Compatibility != schema.CompatibilityCompatible {
		t.Errorf("the scan of the new file = %+v, want compatible with the class", w)
	}
}

// TestPatch_ASecondRunChangesNothing: after a patch, the workloads have the
// class, so the same command selects nothing, writes nothing, and exits 0.
func TestPatch_ASecondRunChangesNothing(t *testing.T) {
	t.Parallel()
	root := patchTree(t)
	patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: false})

	again, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: false})
	if again.Spec.Summary.Selected != 0 || len(again.Spec.Files) != 0 || PatchExitCode(again) != 0 {
		t.Errorf("the second run = %+v, want nothing selected and exit 0", again.Spec.Summary)
	}
	if !strings.Contains(stderr, "nothing to change") {
		t.Errorf("stderr of the second run:\n%s", stderr)
	}
	wantFile(t, filepath.Join(root, "web.yaml"), patchedDeployment, "the file after the second run")
}

// TestPatch_IncludeReview: with IncludeReview the workload that needs review
// is selected too, and the result keeps its verdict.
func TestPatch_IncludeReview(t *testing.T) {
	t.Parallel()
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{patchTree(t)}, DryRun: true, IncludeReview: true})
	if res.Spec.Summary.Selected != 2 || !res.Metadata.IncludeReview {
		t.Fatalf("summary = %+v, want 2 selected", res.Spec.Summary)
	}
	if logs := changeOf(t, res, "logs"); logs.Compatibility != schema.CompatibilityReview || logs.Status != schema.PatchStatusPatched {
		t.Errorf("the change for logs = %+v, want patched with the verdict review", logs)
	}
}

// TestPatch_RuntimeClassName: another class name is written, and the
// default is gvisor.
func TestPatch_RuntimeClassName(t *testing.T) {
	t.Parallel()
	root := writeManifests(t, map[string]string{"web.yaml": plainDeployment})
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, RuntimeClassName: "sandbox-v2"})
	if res.Metadata.RuntimeClassName != "sandbox-v2" {
		t.Errorf("class = %q", res.Metadata.RuntimeClassName)
	}
	if !strings.Contains(readFile(t, filepath.Join(root, "web.yaml")), "      runtimeClassName: sandbox-v2\n") {
		t.Errorf("the file does not have the class")
	}
	def, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{patchTree(t)}, DryRun: true})
	if def.Metadata.RuntimeClassName != "gvisor" {
		t.Errorf("default class = %q, want gvisor", def.Metadata.RuntimeClassName)
	}
}

// TestPatch_AWorkloadWithAClassIsExcluded: with the class name kata, the
// Deployment that has kata has nothing to change, and the ones that have
// gvisor have another class. Neither is edited.
func TestPatch_AWorkloadWithAClassIsExcluded(t *testing.T) {
	t.Parallel()
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{patchTree(t)}, DryRun: true, RuntimeClassName: "kata"})
	reasons := excludedReasons(res)
	if !strings.Contains(reasons["vm"], `already has RuntimeClass "kata"`) || !strings.Contains(reasons["agent"], `has RuntimeClass "gvisor": patch does not replace`) {
		t.Errorf("reasons = %v", reasons)
	}
}

// TestPatch_LabelSelector: a label selector limits the patch.
func TestPatch_LabelSelector(t *testing.T) {
	t.Parallel()
	labeled := strings.Replace(plainDeployment, "  name: web\n", "  name: web\n  labels: {tier: front}\n", 1)
	other := strings.Replace(plainDeployment, "name: web", "name: api", 1)
	root := writeManifests(t, map[string]string{"web.yaml": labeled, "api.yaml": other})
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: true, LabelSelector: "tier=front"})
	if res.Spec.Summary.Total != 1 || res.Spec.Changes[0].Target.Name != "web" {
		t.Errorf("result = %+v, want web only", res.Spec)
	}
}

// TestPatch_NamespaceIsADefault: the namespace option fills in the namespace
// of the report for an object that names none. It is not written to a file.
func TestPatch_NamespaceIsADefault(t *testing.T) {
	t.Parallel()
	bare := strings.Replace(plainDeployment, "  namespace: agents\n", "", 1)
	root := writeManifests(t, map[string]string{"web.yaml": bare})
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, Namespace: "team-a"})
	if res.Spec.Changes[0].Target.Namespace != "team-a" {
		t.Errorf("namespace = %q, want the default", res.Spec.Changes[0].Target.Namespace)
	}
	if got := readFile(t, filepath.Join(root, "web.yaml")); strings.Contains(got, "team-a") {
		t.Errorf("the default namespace was written to the file:\n%s", got)
	}
}

// The forms that patch does not edit, as test input.
const flowPod = "apiVersion: v1\nkind: Pod\nmetadata: {name: flow, namespace: agents}\nspec: {containers: [{name: c, image: example.test/c:1}]}\n"

func jsonPod(name string) string {
	return `{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "` + name + `", "namespace": "agents"}, "spec": {"containers": [{"name": "c", "image": "example.test/c:1"}]}}` + "\n"
}

// TestPatch_NotEdited: a selected workload in a form that is not edited is
// listed with the reason, its file stays as it is, and the exit code says
// that the patch is partial.
func TestPatch_NotEdited(t *testing.T) {
	t.Parallel()
	mixed := writeManifests(t, map[string]string{"web.yaml": plainDeployment, "flow.yaml": flowPod, "pod.json": jsonPod("json")})
	res, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{mixed}, DryRun: false})
	if got, want := res.Spec.Summary, (schema.PatchSummary{Total: 3, Selected: 3, Patched: 1, NotEdited: 2, Files: 1}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if code := PatchExitCode(res); code != 3 {
		t.Errorf("exit code = %d, want 3 (partial)", code)
	}
	for name, reason := range map[string]string{"flow": "flow-style YAML", "json": "JSON is not edited"} {
		if c := changeOf(t, res, name); c.Status != schema.PatchStatusNotEdited || !strings.Contains(c.Reason, reason) {
			t.Errorf("%s = %+v, want not-edited with the reason %q", name, c, reason)
		}
	}
	wantFile(t, filepath.Join(mixed, "flow.yaml"), flowPod, "the flow-style file")
	wantFile(t, filepath.Join(mixed, "web.yaml"), patchedDeployment, "the file of the workload that could be patched")
	if !strings.Contains(stderr, "2 selected workload(s) are in a form that is not edited") {
		t.Errorf("stderr:\n%s", stderr)
	}
}

// TestPatch_ExitCodeOfADryRun: the dry-run has the exit code of the real
// run. It says before the write that some workloads need an edit by hand,
// and with nothing patched the code is 1, as for an apply where every step
// failed.
func TestPatch_ExitCodeOfADryRun(t *testing.T) {
	t.Parallel()
	some := writeManifests(t, map[string]string{"web.yaml": plainDeployment, "flow.yaml": flowPod})
	dry, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{some}, DryRun: true})
	if code := PatchExitCode(dry); code != 3 {
		t.Errorf("dry-run exit code = %d, want 3", code)
	}

	none := writeManifests(t, map[string]string{"a.json": jsonPod("a"), "b.json": jsonPod("b")})
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{none}, DryRun: true})
	if code := PatchExitCode(res); code != 1 || res.Spec.Summary.Patched != 0 || len(res.Spec.Files) != 0 {
		t.Errorf("exit code = %d, summary = %+v; want 1 with nothing patched", code, res.Spec.Summary)
	}
}

func TestPatch_Errors(t *testing.T) {
	t.Parallel()
	root := writeManifests(t, map[string]string{"web.yaml": plainDeployment})
	cases := []struct {
		name string
		opts PatchOptions
		want string
	}{
		{"no path", PatchOptions{}, "no manifest paths: give the files or directories to edit"},
		{"standard input", PatchOptions{ManifestPaths: []string{"-"}}, `standard input (the path "-") has no file to edit`},
		{"standard input next to a directory", PatchOptions{ManifestPaths: []string{root, "-"}}, `standard input (the path "-") has no file to edit`},
		{"a class name that is not a name", PatchOptions{ManifestPaths: []string{root}, RuntimeClassName: "gvisor\n  hostNetwork: true"}, "is not a valid RuntimeClass name"},
		{"a path that is not there", PatchOptions{ManifestPaths: []string{filepath.Join(root, "none.yaml")}}, "reading manifests"},
		{"a bad selector", PatchOptions{ManifestPaths: []string{root}, LabelSelector: "=="}, "label selector"},
		{"a document that does not parse", PatchOptions{ManifestPaths: []string{writeManifests(t, map[string]string{"bad.yaml": "apiVersion: v1\nkind: [\n"})}}, "manifest problem"},
		{"a Helm template", PatchOptions{ManifestPaths: []string{writeManifests(t, map[string]string{"t.yaml": "apiVersion: v1\nkind: Pod\nmetadata:\n  name: {{ .Release.Name }}\n"})}}, "A Helm template cannot be edited"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.opts.Stderr = &bytes.Buffer{}
			res, err := Patch(context.Background(), tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Patch = %v, %v; want an error that has %q", res, err, tc.want)
			}
		})
	}
	// The valid file next to a bad class name was not changed.
	if got := readFile(t, filepath.Join(root, "web.yaml")); got != plainDeployment {
		t.Errorf("a refused patch changed the file")
	}
}

// TestPatch_ASymbolicLinkStaysALink: a path that the user named can be a
// link. The file behind it gets the change, and the link is still a link.
func TestPatch_ASymbolicLinkStaysALink(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	root := writeManifests(t, map[string]string{"real/web.yaml": plainDeployment})
	link := filepath.Join(root, "link.yaml")
	if err := os.Symlink(filepath.Join("real", "web.yaml"), link); err != nil {
		t.Fatal(err)
	}
	res, _ := patchFiles(t, PatchOptions{ManifestPaths: []string{link}, DryRun: false})
	if res.Spec.Summary.Patched != 1 || !res.Spec.Files[0].Written {
		t.Fatalf("result = %+v", res.Spec)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link is a regular file now")
	}
	if got := readFile(t, filepath.Join(root, "real", "web.yaml")); !strings.Contains(got, "runtimeClassName: gvisor") {
		t.Errorf("the file behind the link does not have the class:\n%s", got)
	}
}

// TestPatch_AFileThatCannotBeWritten: the write of one file fails. Its
// workload is "failed" with the error, the other file is written, and the
// exit code is 3.
func TestPatch_AFileThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory that the user cannot write to")
	}
	api := strings.Replace(plainDeployment, "name: web", "name: api", 1)
	root := writeManifests(t, map[string]string{"open/web.yaml": plainDeployment, "locked/api.yaml": api})
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o500); err != nil { // #nosec G302 -- read and search only: the temporary file cannot be made.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) }) // #nosec G302 -- so that the temp directory can be removed.

	res, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: false})
	if got, want := res.Spec.Summary, (schema.PatchSummary{Total: 2, Selected: 2, Patched: 1, Failed: 1, Files: 2}); got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if code := PatchExitCode(res); code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if c := changeOf(t, res, "api"); c.Status != schema.PatchStatusFailed || !strings.Contains(c.Reason, "the file was not written") || c.Line != 0 {
		t.Errorf("api = %+v, want failed", c)
	}
	for _, f := range res.Spec.Files {
		// The file in the locked directory has an error and is not written;
		// the other one is written and has no error.
		isLocked := strings.Contains(f.Path, "locked")
		if f.Written == isLocked || (f.Error != "") != isLocked {
			t.Errorf("file %+v: written and error do not fit", f)
		}
	}
	wantFile(t, filepath.Join(locked, "api.yaml"), api, "the file that could not be written")
	wantFile(t, filepath.Join(root, "open", "web.yaml"), patchedDeployment, "the file that could be written")
	for _, want := range []string{"1 of 2 file(s) written", "1 selected workload(s) are in a file that could not be written"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
}

// TestWriteManifest_RefusesAFileThatChanged: the edit was made for the text
// that was read. If the file has another text now, it is not written.
func TestWriteManifest_RefusesAFileThatChanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "web.yaml")
	if err := os.WriteFile(path, []byte("edited by a person\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := writeManifest(path, []byte(plainDeployment), []byte(plainDeployment+"x"))
	if err == nil || !strings.Contains(err.Error(), "changed after it was read") {
		t.Fatalf("writeManifest = %v, want a refusal", err)
	}
	if got := readFile(t, path); got != "edited by a person\n" {
		t.Errorf("the file was written: %q", got)
	}
}

// TestPatch_ReadsNoCluster is the invariant test: a patch builds no
// Kubernetes client and reads no kubeconfig. PatchOptions has no field for
// a client or a kubeconfig, so the one way in is the environment. The
// kubeconfig here names a context on a server that does not exist; the
// patch must work, and the name must be in no output.
func TestPatch_ReadsNoCluster(t *testing.T) {
	kubeconfig := writeFile(t, "kubeconfig", `apiVersion: v1
kind: Config
current-context: canary-must-not-appear
clusters: [{name: canary, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: canary-must-not-appear, context: {cluster: canary, user: canary}}]
users: [{name: canary, user: {}}]
`)
	t.Setenv("KUBECONFIG", kubeconfig) // no t.Parallel: Setenv forbids it

	root := patchTree(t)
	res, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: false})
	if res.Spec.Summary.Patched != 1 {
		t.Fatalf("summary = %+v, want 1 patched", res.Spec.Summary)
	}
	data, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(data), stderr} {
		if strings.Contains(text, "canary") {
			t.Errorf("the kubeconfig is in the output:\n%s", text)
		}
	}
	if res.Metadata.Source == nil || res.Metadata.Source.Type != schema.SourceManifests || res.Metadata.Source.Inputs != 7 {
		t.Errorf("source = %+v, want manifests with 7 inputs", res.Metadata.Source)
	}
}

// TestPatch_UnrenderedInputIsEditedWithAWarning: a Kustomize tree. The base
// is a file that a person edits, so patch edits it; the result and stderr
// say that the verdicts describe the files and not what gets deployed.
func TestPatch_UnrenderedInputIsEditedWithAWarning(t *testing.T) {
	t.Parallel()
	root := kustomizeTree(t)
	res, stderr := patchFiles(t, PatchOptions{ManifestPaths: []string{root}, DryRun: true})
	if len(res.Metadata.Source.Unrendered) == 0 {
		t.Fatalf("source = %+v, want the signs of input that is not rendered", res.Metadata.Source)
	}
	if !strings.Contains(stderr, "this input is not rendered") || !strings.Contains(stderr, "--fail-on sandboxed") {
		t.Errorf("stderr lacks the warning:\n%s", stderr)
	}
	// A partial object (an overlay patch) is never a target.
	for _, c := range res.Spec.Changes {
		if c.Origin != nil && strings.Contains(c.Origin.File, "overlay") {
			t.Errorf("an overlay patch was selected: %+v", c)
		}
	}
}

func TestPatchExitCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sum  schema.PatchSummary
		want int
	}{
		{schema.PatchSummary{}, 0},
		{schema.PatchSummary{Selected: 2, Patched: 2}, 0},
		{schema.PatchSummary{Selected: 3, Patched: 2, NotEdited: 1}, 3},
		{schema.PatchSummary{Selected: 3, Patched: 2, Failed: 1}, 3},
		{schema.PatchSummary{Selected: 2, NotEdited: 1, Failed: 1}, 1},
		{schema.PatchSummary{Selected: 1, Failed: 1}, 1},
	} {
		res := schema.NewPatchResult()
		res.Spec.Summary = tc.sum
		if got := PatchExitCode(res); got != tc.want {
			t.Errorf("PatchExitCode(%+v) = %d, want %d", tc.sum, got, tc.want)
		}
	}
}
