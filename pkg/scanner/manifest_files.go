// Package scanner: reading manifest text from paths.
//
// ReadManifestPaths is the I/O half of manifest input. It turns what a user
// typed after `-f` (a file, a directory, or "-" for stdin) into the
// (name, bytes) pairs that FromManifests decodes. The split keeps
// FromManifests free of I/O, so manifest text from another source (text
// that is embedded in a binary, for example) and the files of a real
// repository go through the same decoder.
//
// The rules are written for the place this runs most: a CI job pointed at a
// repository it did not write.
//
//   - A named file is always read, whatever its extension. The user asked
//     for it.
//   - A directory is walked. Only regular *.yaml, *.yml and *.json files
//     are read. Symbolic links are not followed (a link can point outside
//     the checkout) and dot-directories are skipped (.git, .github).
//   - In a walk, a file that does not contain the text "apiVersion" is not a
//     Kubernetes manifest (a workflow file, a values file, an OpenAPI
//     document) and is left out. A file the user names is never left out
//     this way.
//   - Each input has a size limit and a walk has a file-count limit, so a
//     wrong path (`-f /`) fails fast instead of reading a disk.
//   - A kustomization file is recorded by its name, whatever is in it. Most
//     of them have no apiVersion and no kind, so the decoder cannot see
//     them, and the fact that a directory is a Kustomize build (bases plus
//     patches, not the objects that get deployed) would be lost.
package scanner

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// StdinPath is the path argument that means "read standard input".
	StdinPath = "-"

	// StdinName is the ManifestInput.Name for standard input.
	StdinName = "<stdin>"

	// MaxManifestBytes is the size limit for one input. A manifest file is
	// kilobytes; the rendered output of a very large chart is a few
	// megabytes.
	MaxManifestBytes = 32 << 20

	// MaxManifestFiles is the limit on files examined in directory walks,
	// summed over one ReadManifestPaths call.
	MaxManifestFiles = 10000
)

// manifestExtensions are the file extensions a directory walk reads.
var manifestExtensions = map[string]struct{}{".yaml": {}, ".yml": {}, ".json": {}}

// kustomizationNames are the three file names `kustomize build` looks for.
var kustomizationNames = map[string]struct{}{
	"kustomization.yaml": {}, "kustomization.yml": {}, "Kustomization": {},
}

// apiVersionMarker is the text every Kubernetes manifest contains. A
// walked file without it is not one.
var apiVersionMarker = []byte("apiVersion")

// ManifestFiles is what ReadManifestPaths read.
type ManifestFiles struct {
	// Inputs holds the manifest text, in the order the paths were given (a
	// directory expands in lexical order, in place).
	Inputs []ManifestInput

	// Ignored is the number of files that directory walks left out because
	// they do not look like manifests. It lets the caller say "read 12
	// files, ignored 30" instead of leaving the user to wonder.
	Ignored int

	// Kustomizations lists the kustomization files that were met, by name
	// (sorted). See the package comment at the top of this file.
	Kustomizations []string
}

// ReadManifestPaths reads every path: a file, a directory, or "-".
//
// stdin is the reader for the "-" path. Pass nil where standard input is
// not available (the MCP server: its stdin is the JSON-RPC channel); "-" is
// then an error. The caller, not this function, decides whether reading a
// terminal is sensible.
func ReadManifestPaths(paths []string, stdin io.Reader) (*ManifestFiles, error) {
	if len(paths) == 0 {
		return nil, errors.New("no manifest path given")
	}

	r := &manifestReader{stdin: stdin, seen: map[string]struct{}{}}
	for _, path := range paths {
		if err := r.readPath(path); err != nil {
			return nil, err
		}
	}
	sort.Strings(r.kustomizations)
	return &ManifestFiles{Inputs: r.inputs, Ignored: r.ignored, Kustomizations: r.kustomizations}, nil
}

