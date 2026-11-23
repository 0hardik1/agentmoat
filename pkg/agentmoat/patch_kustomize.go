// Package agentmoat: `patch --kustomize`, the change as a Kustomize component.
//
// `patch` edits the source files. In a Kustomize tree the source of a
// workload is its base, and a change to a base changes every overlay that
// uses it. A team that moves one environment first needs the change in one
// overlay only.
//
// With PatchOptions.KustomizeDir, Patch edits no source file. It writes a
// directory that is a Kustomize component: one strategic merge patch for
// each selected workload (pkg/patcher makes the text), and a
// kustomization.yaml of kind Component that lists the patches. The
// kustomization that must move adds the directory to its `components:`.
//
// The rules, and why:
//
//   - A run only adds. It makes patch files that are not there, and it adds
//     entries at the end of its own list. It changes no file that is there,
//     and it removes nothing. A reviewer of a pull request from a pipeline
//     then knows what kind of change to expect, as with the edit in place.
//   - The directory is new, empty, or the directory of an earlier run. A
//     kustomization.yaml in a directory changes what the directory is, for
//     Kustomize and for a deploy tool that reads it, so a run does not put
//     one next to the files of a person.
//   - A workload is done when its patch file is there with the right
//     content AND the list names it. The two are checked together: a file
//     that no list names does nothing in a build.
//   - The patch files are written first and the list last. A run that stops
//     in the middle leaves files that no list names (they do nothing), and
//     never a list that names a file that is not there (that stops a build).
//   - A second run changes nothing: the workloads that are done are
//     excluded, as a workload with the class is for the edit in place.
//
// What this code does not do: it does not run Kustomize, and it does not
// edit the kustomization of the user. The names in a patch are the names in
// the input, so the input must be what that kustomization reads from its
// `resources:`. A patch for an object that the kustomization does not read
// stops its build with a clear error; that is how Kustomize reports it.
package agentmoat

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/patcher"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// maxComponentFileBytes is the largest file of a component directory that a
// run reads. A patch file is a few hundred bytes, and the list has one short
// line for each workload. A larger file is not one of ours.
const maxComponentFileBytes = 4 << 20

// kustomizeRun is the state of the Kustomize form of one Patch call.
type kustomizeRun struct {
	// dir is the component directory, as it was given.
	dir string

	// What is on disk before the run.
	dirExists bool
	list      []byte              // the text of the list; nil when there is none
	listed    map[string]bool     // the entries of the list
	disk      map[string]diskFile // each file that was looked at, by name

	// What the run makes.
	files     map[string]*componentFile // the patch files of this run, by name
	newList   []byte                    // the list after the run; nil when it does not change
	added     []string                  // the entries that the run adds to the list
	listWrote bool
	listErr   error
}

// componentFile is the patch file of one selected workload (or of more than
// one, when the input has the same object two times).
type componentFile struct {
	name string
	path string
	body []byte

	// present is true when the file is on disk with this content already.
	present bool

	// changes is the indexes, in patchRun.changes, of its workloads.
	changes []int

	wrote bool
	err   error
}

// diskState is what a path in the component directory is.
type diskState int

const (
	diskAbsent  diskState = iota // nothing has this name
	diskRegular                  // a regular file; diskFile.data has its bytes
	diskLarge                    // a regular file that is too large to be one of ours
	diskOther                    // a directory, a symbolic link, a device
	diskError                    // the path cannot be read; diskFile.err says why
)

type diskFile struct {
	state diskState
	data  []byte
	err   error
}

// lookAt reads one path of the component directory. It does not follow a
// symbolic link: a link there is not a file that a run made, and a write
// through it could land outside the directory.
func lookAt(path string) diskFile {
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return diskFile{state: diskAbsent}
	case err != nil:
		return diskFile{state: diskError, err: err}
	case !info.Mode().IsRegular():
		return diskFile{state: diskOther}
	case info.Size() > maxComponentFileBytes:
		return diskFile{state: diskLarge}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- a file in the directory that the user named for the component.
	if err != nil {
		return diskFile{state: diskError, err: err}
	}
	return diskFile{state: diskRegular, data: data}
}

