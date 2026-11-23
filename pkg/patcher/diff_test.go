// Tests for the unified diff. The form is checked two ways: against text
// written by hand, and by `git apply`, which is the reader that matters: if
// git takes the diff of the old file and gives the new file, the diff says
// what the edit did.
package patcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnifiedDiff_OneHunk(t *testing.T) {
	t.Parallel()
	res := AddRuntimeClass([]byte(deployment), "gvisor", targetsOf(t, deployment))
	const want = `--- a/k8s/web.yaml
+++ b/k8s/web.yaml
@@ -10,6 +10,7 @@
     metadata:
       labels: {app: web}
     spec:
+      runtimeClassName: gvisor
       # The one container.
       containers:
         - name: web
`
	if got := UnifiedDiff("./k8s/web.yaml", res.Text, res.Added); got != want {
		t.Errorf("diff =\n%s\nwant\n%s", got, want)
	}
	if got := UnifiedDiff("k8s/web.yaml", res.Text, nil); got != "" {
		t.Errorf("a diff with no added line = %q, want empty", got)
	}
}

// manyPods returns a file of n Pods in block style, each with `padding`
// comment lines before its spec. With enough padding, the changes are far
// apart and each one has a hunk of its own.
func manyPods(n, padding int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString("apiVersion: v1\nkind: Pod\nmetadata:\n  name: pod-" + string(rune('a'+i)) + "\n")
		for p := 0; p < padding; p++ {
			b.WriteString("  # padding\n")
		}
		b.WriteString("spec:\n  containers:\n    - {name: c, image: busybox}\n")
	}
	return b.String()
}

// shortPods returns a file of n Pods that are six lines apart, so the
// context of one change reaches the next one and the hunks join.
func shortPods(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString("---\n")
		}
		b.WriteString("apiVersion: v1\nkind: Pod\nmetadata: {name: pod-" + string(rune('a'+i)) + "}\nspec:\n  containers: [{name: c, image: busybox}]\n")
	}
	return b.String()
}

// TestUnifiedDiff_GitApply: git reads each diff and makes the new file from
// the old one. The cases are the ones where a hunk header is easy to get
// wrong: a change near the start, hunks that join, hunks that do not, CRLF,
// and a file with no line ending on its last line.
func TestUnifiedDiff_GitApply(t *testing.T) {
	t.Parallel()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	shortPod := "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers:\n    - {name: c, image: busybox}"
	cases := map[string]string{
		"one Deployment":                    deployment,
		"hunks that join (short objects)":   shortPods(4),
		"hunks that almost join":            manyPods(4, 0),
		"hunks that are apart":              manyPods(3, 12),
		"no line ending on the last line":   shortPod,
		"the change is on line 2 of a file": "spec:\n  containers:\n    - {name: c, image: busybox}\nkind: Pod\napiVersion: v1\nmetadata: {name: p}\n",
		"CRLF":                              strings.ReplaceAll(manyPods(2, 5), "\n", "\r\n"),
	}
	for name, text := range cases {
		name, text := name, text
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res := AddRuntimeClass([]byte(text), "gvisor", targetsOf(t, text))
			if len(res.Added) == 0 {
				t.Fatalf("nothing was edited: %+v", res.Outcomes)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "file.yaml"), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			diff := UnifiedDiff("file.yaml", res.Text, res.Added)
			// --whitespace=nowarn: the CRLF case has a carriage return at
			// the end of each line, which git reports and still applies.
			cmd := exec.Command(git, "apply", "--whitespace=nowarn", "-") // #nosec G204 -- git from PATH, fixed arguments.
			cmd.Dir = dir
			cmd.Stdin = strings.NewReader(diff)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git apply: %v\n%s\nthe diff:\n%s", err, out, diff)
			}
			got, err := os.ReadFile(filepath.Join(dir, "file.yaml")) // #nosec G304 -- a path under t.TempDir().
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(res.Text) {
				t.Errorf("git made\n%q\nfrom the diff, want\n%q\nthe diff:\n%s", got, res.Text, diff)
			}
		})
	}
}

func TestUnifiedDiff_Hunks(t *testing.T) {
	t.Parallel()
	// Three Pods, 12 padding lines each: the added lines are far apart, so
	// there are three hunks, and the old line numbers of the later hunks
	// are less than the new ones by the number of lines added before. Each
	// hunk has three lines of context on each side; the last has two after
	// the change, because the file ends there.
	text := manyPods(3, 12)
	res := AddRuntimeClass([]byte(text), "gvisor", targetsOf(t, text))
	diff := UnifiedDiff("pods.yaml", res.Text, res.Added)
	var headers []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "@@") {
			headers = append(headers, line)
		}
	}
	want := []string{"@@ -15,6 +15,7 @@", "@@ -35,6 +36,7 @@", "@@ -55,5 +57,6 @@"}
	if strings.Join(headers, "|") != strings.Join(want, "|") {
		t.Errorf("hunk headers = %q, want %q\n%s", headers, want, diff)
	}

	// Four short Pods: the context of each change reaches the next one, so
	// the diff has one hunk for the four added lines.
	text = shortPods(4)
	res = AddRuntimeClass([]byte(text), "gvisor", targetsOf(t, text))
	diff = UnifiedDiff("pods.yaml", res.Text, res.Added)
	if n := strings.Count(diff, "\n@@"); n != 1 || !strings.Contains(diff, "@@ -2,22 +2,26 @@") {
		t.Errorf("hunks = %d, want 1 with the header -2,22 +2,26\n%s", n, diff)
	}
}

func TestUnifiedDiff_Names(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"web.yaml":            "--- a/web.yaml\n+++ b/web.yaml\n",
		"./k8s//web.yaml":     "--- a/k8s/web.yaml\n+++ b/k8s/web.yaml\n",
		"/repo/k8s/web.yaml":  "--- /repo/k8s/web.yaml\n+++ /repo/k8s/web.yaml\n",
		"k8s/we\nb.yaml":      "--- a/k8s/we?b.yaml\n+++ b/k8s/we?b.yaml\n",
		"k8s/\x1b[31mred.yml": "--- a/k8s/?[31mred.yml\n+++ b/k8s/?[31mred.yml\n",
	} {
		got := UnifiedDiff(path, []byte("a\nb\n"), []int{2})
		if !strings.HasPrefix(got, want) {
			t.Errorf("path %q: diff starts\n%q\nwant\n%q", path, got, want)
		}
	}
}

func TestUnifiedDiff_NoNewlineAtEndOfFile(t *testing.T) {
	t.Parallel()
	const text = "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec:\n  containers:\n    - {name: c, image: busybox}"
	res := AddRuntimeClass([]byte(text), "gvisor", targetsOf(t, text))
	const want = `--- a/pod.yaml
+++ b/pod.yaml
@@ -2,5 +2,6 @@
 kind: Pod
 metadata: {name: p}
 spec:
+  runtimeClassName: gvisor
   containers:
     - {name: c, image: busybox}
\ No newline at end of file
`
	if got := UnifiedDiff("pod.yaml", res.Text, res.Added); got != want {
		t.Errorf("diff =\n%s\nwant\n%s", got, want)
	}
}
