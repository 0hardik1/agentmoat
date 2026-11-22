// Tests for ReadManifestPaths: files, directories, and stdin into
// ManifestInput values. Every test builds its own tree under t.TempDir().
package scanner

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const podManifest = "apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec: {containers: [{name: c, image: busybox}]}\n"

// writeTree creates files under root. Keys are slash-separated relative
// paths; parent directories are made as needed.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// readPaths calls ReadManifestPaths and spreads the result, for the tests
// that look only at the inputs and the ignored count.
func readPaths(paths []string, stdin io.Reader) ([]ManifestInput, int, error) {
	files, err := ReadManifestPaths(paths, stdin)
	if err != nil {
		return nil, 0, err
	}
	return files.Inputs, files.Ignored, nil
}

// relNames returns the input names relative to root, in input order.
func relNames(t *testing.T, root string, inputs []ManifestInput) []string {
	t.Helper()
	out := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if in.Name == StdinName {
			out = append(out, in.Name)
			continue
		}
		rel, err := filepath.Rel(root, filepath.FromSlash(in.Name))
		if err != nil {
			t.Fatalf("rel %s: %v", in.Name, err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func TestReadManifestPaths_DirectoryWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"b/deploy.yaml":             podManifest,
		"a/pod.yml":                 podManifest,
		"a/list.JSON":               `{"apiVersion":"v1","kind":"List","items":[]}`,
		"a/values.yaml":             "replicaCount: 2\nimage: nginx\n", // no apiVersion: not a manifest
		"a/README.md":               "apiVersion appears here but the extension is wrong\n",
		"a/script.sh":               "#!/bin/sh\n",
		".github/workflows/ci.yaml": "apiVersion: not-really\n", // dot-directory: skipped
		"a/.hidden/pod.yaml":        podManifest,                // dot-directory: skipped
		"zz.yaml":                   podManifest,
	})

	inputs, ignored, err := readPaths([]string{root}, nil)
	if err != nil {
		t.Fatalf("ReadManifestPaths: %v", err)
	}
	// Lexical walk order, and only the files that look like manifests.
	want := []string{"a/list.JSON", "a/pod.yml", "b/deploy.yaml", "zz.yaml"}
	if got := relNames(t, root, inputs); !equalStrings(got, want) {
		t.Errorf("inputs = %v, want %v", got, want)
	}
	if ignored != 1 {
		t.Errorf("ignored = %d, want 1 (values.yaml)", ignored)
	}
	for _, in := range inputs {
		if strings.Contains(in.Name, `\`) {
			t.Errorf("input name %q is not slash-separated", in.Name)
		}
	}
}

func TestReadManifestPaths_NamedFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"rendered.txt":     podManifest,         // odd extension
		"values.yaml":      "replicaCount: 2\n", // no apiVersion
		".deploy/app.yaml": podManifest,         // inside a dot-directory
	})

	t.Run("a named file is read whatever its name or content", func(t *testing.T) {
		t.Parallel()
		paths := []string{filepath.Join(root, "rendered.txt"), filepath.Join(root, "values.yaml")}
		inputs, ignored, err := readPaths(paths, nil)
		if err != nil {
			t.Fatalf("ReadManifestPaths: %v", err)
		}
		if got, want := relNames(t, root, inputs), []string{"rendered.txt", "values.yaml"}; !equalStrings(got, want) {
			t.Errorf("inputs = %v, want %v", got, want)
		}
		if ignored != 0 {
			t.Errorf("ignored = %d, want 0", ignored)
		}
	})

	t.Run("a dot-directory named as the root is walked", func(t *testing.T) {
		t.Parallel()
		inputs, _, err := readPaths([]string{filepath.Join(root, ".deploy")}, nil)
		if err != nil {
			t.Fatalf("ReadManifestPaths: %v", err)
		}
		if got, want := relNames(t, root, inputs), []string{".deploy/app.yaml"}; !equalStrings(got, want) {
			t.Errorf("inputs = %v, want %v", got, want)
		}
	})

	t.Run("a file given twice, or inside a given directory, is read once", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		writeTree(t, dir, map[string]string{"k8s/app.yaml": podManifest})
		file := filepath.Join(dir, "k8s", "app.yaml")
		inputs, _, err := readPaths([]string{filepath.Join(dir, "k8s"), file, file}, nil)
		if err != nil {
			t.Fatalf("ReadManifestPaths: %v", err)
		}
		if len(inputs) != 1 {
			t.Errorf("inputs = %v, want one", relNames(t, dir, inputs))
		}
	})
}

func TestReadManifestPaths_Stdin(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{"app.yaml": podManifest})
	file := filepath.Join(root, "app.yaml")

	t.Run("stdin and a file together, in the order given", func(t *testing.T) {
		t.Parallel()
		inputs, _, err := readPaths([]string{"-", file}, strings.NewReader(podManifest))
		if err != nil {
			t.Fatalf("ReadManifestPaths: %v", err)
		}
		if got, want := relNames(t, root, inputs), []string{StdinName, "app.yaml"}; !equalStrings(got, want) {
			t.Errorf("inputs = %v, want %v", got, want)
		}
		if string(inputs[0].Data) != podManifest {
			t.Errorf("stdin data = %q", inputs[0].Data)
		}
	})

	tests := []struct {
		name    string
		paths   []string
		stdin   io.Reader
		wantErr string
	}{
		{name: "no stdin available", paths: []string{"-"}, stdin: nil, wantErr: "not available here"},
		{name: "stdin given twice", paths: []string{"-", "-"}, stdin: strings.NewReader(podManifest), wantErr: "only once"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := readPaths(tc.paths, tc.stdin)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestReadManifestPaths_Errors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()

	tests := []struct {
		name    string
		paths   []string
		wantErr string
	}{
		{name: "no path", paths: nil, wantErr: "no manifest path"},
		{name: "empty path", paths: []string{""}, wantErr: "empty manifest path"},
		{name: "a path that does not exist", paths: []string{filepath.Join(root, "nope.yaml")}, wantErr: "nope.yaml"},
		{name: "a URL", paths: []string{"https://example.test/app.yaml"}, wantErr: "a URL is not read"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			inputs, _, err := readPaths(tc.paths, nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
			if inputs != nil {
				t.Errorf("inputs = %v, want nil on error", inputs)
			}
		})
	}
}

// TestReadManifestPaths_Symlinks: a walk does not follow links (a link in a
// checkout can point at any file the CI job can read), but a link the user
// names is read.
func TestReadManifestPaths_Symlinks(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("creating symbolic links needs a privilege on Windows")
	}

	outside := t.TempDir()
	writeTree(t, outside, map[string]string{
		"outside.yaml":    "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: outside}\n",
		"more/other.yaml": podManifest,
	})
	root := t.TempDir()
	writeTree(t, root, map[string]string{"real.yaml": podManifest})
	if err := os.Symlink(filepath.Join(outside, "outside.yaml"), filepath.Join(root, "link.yaml")); err != nil {
		t.Fatalf("symlink file: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "more"), filepath.Join(root, "linkdir")); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	inputs, _, err := readPaths([]string{root}, nil)
	if err != nil {
		t.Fatalf("ReadManifestPaths: %v", err)
	}
	if got, want := relNames(t, root, inputs), []string{"real.yaml"}; !equalStrings(got, want) {
		t.Errorf("walk inputs = %v, want %v (links not followed)", got, want)
	}

	named, _, err := readPaths([]string{filepath.Join(root, "link.yaml")}, nil)
	if err != nil {
		t.Fatalf("ReadManifestPaths on a named link: %v", err)
	}
	if len(named) != 1 || !bytes.Contains(named[0].Data, []byte("kind: ConfigMap")) {
		t.Errorf("a named link was not read: %v", named)
	}
}

// endless is a reader that never ends. The size limit must stop the read.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

func TestReadManifestPaths_SizeLimit(t *testing.T) {
	t.Parallel()

	t.Run("stdin over the limit", func(t *testing.T) {
		t.Parallel()
		_, _, err := readPaths([]string{"-"}, endless{})
		if err == nil || !strings.Contains(err.Error(), "32 MiB limit") || !strings.Contains(err.Error(), StdinName) {
			t.Errorf("err = %v, want the size-limit error for stdin", err)
		}
	})

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		t.Parallel()
		data, err := readLimited(io.LimitReader(endless{}, MaxManifestBytes), "exact")
		if err != nil || len(data) != MaxManifestBytes {
			t.Errorf("readLimited at the limit: len = %d, err = %v", len(data), err)
		}
	})

	t.Run("a file over the limit", func(t *testing.T) {
		t.Parallel()
		// A sparse file: the size is set without writing 32 MiB of data.
		path := filepath.Join(t.TempDir(), "big.yaml")
		f, err := os.Create(path) // #nosec G304 -- a path under t.TempDir()
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := f.Truncate(MaxManifestBytes + 1); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
		_, _, err = readPaths([]string{path}, nil)
		if err == nil || !strings.Contains(err.Error(), "32 MiB limit") {
			t.Errorf("err = %v, want the size-limit error", err)
		}
	})
}

// TestReadManifestPaths_IntoFromManifests joins the two halves: a directory
// on disk becomes workloads whose origin names the file.
func TestReadManifestPaths_IntoFromManifests(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"apps/web.yaml": deploymentWeb + "---\napiVersion: v1\nkind: Service\nmetadata: {name: web, namespace: shop}\n",
		"apps/job.yaml": "apiVersion: batch/v1\nkind: Job\nmetadata: {name: migrate, namespace: shop}\n" +
			"spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n",
	})

	inputs, _, err := readPaths([]string{root}, nil)
	if err != nil {
		t.Fatalf("ReadManifestPaths: %v", err)
	}
	res := mustManifests(t, inputs, ManifestOptions{})
	if got, want := ids(res), []string{"Deployment/shop/web", "Job/shop/migrate"}; !equalStrings(got, want) {
		t.Fatalf("workloads = %v, want %v", got, want)
	}
	if !strings.HasSuffix(res.Origins[0].File, "apps/web.yaml") || res.Origins[0].Line != 1 {
		t.Errorf("origin of web = %+v", res.Origins[0])
	}
	if res.Skipped["Service"] != 1 || res.Objects != 3 {
		t.Errorf("Skipped = %v, Objects = %d; want one Service and 3 objects", res.Skipped, res.Objects)
	}
}

// TestReadManifestPaths_Kustomizations: a kustomization file is recorded by
// its name. Most have no apiVersion, so nothing later in the pipeline could
// see that the directory is a Kustomize build.
func TestReadManifestPaths_Kustomizations(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"base/deploy.yaml":                podManifest,
		"base/kustomization.yaml":         "resources:\n  - deploy.yaml\n", // no apiVersion, as most are
		"overlays/prod/kustomization.yml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [../../base]\n",
		"overlays/dev/Kustomization":      "resources: [../../base]\n", // no extension
		"docs/not-kustomization.yaml":     "title: just a file\n",
		".hidden/kustomization.yaml":      "resources: []\n", // dot-directory: not walked
	})

	files, err := ReadManifestPaths([]string{root}, nil)
	if err != nil {
		t.Fatalf("ReadManifestPaths: %v", err)
	}

	var got []string
	for _, k := range files.Kustomizations {
		rel, err := filepath.Rel(root, filepath.FromSlash(k))
		if err != nil {
			t.Fatalf("rel %s: %v", k, err)
		}
		got = append(got, filepath.ToSlash(rel))
	}
	want := []string{"base/kustomization.yaml", "overlays/dev/Kustomization", "overlays/prod/kustomization.yml"}
	if !equalStrings(got, want) {
		t.Errorf("Kustomizations = %v, want %v", got, want)
	}

	// Only the file with an apiVersion is an input; the one without is
	// neither an input nor "ignored" (it is accounted for above).
	if got, want := relNames(t, root, files.Inputs), []string{"base/deploy.yaml", "overlays/prod/kustomization.yml"}; !equalStrings(got, want) {
		t.Errorf("inputs = %v, want %v", got, want)
	}
	if files.Ignored != 1 {
		t.Errorf("Ignored = %d, want 1 (docs/not-kustomization.yaml)", files.Ignored)
	}

	t.Run("a kustomization file named on the command line", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(root, "base", "kustomization.yaml")
		named, err := ReadManifestPaths([]string{path, path}, nil)
		if err != nil {
			t.Fatalf("ReadManifestPaths: %v", err)
		}
		if len(named.Kustomizations) != 1 || len(named.Inputs) != 1 {
			t.Errorf("Kustomizations = %v, inputs = %d; want one of each", named.Kustomizations, len(named.Inputs))
		}
	})
}