// readKustomizeDir looks at the component directory before anything is
// selected, and refuses a directory that a run must not write into. It
// returns an error for the caller to show; nothing was changed then.
func readKustomizeDir(dir string) (*kustomizeRun, error) {
	k := &kustomizeRun{
		dir:    dir,
		listed: map[string]bool{},
		disk:   map[string]diskFile{},
		files:  map[string]*componentFile{},
	}
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return k, nil // a run with DryRun false makes it
	case err != nil:
		return nil, fmt.Errorf("the directory for the Kustomize component: %w", err)
	case !info.IsDir():
		return nil, fmt.Errorf("the path for the Kustomize component, %s, is a file and not a directory", dir)
	}
	k.dirExists = true

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("the directory for the Kustomize component: %w", err)
	}
	example := filepath.Join(dir, "gvisor")
	hasList, others := false, 0
	for _, entry := range entries {
		name := entry.Name()
		switch lower := strings.ToLower(name); {
		case name == patcher.KustomizationFile:
			hasList = true
		case lower == "kustomization.yaml", lower == "kustomization.yml", lower == "kustomization":
			// The other names that Kustomize reads as a kustomization.
			return nil, fmt.Errorf("%s has a %s: it is the directory of another kustomization. "+
				"Give the component a directory of its own (for example %s)", dir, name, example)
		case strings.HasPrefix(name, "."):
			// A hidden file (.gitkeep) does not make the directory one of
			// a person.
		default:
			others++
		}
	}
	listPath := filepath.Join(dir, patcher.KustomizationFile)
	if !hasList {
		if others > 0 {
			return nil, fmt.Errorf("%s has files, and no %s that agentmoat wrote. A run writes a %s there, "+
				"and that makes the directory a Kustomize component for each tool that reads it. "+
				"Give the component a new or an empty directory (for example %s)",
				dir, patcher.KustomizationFile, patcher.KustomizationFile, example)
		}
		return k, nil
	}

	const whatNow = "Give the component another directory, or remove the file " +
		"(the patch files can stay: the next run lists the ones of the workloads that it selects)"
	on := lookAt(listPath)
	switch on.state {
	case diskRegular:
		// read below
	case diskError:
		return nil, fmt.Errorf("the list of the Kustomize component: %w", on.err)
	default:
		return nil, fmt.Errorf("%s is not a file that a run can add to: it is not a regular file of the size of a list. %s", listPath, whatNow)
	}
	paths, err := patcher.ComponentPaths(on.data)
	if err != nil {
		return nil, fmt.Errorf("%s is not a file that a run can add to: %w. %s", listPath, err, whatNow)
	}
	k.list = on.data
	for _, p := range paths {
		k.listed[p] = true
	}
	return k, nil
}

// look is lookAt for a file name in the directory, one time for each name.
func (k *kustomizeRun) look(name string) diskFile {
	if f, ok := k.disk[name]; ok {
		return f
	}
	f := diskFile{state: diskAbsent}
	if k.dirExists {
		f = lookAt(filepath.Join(k.dir, name))
	}
	k.disk[name] = f
	return f
}

// has reports whether the component has the patch of one object already:
// the list names the file, and the file has the content that this run would
// write. It returns the path of the file for the report.
func (k *kustomizeRun) has(obj scanner.ManifestObject, class string) (string, bool) {
	name, body, reason := patcher.KustomizePatch(obj, class)
	if reason != "" || !k.listed[name] {
		return "", false
	}
	if on := k.look(name); on.state != diskRegular || !bytes.Equal(on.data, body) {
		return "", false
	}
	return filepath.Join(k.dir, name), true
}

