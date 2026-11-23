// Package agentmoat: the workloads to check for `verify -f`.
//
// `verify --plan` checks the steps of a plan. A change that came through the
// repository has no plan: the manifest files say which workloads must run in
// the sandbox. This file reads those files (with the code of `scan -f`, so
// one document that does not parse stops the run) and makes the list of
// workloads to check: each workload whose pod spec asks for the
// RuntimeClass. The live state then comes from the cluster, in pkg/verifier.
//
// # The rule that decides each case in this file
//
// Exit code 4 from `verify --wait` is a rollback signal (docs/exit-codes.md).
// So a result that is not ok must come from evidence in the cluster, and
// never from input that verify cannot read as the cluster has it. Each case
// where the files do not name what is in the cluster ends in one of two
// ways, and neither is exit code 4:
//
//   - The run is refused before the cluster is read (an error, exit code 1):
//     input that is not rendered, a workload with no namespace, no workload
//     that asks for the class.
//   - The one workload is listed as not checked, and the others are checked:
//     an object with generateName and no name, and a kind that the verifier
//     has no reader for and that has no selector.
//
// pkg/verifier adds two cases of "not checked" that only the cluster can
// show: a selector that selects no pods, and a Job or a bare Pod that is
// not in the cluster (it can be gone after a deploy that worked: a time to
// live, a hook that its tool deletes).
package agentmoat

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/patcher"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	"github.com/0hardik1/agentmoat/pkg/verifier"
)

// ErrNothingToVerify is wrapped by the error of a verify from manifests when
// no workload of the input asks for the RuntimeClass.
//
// `verify --plan` with a plan that has no steps is exit code 0 with a note:
// the second run of a migration pipeline gets there, and it is correct.
// Files are different. After a move through the repository the files always
// have the class, so "no workload asks for it" nearly always means the wrong
// input (the base and not the overlay, the branch before the merge). A
// verify that looked at nothing must not be a pass there.
var ErrNothingToVerify = errors.New("there is nothing to verify")

// liveKinds are the kinds that the verifier reads from the cluster as
// objects. A workload from a manifest has one of these as its kind, or
// "Kind.group" for each other kind with a pod template.
var liveKinds = map[string]bool{
	"Pod": true, "Deployment": true, "StatefulSet": true,
	"DaemonSet": true, "Job": true, "CronJob": true,
}

// verifyInput is what `verify -f` checks, made from the manifest input.
type verifyInput struct {
	// className is the RuntimeClass that the verify looks for.
	className string

	// targets are the workloads to check in the cluster. Not nil.
	targets []verifier.Target

	// notChecked are the workloads that ask for the class and that verify
	// cannot find in the cluster from what the file says.
	notChecked []schema.VerifyNotChecked

	// excluded is the number of workloads that do not ask for the class.
	excluded int

	// source describes the input, for the report.
	source *schema.ReportSource
}

// readVerifyTargets reads the manifests of opts and returns the workloads to
// check. It reads no cluster. Each error here is a refusal before the first
// API call.
func readVerifyTargets(opts VerifyOptions, stderr io.Writer) (*verifyInput, error) {
	className := opts.RuntimeClassName
	if className == "" {
		className = schema.DefaultRuntimeClassName
	}
	if err := patcher.ValidClassName(className); err != nil {
		return nil, err
	}

	scanOpts := ScanOptions{
		ManifestPaths: opts.ManifestPaths,
		Stdin:         opts.Stdin,
		LabelSelector: opts.LabelSelector,
	}
	if opts.Namespace != "" {
		scanOpts.Namespaces = []string{opts.Namespace}
	}
	set, err := readManifestSet(scanOpts, stderr)
	if err != nil {
		return nil, err
	}

	// A name in input that is not rendered is not the name in the cluster:
	// an overlay adds a prefix and a namespace. A read by such a name gives
	// "not found", and that would be exit code 4 for a deploy that is fine.
	if signs := set.source.Unrendered; len(signs) > 0 {
		return nil, fmt.Errorf("verify -f cannot answer: %w (%s). The names in these files are not always the names in the cluster. "+
			"Give the rendered objects: kustomize build <dir> | agentmoat verify -f -", ErrUnrenderedInput, summarizeSigns(signs))
	}

	in := &verifyInput{className: className, targets: []verifier.Target{}, source: set.source}
	if noNamespace := in.collect(set.found); len(noNamespace) > 0 {
		return nil, noNamespaceError(noNamespace)
	}
	if len(in.targets) == 0 && len(in.notChecked) == 0 {
		return nil, fmt.Errorf("verify -f: %w: no workload in %s asks for RuntimeClass %q (%d workload(s) read). "+
			"Give the files that were deployed, after the change that adds runtimeClassName. "+
			"For another class name, use --runtime-class",
			ErrNothingToVerify, describePaths(opts.ManifestPaths), className, len(set.found.Workloads))
	}
	return in, nil
}