type manifestReader struct {
	stdin     io.Reader
	stdinUsed bool

	inputs         []ManifestInput
	kustomizations []string
	ignored        int
	walked         int // files examined in directory walks

	// seen holds the cleaned path of every file read, so `-f k8s -f
	// k8s/app.yaml` does not read app.yaml twice and report every workload
	// in it as a duplicate.
	seen map[string]struct{}
}

func (r *manifestReader) readPath(path string) error {
	switch {
	case path == StdinPath:
		return r.readStdin()
	case path == "":
		return errors.New("empty manifest path")
	case strings.HasPrefix(path, "http://"), strings.HasPrefix(path, "https://"):
		return fmt.Errorf("%s: a URL is not read; download the file, or pipe it: curl -sSL <url> | agentmoat scan -f -", path)
	}

	// os.Stat follows a symbolic link. That is intended for a path the
	// user names; only links met during a walk are not followed.
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if info.IsDir() {
		return r.walk(path)
	}
	return r.readFile(path, true)
}

func (r *manifestReader) readStdin() error {
	if r.stdin == nil {
		return errors.New(`"-" (standard input) is not available here; give a file or directory path`)
	}
	if r.stdinUsed {
		return errors.New(`"-" (standard input) can be given only once`)
	}
	r.stdinUsed = true

	data, err := readLimited(r.stdin, StdinName)
	if err != nil {
		return err
	}
	r.inputs = append(r.inputs, ManifestInput{Name: StdinName, Data: data})
	return nil
}

// walk reads the manifest files under root.
func (r *manifestReader) walk(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		if d.IsDir() {
			// Skip .git, .github, .terraform and the like, but not the
			// root itself (`-f .` and `-f ./.deploy` must both work).
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		// Regular files only. This leaves out symbolic links (not
		// followed), sockets, and devices.
		if !d.Type().IsRegular() {
			return nil
		}
		if _, ok := manifestExtensions[strings.ToLower(filepath.Ext(d.Name()))]; !ok {
			// "Kustomization" has no extension. It is recorded, not read.
			r.noteKustomization(path)
			return nil
		}
		r.walked++
		if r.walked > MaxManifestFiles {
			return fmt.Errorf("%s: more than %d manifest files; give a narrower path", root, MaxManifestFiles)
		}
		return r.readFile(path, false)
	})
}

// readFile reads one file. named is true for a path the user gave and false
// for a file found in a walk; only the second kind is subject to the
// "does it look like a manifest" filter.
func (r *manifestReader) readFile(path string, named bool) error {
	key := filepath.Clean(path)
	if _, dup := r.seen[key]; dup {
		return nil
	}
	r.seen[key] = struct{}{}
	isKustomization := r.noteKustomization(path)

	// #nosec G304 -- the path is the operator's -f argument (or a file
	// under the directory they named); reading it is the feature.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	name := filepath.ToSlash(path)
	data, err := readLimited(f, name)
	if err != nil {
		return err
	}
	if !named && !bytes.Contains(data, apiVersionMarker) {
		// A kustomization file is accounted for in Kustomizations, so it
		// is not also counted as "ignored".
		if !isKustomization {
			r.ignored++
		}
		return nil
	}
	r.inputs = append(r.inputs, ManifestInput{Name: name, Data: data})
	return nil
}

// noteKustomization records path when its base name is one of the
// kustomization file names, and reports whether it is.
func (r *manifestReader) noteKustomization(path string) bool {
	if _, ok := kustomizationNames[filepath.Base(path)]; !ok {
		return false
	}
	name := filepath.ToSlash(path)
	for _, have := range r.kustomizations {
		if have == name {
			return true
		}
	}
	r.kustomizations = append(r.kustomizations, name)
	return true
}

// readLimited reads all of src, and fails when it holds more than
// MaxManifestBytes. It reads one byte past the limit to tell "exactly at
// the limit" from "over it".
func readLimited(src io.Reader, name string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(src, MaxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	if len(data) > MaxManifestBytes {
		return nil, fmt.Errorf("%s: larger than the %d MiB limit for one manifest input", name, MaxManifestBytes>>20)
	}
	return data, nil
}
