// Package agentmoat: `patch`, the move to the sandbox as a change to files.
//
// `apply` changes the live object. For a team whose source of truth is a
// repository, that is the wrong place: the next deploy from the repository
// does not have the change. `patch` makes the same change where such a team
// makes every change: in the manifest files, where a pull request reviews
// it, the pipeline deploys it, and `git revert` undoes it.
//
// What it does:
//
//  1. Reads the manifests exactly as `scan -f` does (readManifestSet), and
//     classifies each workload with the same rules.
//  2. Selects the workloads that `plan` would select: compatible (or needs
//     review, with IncludeReview), and with no RuntimeClass. The decision
//     is planner.ExclusionCause, so the two commands cannot disagree. The
//     one difference is the kind: a live Pod or Job cannot be patched, and
//     the file of a Pod or a Job can be edited.
//  3. Adds one line, `runtimeClassName: <class>`, to the pod spec of each
//     selected workload (pkg/patcher). Nothing else in a file changes, and
//     the edit proves that before it is used.
//  4. Writes the files, but only with DryRun false. Dry-run is the default,
//     as for apply. The result has the diff of each file in the two cases.
//
// A second run finds the class on the workloads and changes nothing.
//
// Invariants, the same as for a file scan: no Kubernetes client is built
// and no kubeconfig is read (a test keeps this honest), and input that
// cannot be read is an error and not a skip.
package agentmoat

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/patcher"
	"github.com/0hardik1/agentmoat/pkg/planner"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// PatchOptions is the input of Patch.
type PatchOptions struct {
	// ManifestPaths are the files and directories to read and edit, as for
	// `scan -f`. Standard input ("-") is refused: it has no file to edit.
	ManifestPaths []string

	// Namespace is the default for objects that name no namespace. It is
	// not a filter, and it is not written to a file.
	Namespace string

	// LabelSelector limits the patch to the workloads whose own labels
	// match.
	LabelSelector string

	// RulesYAMLPath is a classifier rules override, as for scan.
	RulesYAMLPath string

	// FactsPath is a saved report with cluster facts, as for `scan -f
	// --facts`. Some rules give another verdict with facts (GPU drivers).
	FactsPath string

	// RuntimeClassName is the value to write. Empty means the default
	// ("gvisor").
	RuntimeClassName string

	// IncludeReview also selects the workloads that need review.
	IncludeReview bool

	// DryRun, when true, writes no file. The CLI and the MCP tool default
	// to true.
	DryRun bool

	// Stderr receives the progress lines. Nil means os.Stderr.
	Stderr io.Writer
}

// Patch adds runtimeClassName to the manifest files of the workloads that
// can run in the sandbox. See the file comment.
//
// The context is not used today: the function reads and writes local files
// only. It is in the signature because every verb of this package has one.
func Patch(_ context.Context, opts PatchOptions) (*schema.PatchResult, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	class := sandboxClassName(opts.RuntimeClassName)
	if err := patcher.ValidClassName(class); err != nil {
		return nil, fmt.Errorf("patch: %w", err)
	}
	if len(opts.ManifestPaths) == 0 {
		return nil, fmt.Errorf("patch: no manifest paths: give the files or directories to edit")
	}
	for _, p := range opts.ManifestPaths {
		if p == scanner.StdinPath {
			return nil, fmt.Errorf("patch: standard input (the path \"-\") has no file to edit: give the files or directories")
		}
	}

	scanOpts := ScanOptions{
		ManifestPaths: opts.ManifestPaths,
		LabelSelector: opts.LabelSelector,
		RulesYAMLPath: opts.RulesYAMLPath,
		FactsPath:     opts.FactsPath,
	}
	if opts.Namespace != "" {
		scanOpts.Namespaces = []string{opts.Namespace}
	}
	set, err := readManifestSet(scanOpts, stderr)
	if err != nil {
		// The hint of a scan for a Helm template is "render it, then scan
		// the output". That does not help here: rendered output is not a
		// file of the repository, so there is nothing in it to edit.
		var bad *scanner.ManifestError
		if errors.As(err, &bad) {
			bad.TemplateHint = patchTemplateHint
		}
		return nil, err
	}

	run := &patchRun{
		opts:   opts,
		class:  class,
		set:    set,
		stderr: stderr,
		byFile: map[string][]int{},
	}
	run.selectWorkloads()
	run.editFiles()
	if !opts.DryRun {
		run.writeFiles()
	}
	res := run.result()
	run.describe(res)
	return res, nil
}

// patchTemplateHint is the hint of `patch` for text that has {{ }} in it.
const patchTemplateHint = "the text contains {{ }} template syntax. A Helm template cannot be edited: it is not YAML before it is rendered. " +
	"Set the chart value for runtimeClassName, and give patch only the files that are not templates"