// planComponent decides, for each selected workload, which patch file it
// gets, and records the outcome. No file is written here.
func (r *patchRun) planComponent() {
	k := r.kustomize
	for i := range r.changes {
		change := &r.changes[i]
		name, body, reason := patcher.KustomizePatch(r.objects[i], r.class)
		if reason != "" {
			change.Status, change.Reason = schema.PatchStatusNotEdited, reason
			continue
		}
		path := filepath.Join(k.dir, name)

		if f, ok := k.files[name]; ok {
			// The input has an object with these four fields two times
			// (the report lists it as a duplicate). One file is the patch
			// of the two, when the two need the same patch.
			if !bytes.Equal(f.body, body) {
				change.Status = schema.PatchStatusNotEdited
				change.Reason = "another object of the input has the same apiVersion, kind, name, and namespace and needs another patch: " +
					"one file cannot be the patch of the two"
				continue
			}
			f.changes = append(f.changes, i)
			change.Status, change.PatchFile = schema.PatchStatusPatched, path
			continue
		}

		present, reason := k.canWrite(name, path, body)
		if reason != "" {
			change.Status, change.Reason = schema.PatchStatusNotEdited, reason
			continue
		}
		k.files[name] = &componentFile{name: name, path: path, body: body, present: present, changes: []int{i}}
		change.Status, change.PatchFile = schema.PatchStatusPatched, path
	}
	k.planList()
}

// canWrite says if a run can have the file name with this content: the name
// is free, or the file is there with the content already (present). In each
// other case it returns the reason. A run changes no file that is there.
func (k *kustomizeRun) canWrite(name, path string, body []byte) (present bool, reason string) {
	switch on := k.look(name); {
	case on.state == diskAbsent:
		return false, ""
	case on.state == diskRegular && bytes.Equal(on.data, body):
		return true, ""
	case on.state == diskRegular, on.state == diskLarge:
		return false, fmt.Sprintf("the file %s is there with other content, and a run changes no file that is there. "+
			"Remove it to get it again, or correct it by hand", path)
	case on.state == diskOther:
		return false, fmt.Sprintf("%s is there and is not a regular file", path)
	default:
		return false, fmt.Sprintf("%s cannot be read: %v", path, on.err)
	}
}