// collect sorts the workloads of the input into the three lists of in: the
// targets, the workloads that are not checked, and the count of the ones
// that do not ask for the class. It returns the workloads that ask for the
// class and have no namespace ("Kind name"); the caller refuses the run for
// those.
func (in *verifyInput) collect(found *scanner.ManifestResult) (noNamespace []string) {
	seen := map[schema.WorkloadRef]bool{}
	for i, w := range found.Workloads {
		if w.PodSpec.RuntimeClassName == nil || *w.PodSpec.RuntimeClassName != in.className {
			in.excluded++
			continue
		}
		ref := schema.WorkloadRef{Kind: w.Kind, Namespace: w.Namespace, Name: w.Name}
		// One identity gets one result, also when the input defines it two
		// times (source.duplicates lists those).
		if seen[ref] {
			continue
		}
		seen[ref] = true

		written := found.Written[i]
		origin := originOf(found.Origins[i])
		if reason := unverifiable(w.Kind, written); reason != "" {
			in.notChecked = append(in.notChecked, schema.VerifyNotChecked{Target: ref, Reason: reason, Origin: origin})
			continue
		}
		if w.Namespace == "" {
			noNamespace = append(noNamespace, w.Kind+" "+w.Name)
			continue
		}
		target := verifier.Target{Ref: ref, Expected: in.className, Origin: origin}
		if !liveKinds[w.Kind] {
			target.Selector = written.Selector
		}
		in.targets = append(in.targets, target)
	}
	return noNamespace
}

// unverifiable returns the reason why verify cannot find the workload in the
// cluster from what its manifest says, or "" when it can.
func unverifiable(kind string, written scanner.ManifestObject) string {
	if written.Name == "" {
		// The API server makes the name, so the file does not have it. A
		// read by the prefix would be "not found".
		return "the object has generateName and no metadata.name, so its name in the cluster is not in the file. " +
			"Look at the runtimeClassName of its pods"
	}
	if liveKinds[kind] {
		return ""
	}
	if !hasSelectorTerms(written.Selector) {
		// A selector with no terms selects each pod of the namespace, so it
		// is the same as no selector here.
		return fmt.Sprintf("verify has no reader for the kind %s, and the object has no spec.selector that selects its pods by label. "+
			"Look at the runtimeClassName of its pods", kind)
	}
	return ""
}

// hasSelectorTerms reports whether the selector has one term or more.
func hasSelectorTerms(sel *metav1.LabelSelector) bool {
	return sel != nil && (len(sel.MatchLabels) > 0 || len(sel.MatchExpressions) > 0)
}

// noNamespaceError is the refusal for workloads that name no namespace when
// the caller gave no default. A guess ("default") could read the wrong
// namespace, and "not found" there would be exit code 4.
func noNamespaceError(workloads []string) error {
	sort.Strings(workloads)
	shown := workloads
	const limit = 5
	more := ""
	if len(shown) > limit {
		more = fmt.Sprintf(", and %d more", len(shown)-limit)
		shown = shown[:limit]
	}
	return fmt.Errorf("verify -f: %d workload(s) that ask for the RuntimeClass name no namespace (%s%s), "+
		"so verify does not know where they are in the cluster. Pass -n <namespace>: the namespace that they were deployed to",
		len(workloads), strings.Join(shown, ", "), more)
}

// stamp puts what only the manifest input knows on the report of the
// verifier: the source, the class, the workloads that were not looked at,
// and the workloads that could not be found from the files. The not-checked
// entries of the verifier (a selector with no pods) come after the ones of
// this file.
func (in *verifyInput) stamp(report *schema.VerifyReport) {
	report.Metadata.Source = in.source
	report.Metadata.RuntimeClassName = in.className
	report.Spec.Summary.Excluded = in.excluded
	report.Spec.NotChecked = append(append([]schema.VerifyNotChecked(nil), in.notChecked...), report.Spec.NotChecked...)
	report.Spec.Summary.NotChecked = len(report.Spec.NotChecked)
}