// PatchExitCode is the exit code for a PatchResult (docs/exit-codes.md). It
// is here, and not in cmd/, so that every caller decides in the same way.
//
//	0  every selected workload is patched, or none was selected
//	3  some selected workloads are patched and some are not
//	1  no selected workload could be patched
//
// A dry-run uses the same rule: it says what a real run would do, and a real
// run that cannot edit a selected workload is partial.
func PatchExitCode(res *schema.PatchResult) int {
	s := res.Spec.Summary
	switch {
	case s.NotEdited+s.Failed == 0:
		return 0
	case s.Patched > 0:
		return 3
	default:
		return 1
	}
}

// patchRun is the state of one Patch call.
type patchRun struct {
	opts   PatchOptions
	class  string
	set    *manifestSet
	stderr io.Writer

	changes  []schema.PatchChange
	excluded []schema.ExcludedWorkload

	// targets is parallel to changes: where each selected workload is.
	targets []scanner.ManifestOrigin

	// byFile maps a file to the indexes (in changes) of its workloads.
	byFile map[string][]int

	// files is the edit of each file that has one, by path.
	files map[string]*fileEdit
}

// fileEdit is the edit of one file, in memory.
type fileEdit struct {
	old   []byte
	text  []byte
	added []int
	wrote bool
	err   error
}

// selectWorkloads classifies each workload and puts it in changes or in
// excluded. The order is the order of the scanner: (namespace, kind, name).
func (r *patchRun) selectWorkloads() {
	planOpts := planner.Options{RuntimeClassName: r.class, IncludeReview: r.opts.IncludeReview}
	for i, w := range r.set.found.Workloads {
		origin := r.set.found.Origins[i]
		result := workloadResult(w, classifier.ClassifyWithFacts(w, r.set.registry, r.set.facts))
		ref := schema.WorkloadRef{Kind: result.Kind, Namespace: result.Namespace, Name: result.Name}

		if cause := planner.ExclusionCause(result, planOpts); cause != planner.CauseNone {
			r.excluded = append(r.excluded, schema.ExcludedWorkload{
				Target:        ref,
				Compatibility: result.Compatibility,
				Reason:        patchExclusionReason(cause, result, planOpts),
			})
			continue
		}
		r.byFile[origin.File] = append(r.byFile[origin.File], len(r.changes))
		r.changes = append(r.changes, schema.PatchChange{
			Target:        ref,
			Compatibility: result.Compatibility,
			Origin:        originOf(origin),
		})
		r.targets = append(r.targets, origin)
	}
}

// patchExclusionReason is the sentence for a workload that is not selected.
// It is the sentence of the plan, except for a workload with another
// RuntimeClass: the plan's sentence there is about rollback.
func patchExclusionReason(cause planner.Cause, w schema.WorkloadResult, opts planner.Options) string {
	if cause == planner.CauseHasOtherClass {
		return fmt.Sprintf("has RuntimeClass %q: patch does not replace another RuntimeClass "+
			"(change the value by hand if the workload must move to %q)", w.RuntimeClassName, opts.RuntimeClassName)
	}
	return planner.ExclusionReason(cause, w, opts)
}

// editFiles makes the edit of each file in memory and records the outcome
// of each selected workload. No file is written here.
func (r *patchRun) editFiles() {
	text := make(map[string][]byte, len(r.set.files.Inputs))
	for _, in := range r.set.files.Inputs {
		text[in.Name] = in.Data
	}

	r.files = map[string]*fileEdit{}
	for file, indexes := range r.byFile {
		targets := make([]patcher.Target, 0, len(indexes))
		for _, i := range indexes {
			targets = append(targets, patcher.Target{
				Document: r.targets[i].Document,
				Object:   r.targets[i].Object,
				Kind:     r.changes[i].Target.Kind,
			})
		}
		edit := patcher.AddRuntimeClass(text[file], r.class, targets)
		for n, outcome := range edit.Outcomes {
			change := &r.changes[indexes[n]]
			if outcome.Edited {
				change.Status = schema.PatchStatusPatched
				change.Line = outcome.Line
			} else {
				change.Status = schema.PatchStatusNotEdited
				change.Reason = outcome.Reason
			}
		}
		if len(edit.Added) > 0 {
			r.files[file] = &fileEdit{old: text[file], text: edit.Text, added: edit.Added}
		}
	}
}

// writeFiles writes each edited file. A file that cannot be written does
// not stop the others: its workloads become "failed", with the error.
func (r *patchRun) writeFiles() {
	for file, edit := range r.files {
		if edit.err = writeManifest(file, edit.old, edit.text); edit.err == nil {
			edit.wrote = true
			continue
		}
		for _, i := range r.byFile[file] {
			if r.changes[i].Status == schema.PatchStatusPatched {
				r.changes[i].Status = schema.PatchStatusFailed
				r.changes[i].Line = 0
				r.changes[i].Reason = "the file was not written: " + edit.err.Error()
			}
		}
	}
}