// planList makes the text of the list for the patch files that are there or
// will be. It runs again after the files are written, without the files
// that could not be written: the list must not name a file that is not
// there.
func (k *kustomizeRun) planList() {
	k.newList, k.added = nil, nil
	names := make([]string, 0, len(k.files))
	for name, f := range k.files {
		if f.err == nil {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return
	}
	text, added, err := patcher.ComponentAdd(k.list, names)
	if err != nil {
		// readKustomizeDir read this list with the same function, so this
		// does not occur. If it does, nothing is written to the list.
		k.listErr = err
		return
	}
	if len(added) > 0 {
		k.newList, k.added = text, added
	}
}

// writeComponent writes the patch files and then the list. A file that
// cannot be written does not stop the others: its workloads become "failed".
func (r *patchRun) writeComponent() {
	k := r.kustomize
	creates := k.toCreate()
	if len(creates) == 0 && k.newList == nil {
		return
	}

	var dirErr error
	if !k.dirExists {
		// The directory is a part of a repository: other users of the
		// checkout read it, and git keeps no mode for a directory.
		dirErr = os.MkdirAll(k.dir, 0o755) // #nosec G301 -- see the comment above.
	}
	for _, f := range creates {
		if f.err = dirErr; f.err == nil {
			f.err = writeNewFile(f.path, f.body)
		}
		if f.err == nil {
			f.wrote = true
			continue
		}
		for _, i := range f.changes {
			r.changes[i].Status = schema.PatchStatusFailed
			r.changes[i].PatchFile = ""
			r.changes[i].Reason = "the patch file was not written: " + f.err.Error()
		}
	}

	// The list is made again here: it names only the files that are there.
	k.planList()
	k.writeList()
	if k.listErr == nil {
		return
	}
	// The patch files of these workloads are there, and no list names them:
	// a build does not use them. The next run finds them and lists them.
	for _, name := range k.unlisted() {
		for _, i := range k.files[name].changes {
			r.changes[i].Status = schema.PatchStatusFailed
			r.changes[i].Reason = "the list of the component was not written, so a build does not use the patch file: " + k.listErr.Error()
		}
	}
}

// toCreate returns the patch files that are not on disk, sorted by name, so
// that the order of the writes does not come from a map.
func (k *kustomizeRun) toCreate() []*componentFile {
	creates := make([]*componentFile, 0, len(k.files))
	for _, f := range k.files {
		if !f.present {
			creates = append(creates, f)
		}
	}
	sort.Slice(creates, func(i, j int) bool { return creates[i].name < creates[j].name })
	return creates
}

// writeList writes the list when the run has new entries for it. A list
// that is there must be as it was read (writeManifest); a new one must not
// be there (writeNewFile).
func (k *kustomizeRun) writeList() {
	if k.listErr != nil || k.newList == nil {
		return
	}
	listPath := filepath.Join(k.dir, patcher.KustomizationFile)
	if k.list == nil {
		k.listErr = writeNewFile(listPath, k.newList)
	} else {
		k.listErr = writeManifest(listPath, k.list, k.newList)
	}
	k.listWrote = k.listErr == nil
}

// unlisted returns the names of the patch files of this run that the list on
// disk does not name, after a list that could not be written.
func (k *kustomizeRun) unlisted() []string {
	var names []string
	for name, f := range k.files {
		if f.err == nil && !k.listed[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// patchFiles returns the files of the component that the run makes or
// changes, with the diff of each, sorted by path.
func (k *kustomizeRun) patchFiles() []schema.PatchFile {
	var files []schema.PatchFile
	for _, f := range k.files {
		if f.present {
			continue
		}
		pf := schema.PatchFile{
			Path:    f.path,
			Created: true,
			Written: f.wrote,
			Diff:    patcher.FileDiff(f.path, nil, f.body, false),
		}
		pf.Added, pf.Removed = patcher.DiffStat(pf.Diff)
		if f.err != nil {
			pf.Error = f.err.Error()
		}
		files = append(files, pf)
	}
	if k.newList != nil {
		listPath := filepath.Join(k.dir, patcher.KustomizationFile)
		pf := schema.PatchFile{
			Path:    listPath,
			Created: k.list == nil,
			Written: k.listWrote,
			Diff:    patcher.FileDiff(listPath, k.list, k.newList, k.list != nil),
		}
		pf.Added, pf.Removed = patcher.DiffStat(pf.Diff)
		if k.listErr != nil {
			pf.Error = k.listErr.Error()
		}
		files = append(files, pf)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// writeNewFile makes a file that is not there. As writeManifest does, it
// writes a temporary file in the same directory and puts it in place with a
// rename, so that the name never has a part of the content. A name that was
// taken after the run looked at the directory is not written over.
func writeNewFile(path string, data []byte) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%s was made after the directory was read; run the command again", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// 0644: a file of a repository, as an editor makes it. (os.CreateTemp
	// makes a file that only its owner reads.)
	tmpName, err := writeTemp(filepath.Dir(path), data, 0o644)
	if err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// writeTemp writes data to a new temporary file in dir, with the permission
// bits of perm, and returns its name. When it returns an error, the file is
// not there.
func writeTemp(dir string, data []byte, perm fs.FileMode) (string, error) {
	tmp, err := os.CreateTemp(dir, ".agentmoat-patch-*")
	if err != nil {
		return "", err
	}
	name := tmp.Name()
	fail := func(err error) (string, error) {
		_ = tmp.Close()
		_ = os.Remove(name)
		return "", err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}