// writeManifest replaces the content of a manifest file.
//
//   - The new content goes to a temporary file in the same directory, and a
//     rename puts it in place. A run that stops in the middle leaves the old
//     file or the new one, never a part of one.
//   - A path that is a symbolic link stays a link: the file that it points
//     to is the one that is replaced.
//   - The file keeps its permission bits.
//   - The file must still have the bytes that were read. If an editor or
//     another tool changed it in the meantime, the edit was made for another
//     text, and it is not written.
func writeManifest(path string, old, data []byte) error {
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(target)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", target)
	}
	current, err := os.ReadFile(target) // #nosec G304 -- the path is a manifest that the user named, and was read before.
	if err != nil {
		return err
	}
	if !bytes.Equal(current, old) {
		return fmt.Errorf("%s changed after it was read; run the command again", target)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".agentmoat-patch-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		return err
	}
	keep = true
	return nil
}

// result assembles the PatchResult.
func (r *patchRun) result() *schema.PatchResult {
	res := schema.NewPatchResult()
	res.Metadata = schema.PatchMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		AgentmoatVersion: Version,
		DryRun:           r.opts.DryRun,
		RuntimeClassName: r.class,
		IncludeReview:    r.opts.IncludeReview,
		Source:           r.set.source,
	}

	paths := make([]string, 0, len(r.files))
	for path := range r.files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	files := make([]schema.PatchFile, 0, len(paths))
	for _, path := range paths {
		edit := r.files[path]
		f := schema.PatchFile{
			Path:    path,
			Added:   len(edit.added),
			Written: edit.wrote,
			Diff:    patcher.UnifiedDiff(path, edit.text, edit.added),
		}
		if edit.err != nil {
			f.Error = edit.err.Error()
		}
		files = append(files, f)
	}

	summary := schema.PatchSummary{
		Total:    len(r.changes) + len(r.excluded),
		Selected: len(r.changes),
		Excluded: len(r.excluded),
		Files:    len(files),
	}
	for _, c := range r.changes {
		switch c.Status {
		case schema.PatchStatusPatched:
			summary.Patched++
		case schema.PatchStatusNotEdited:
			summary.NotEdited++
		case schema.PatchStatusFailed:
			summary.Failed++
		}
	}

	res.Spec = schema.PatchSpec{
		Summary:  summary,
		Changes:  nonNil(r.changes),
		Excluded: nonNil(r.excluded),
		Files:    files,
	}
	return res
}

// nonNil returns an empty slice for a nil one, so that the JSON has [] and
// not null: a reader of the document can loop over the field with no check.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// describe writes the progress lines. The names of files and workloads are
// in the report and not here: stderr is for the counts and for what the
// reader must do next.
func (r *patchRun) describe(res *schema.PatchResult) {
	s := res.Spec.Summary
	_, _ = fmt.Fprintf(r.stderr, "patch: %d workloads in %d manifest input(s): %d selected, %d excluded\n",
		s.Total, len(r.set.files.Inputs), s.Selected, s.Excluded)
	if n := len(r.set.found.Duplicates); n > 0 {
		_, _ = fmt.Fprintf(r.stderr, "warning: %d workload(s) are defined more than one time; the report lists them\n", n)
	}
	if signs := r.set.source.Unrendered; len(signs) > 0 {
		_, _ = fmt.Fprintf(r.stderr, "warning: this input is not rendered (%d sign(s): kustomization files or partial objects). "+
			"The verdicts describe the files and not what gets deployed. After the edit, scan the rendered objects with the gate: "+
			"kustomize build <dir> | agentmoat scan -f - --fail-on sandboxed\n", len(signs))
	}

	lines := 0
	for _, f := range res.Spec.Files {
		lines += f.Added
	}
	switch {
	case s.Selected == 0:
		_, _ = fmt.Fprintln(r.stderr, "patch: nothing to change: no workload is compatible and without a RuntimeClass")
	case r.opts.DryRun:
		_, _ = fmt.Fprintf(r.stderr, "patch: dry-run: %d line(s) would be added to %d file(s). To write them, run again with --dry-run=false\n",
			lines, s.Files)
	default:
		written := 0
		for _, f := range res.Spec.Files {
			if f.Written {
				written++
			}
		}
		_, _ = fmt.Fprintf(r.stderr, "patch: %d of %d file(s) written\n", written, s.Files)
	}
	if n := s.NotEdited; n > 0 {
		_, _ = fmt.Fprintf(r.stderr, "patch: %d selected workload(s) are in a form that is not edited; the report has the reason for each\n", n)
	}
	if n := s.Failed; n > 0 {
		_, _ = fmt.Fprintf(r.stderr, "patch: %d selected workload(s) are in a file that could not be written\n", n)
	}
}
