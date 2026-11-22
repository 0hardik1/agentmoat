// Package scanner: workloads from manifests, with no cluster.
//
// Enumerate (scanner.go) reads workloads from a live API server. FromManifests
// reads them from YAML or JSON text: files in a repository, the output of
// `helm template` or `kustomize build`, or `kubectl get -o yaml`. Both return
// the same []Workload, so the classifier gives the same verdict for the same
// pod spec whichever way it arrived. This is what lets a CI job check a pull
// request before anything reaches a cluster.
//
// FromManifests does no I/O. It takes (name, bytes) pairs, so files, stdin,
// and manifests embedded in the binary all go through this one function.
// Reading paths from disk is manifest_files.go.
//
// What it accepts
//
//   - Multi-document YAML (`---` separators), a JSON object, a JSON array, or
//     a stream of JSON values.
//   - `kind: List`, typed lists (DeploymentList), and top-level arrays. These
//     are expanded.
//   - The six workload kinds the cluster scan knows (Pod, Deployment,
//     StatefulSet, DaemonSet, Job, CronJob), at any API version. Old charts
//     still say apps/v1beta2 or extensions/v1beta1; the pod template has the
//     same shape there, so they decode into the v1 types.
//   - Any other kind that carries a pod template at spec.template (Argo
//     Rollout, ReplicaSet, OpenKruise CloneSet, Knative Service) or at
//     spec.jobTemplate.spec.template. Its Kind in the result is the
//     "Kind.group" text, for example "Rollout.argoproj.io". The cluster scan
//     does not list these kinds; a file has them right there, and a Rollout
//     that passes a pipeline with no check would defeat the point.
//
// What it does not do
//
//   - It is not a schema validator. Decoding is lenient: an unknown or
//     misspelled field (`hostNetwrok: true`) is ignored, exactly as the Go
//     types ignore it, so the workload is classified without it.
//   - It does not render templates. A file with `{{ ... }}` is an error with
//     a hint to render it first.
//   - It does not apply overlays. Each object is read as a complete object.
//     A Kustomize patch file is a second, partial `Deployment web` to this
//     decoder, not a change to the first one. Such an object (a workload
//     kind with no containers) is never returned as a workload: it would be
//     "compatible" only because it is empty. It goes in
//     ManifestResult.Partial, and a Kustomization object is named in
//     ManifestResult.Kustomizations, so a caller can tell the user that the
//     input is not what gets deployed. `kustomize build <dir>` gives the
//     rendered objects.
//   - It cannot see what admission adds in the cluster (an injected sidecar).
//
// Everything that is not a workload (Service, ConfigMap, a CRD with no pod
// template) is skipped and counted in ManifestResult.Skipped, so a caller
// can say what was not classified instead of staying silent about it.
package scanner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// ManifestInput is one named chunk of manifest text. Name is what error
// messages and ManifestOrigin.File show: a file path, "<stdin>", or a name
// the caller made up for embedded data.
type ManifestInput struct {
	Name string
	Data []byte
}

// ManifestOptions shapes FromManifests. The zero value means "no default
// namespace, no label filter".
type ManifestOptions struct {
	// DefaultNamespace is given to every object that has no
	// metadata.namespace, the way `kubectl apply -n` does it. It is not a
	// filter: an object that names another namespace keeps it. Empty means
	// leave the namespace empty.
	DefaultNamespace string

	// LabelSelector is a Kubernetes label selector matched against each
	// workload's own metadata.labels (the controller's labels, not the pod
	// template's). This is what the cluster scan's selector matches too.
	LabelSelector string
}

// ManifestOrigin says where in the input a workload came from.
type ManifestOrigin struct {
	// File is the ManifestInput.Name.
	File string

	// Document is the 1-based index of the YAML document (or JSON value)
	// inside the input. Every item of a List shares its document's index.
	Document int

	// Line is the 1-based line of the document's first content line (the
	// first line that is not blank and not a comment).
	Line int

	// TemplateSource is the path in the `# Source: <path>` comment that
	// `helm template` writes above each rendered document, or "" when the
	// document has none. For rendered output it is the only link back to a
	// file in the repository.
	TemplateSource string
}

// ManifestResult is what FromManifests found.
type ManifestResult struct {
	// Workloads is sorted by (namespace, kind, name), then by origin, so
	// the order does not depend on the order of the inputs.
	Workloads []Workload

	// Origins is parallel to Workloads: Origins[i] is where Workloads[i]
	// came from.
	Origins []ManifestOrigin

	// Objects is the number of Kubernetes objects read, after lists were
	// expanded. Zero means the input held no object at all.
	Objects int

	// Skipped counts the objects that are not workloads, keyed by
	// "Kind.group" ("Service", "Ingress.networking.k8s.io"). Objects with
	// no kind are under NoKindKey.
	Skipped map[string]int

	// OwnedDropped counts workloads left out because their controller is
	// also in the input (a Pod whose ReplicaSet is there too). They would
	// be the same pod spec twice.
	OwnedDropped int

	// Filtered counts workloads left out by ManifestOptions.LabelSelector.
	Filtered int

	// NoNamespace counts workloads in the result whose namespace is empty
	// (none in the manifest and no DefaultNamespace).
	NoNamespace int

	// Duplicates describes each workload identity that occurs more than
	// once, with the places it occurs. Both copies stay in Workloads: two
	// overlays that define the same Deployment differently are two things
	// to check.
	Duplicates []string

	// Partial lists the objects that have a workload kind but no
	// containers, sorted like Workloads. They are not in Workloads and not
	// in Skipped. See ManifestPartial.
	Partial []ManifestPartial

	// Kustomizations names each input that holds a Kustomize Kustomization
	// or Component object (sorted, no repeats). One of these in the input
	// means the other files are bases and patches, not the rendered result.
	Kustomizations []string
}

// ManifestPartial is an object of a workload kind that has no containers.
//
// A pod needs one container, so this is not a complete object. In practice
// it is an overlay patch: a Kustomize strategic-merge patch names the kind
// and the object and carries only the fields it changes. The decoder cannot
// say what the merged object looks like, so it does not guess:
//
//   - Read as a workload, the patch would have no containers, no rule would
//     fire, and it would be reported compatible.
//   - Worse, a patch that only sets runtimeClassName would look like a
//     sandboxed, compatible workload while the base it patches (the object
//     that really runs) is neither checked as sandboxed nor, perhaps,
//     compatible.
//
// For the six known kinds, any object with no containers is partial. For
// other kinds only a pod template that sets runtimeClassName is: many custom
// resources have a spec.template that is not a pod template at all, and
// those must stay "not a workload".
type ManifestPartial struct {
	Kind      string
	Namespace string
	Name      string // may be empty: a patch can select its target another way
	Origin    ManifestOrigin

	// RuntimeClassName is the runtimeClassName the pod template sets, or "".
	RuntimeClassName string
}

// NoKindKey is the ManifestResult.Skipped key for objects that have no
// `kind` (a Helm Chart.yaml, a values file named on the command line).
const NoKindKey = "(no kind)"

// ManifestProblem is one input that could not be read as a manifest.
type ManifestProblem struct {
	File     string
	Document int
	Line     int

	// Kind and Name identify the object when the problem was found after
	// its header was read. Both are empty for a document that did not
	// parse at all.
	Kind string
	Name string

	Err error

	// Templated is true when the document contains "{{", which is the
	// usual reason a chart file does not parse: it was not rendered.
	Templated bool
}

// ManifestError is the error FromManifests returns when any input is
// unreadable. It carries every problem, not only the first, so a user fixes
// a directory in one pass.
//
// An unreadable document is an error and not a warning on purpose. Exit 0
// from a scan means "nothing here is incompatible"; that cannot be said
// about a document that was not read.
type ManifestError struct {
	Problems []ManifestProblem
}

func (e *ManifestError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d manifest problem(s):", len(e.Problems))
	templated := false
	for _, p := range e.Problems {
		fmt.Fprintf(&b, "\n  %s:%d: ", p.File, p.Line)
		if p.Kind != "" || p.Name != "" {
			fmt.Fprintf(&b, "%s %q: ", p.Kind, p.Name)
		}
		b.WriteString(p.Err.Error())
		templated = templated || p.Templated
	}
	if templated {
		b.WriteString("\n  hint: the text contains {{ }} template syntax. Render the chart first: helm template <chart> | agentmoat scan -f -")
	}
	return b.String()
}

// FromManifests decodes every input and returns the workloads in them.
//
// The error is a *ManifestError when one or more documents are unreadable,
// or a plain error for a bad LabelSelector. In both cases the result is nil.
func FromManifests(inputs []ManifestInput, opts ManifestOptions) (*ManifestResult, error) {
	selector, err := labels.Parse(opts.LabelSelector)
	if err != nil {
		return nil, fmt.Errorf("label selector %q: %w", opts.LabelSelector, err)
	}

	c := &manifestCollector{
		defaultNamespace: opts.DefaultNamespace,
		skipped:          map[string]int{},
		present:          map[string]struct{}{},
		kustomizations:   map[string]struct{}{},
	}
	for _, in := range inputs {
		c.readInput(in)
	}
	if len(c.problems) > 0 {
		return nil, &ManifestError{Problems: c.problems}
	}
	return c.result(selector), nil
}

// ---------------------------------------------------------------------------
// Collector: one pass over every input, then one pass to assemble.
// ---------------------------------------------------------------------------

// foundWorkload is a decoded workload plus what the assemble pass needs.
type foundWorkload struct {
	workload Workload
	origin   ManifestOrigin
	owners   []metav1.OwnerReference
}

type manifestCollector struct {
	defaultNamespace string

	found    []foundWorkload
	partial  []ManifestPartial
	objects  int
	skipped  map[string]int
	problems []ManifestProblem

	// kustomizations is the set of input names that hold a Kustomization.
	kustomizations map[string]struct{}

	// present holds the identity of every named object in the input,
	// workload or not. The assemble pass uses it to answer "is this
	// workload's controller also here?".
	present map[string]struct{}
}

// identityKey is the lookup key for present. The API group is left out: an
// ownerReference names its owner by kind, and two kinds with the same name
// in different groups owning the same-named object in one namespace is not
// a case worth a more complex key.
func identityKey(kind, namespace, name string) string {
	return kind + "\x00" + namespace + "\x00" + name
}

func (c *manifestCollector) problem(origin ManifestOrigin, kind, name string, templated bool, err error) {
	c.problems = append(c.problems, ManifestProblem{
		File: origin.File, Document: origin.Document, Line: origin.Line,
		Kind: kind, Name: name, Err: err, Templated: templated,
	})
}

// readInput splits one input into documents and reads each.
func (c *manifestCollector) readInput(in ManifestInput) {
	data := bytes.TrimPrefix(in.Data, utf8BOM)

	// Text that starts with { or [ is almost always JSON: one value, or a
	// stream of values (`jq -c '.items[]'`). YAML's `---` splitting does not
	// apply to it. "Almost": flow-style YAML ({kind: Pod}) and an unrendered
	// Helm template ({{- if ... }}) start the same way, so when the first
	// value is not JSON the text goes to the YAML path below.
	if first := firstNonSpace(data); first == '{' || first == '[' {
		if c.readJSONStream(in.Name, data) {
			return
		}
	}

	docs, badSeparators := splitYAMLDocuments(data)
	for _, line := range badSeparators {
		c.problem(ManifestOrigin{File: in.Name, Line: line}, "", "", false,
			errors.New(`a line that starts with "---" is a document separator and can hold nothing else but a comment`))
	}
	for _, doc := range docs {
		origin := ManifestOrigin{File: in.Name, Document: doc.index, Line: doc.line, TemplateSource: doc.templateSource}
		raw, err := yaml.YAMLToJSON(doc.data)
		if err != nil {
			// Point at the line the parser names, not at the top of the
			// document: in a 300-line rendered chart that is the difference
			// between a useful CI annotation and a useless one.
			line, cleaned := cleanYAMLError(err, doc.firstLine)
			if line != 0 {
				origin.Line = line
			}
			c.problem(origin, "", "", bytes.Contains(doc.data, []byte("{{")), cleaned)
			continue
		}
		c.readValue(raw, origin, schema.GroupKind{})
	}
}

// readJSONStream reads one or more concatenated JSON values. It returns
// false, and records nothing, when the first value is not JSON; the caller
// then reads the text as YAML. A syntax error after the first value is a
// problem in a JSON stream and is recorded as one.
func (c *manifestCollector) readJSONStream(name string, data []byte) (isJSON bool) {
	dec := json.NewDecoder(bytes.NewReader(data))
	for index := 1; ; index++ {
		start := int(dec.InputOffset())
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return true
		}
		origin := ManifestOrigin{File: name, Document: index, Line: lineAt(data, start)}
		if err != nil {
			if index == 1 {
				return false
			}
			c.problem(origin, "", "", false, err)
			return true // the decoder cannot resync after a syntax error
		}
		c.readValue(raw, origin, schema.GroupKind{})
	}
}

// objectHeader is the part of any Kubernetes object that this file reads
// before it knows the kind. Items is present on List kinds only.
type objectHeader struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
	Items      []json.RawMessage `json:"items"`
}

// readValue reads one JSON value: null (an empty document), an array of
// objects, a List, or a single object. inherited is the GroupKind a typed
// list (DeploymentList) gives to items that omit their own kind.
func (c *manifestCollector) readValue(raw json.RawMessage, origin ManifestOrigin, inherited schema.GroupKind) {
	switch firstNonSpace(raw) {
	case 0, 'n': // nothing, or null: a document of comments only
		return
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			c.problem(origin, "", "", false, err)
			return
		}
		for _, item := range items {
			c.readValue(item, origin, inherited)
		}
		return
	case '{':
		// handled below
	default:
		// A scalar document ("just a string"). Not a Kubernetes object.
		c.objects++
		c.skipped[NoKindKey]++
		return
	}

	var header objectHeader
	if err := json.Unmarshal(raw, &header); err != nil {
		c.problem(origin, "", "", false, fmt.Errorf("reading apiVersion, kind and metadata: %w", cleanJSONError(err)))
		return
	}

	gk := groupKindOf(header.APIVersion, header.Kind)
	if header.Kind == "" {
		gk = inherited
	}

	// A List: `kind: List`, or a typed list such as DeploymentList whose
	// items usually omit kind and apiVersion.
	if strings.HasSuffix(header.Kind, "List") && header.Items != nil {
		itemKind := schema.GroupKind{Group: gk.Group, Kind: strings.TrimSuffix(header.Kind, "List")}
		for _, item := range header.Items {
			c.readValue(item, origin, itemKind)
		}
		return
	}

	c.readObject(raw, header, gk, origin)
}

// readObject handles one non-list object whose header is already decoded.
func (c *manifestCollector) readObject(raw json.RawMessage, header objectHeader, gk schema.GroupKind, origin ManifestOrigin) {
	c.objects++
	if gk.Kind == "" {
		c.skipped[NoKindKey]++
		return
	}

	name := header.Metadata.Name
	if name == "" {
		// `kubectl create` accepts generateName alone (Jobs often use it).
		name = header.Metadata.GenerateName
	}
	namespace := header.Metadata.Namespace
	if namespace == "" {
		namespace = c.defaultNamespace
	}
	if name != "" {
		c.present[identityKey(gk.Kind, namespace, name)] = struct{}{}
	}

	if gk.Group == kustomizeGroup {
		c.kustomizations[origin.File] = struct{}{}
	}

	workload, shape, err := decodeWorkload(raw, gk)
	if err != nil {
		c.problem(origin, gk.Kind, name, false, cleanJSONError(err))
		return
	}
	switch shape {
	case shapeOther:
		c.skipped[gk.String()]++
		return
	case shapePartial:
		runtimeClass := ""
		if rc := workload.PodSpec.RuntimeClassName; rc != nil {
			runtimeClass = *rc
		}
		c.partial = append(c.partial, ManifestPartial{
			Kind: workload.Kind, Namespace: namespace, Name: name,
			Origin: origin, RuntimeClassName: runtimeClass,
		})
		return
	case shapeWorkload:
		// handled below
	}
	if name == "" {
		c.problem(origin, gk.Kind, "", false, errors.New("metadata.name is missing"))
		return
	}

	workload.Name = name
	workload.Namespace = namespace
	c.found = append(c.found, foundWorkload{
		workload: workload,
		origin:   origin,
		owners:   header.Metadata.OwnerReferences,
	})
}

// result is the assemble pass: drop workloads whose controller is in the
// input, apply the selector, sort, and note duplicates.
func (c *manifestCollector) result(selector labels.Selector) *ManifestResult {
	res := &ManifestResult{Objects: c.objects, Skipped: c.skipped}

	kept := make([]foundWorkload, 0, len(c.found))
	for _, f := range c.found {
		if c.controllerPresent(f) {
			res.OwnedDropped++
			continue
		}
		if !selector.Matches(labels.Set(f.workload.Labels)) {
			res.Filtered++
			continue
		}
		kept = append(kept, f)
	}

	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.workload.Namespace != b.workload.Namespace {
			return a.workload.Namespace < b.workload.Namespace
		}
		if a.workload.Kind != b.workload.Kind {
			return a.workload.Kind < b.workload.Kind
		}
		if a.workload.Name != b.workload.Name {
			return a.workload.Name < b.workload.Name
		}
		if a.origin.File != b.origin.File {
			return a.origin.File < b.origin.File
		}
		return a.origin.Document < b.origin.Document
	})

	res.Workloads = make([]Workload, 0, len(kept))
	res.Origins = make([]ManifestOrigin, 0, len(kept))
	for _, f := range kept {
		res.Workloads = append(res.Workloads, f.workload)
		res.Origins = append(res.Origins, f.origin)
		if f.workload.Namespace == "" {
			res.NoNamespace++
		}
	}
	res.Duplicates = describeDuplicates(kept)

	sort.SliceStable(c.partial, func(i, j int) bool {
		a, b := c.partial[i], c.partial[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Origin.File != b.Origin.File {
			return a.Origin.File < b.Origin.File
		}
		return a.Origin.Document < b.Origin.Document
	})
	res.Partial = c.partial

	for file := range c.kustomizations {
		res.Kustomizations = append(res.Kustomizations, file)
	}
	sort.Strings(res.Kustomizations)
	return res
}

// controllerPresent reports whether a workload's controller owner is itself
// an object in the input.
//
// The cluster scan drops every Pod that has a ReplicaSet, StatefulSet,
// DaemonSet or Job controller, because there the controller is always
// listed too. A file gives no such promise: `kubectl get pods -o yaml`
// holds Pods whose ReplicaSets are not in the text, and dropping those
// would leave nothing to scan. So a workload is dropped only when its owner
// is really there.
func (c *manifestCollector) controllerPresent(f foundWorkload) bool {
	for _, o := range f.owners {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		if _, ok := c.present[identityKey(o.Kind, f.workload.Namespace, o.Name)]; ok {
			return true
		}
	}
	return false
}

// describeDuplicates returns one line for each workload identity that
// occurs more than once in the (sorted) list.
func describeDuplicates(sorted []foundWorkload) []string {
	var out []string
	for i := 0; i < len(sorted); {
		j := i + 1
		for j < len(sorted) && sameIdentity(sorted[i].workload, sorted[j].workload) {
			j++
		}
		if j-i > 1 {
			places := make([]string, 0, j-i)
			for _, f := range sorted[i:j] {
				places = append(places, fmt.Sprintf("%s:%d", f.origin.File, f.origin.Line))
			}
			w := sorted[i].workload
			out = append(out, fmt.Sprintf("%s %s/%s is defined %d times (%s)",
				w.Kind, displayNamespace(w.Namespace), w.Name, j-i, strings.Join(places, ", ")))
		}
		i = j
	}
	return out
}

func sameIdentity(a, b Workload) bool {
	return a.Kind == b.Kind && a.Namespace == b.Namespace && a.Name == b.Name
}

func displayNamespace(ns string) string {
	if ns == "" {
		return "-"
	}
	return ns
}

// ---------------------------------------------------------------------------
// Decoding one object into a Workload.
// ---------------------------------------------------------------------------

// workloadDecoder turns the JSON of one object into a Workload. Name and
// Namespace are set by the caller (it applies the generateName and default
// namespace rules), so a decoder only has to get the pod spec, the labels,
// and the Kind right.
type workloadDecoder func(raw []byte) (Workload, error)

// typed builds a decoder for one of the six kinds the cluster scan knows.
// It reuses the cluster scan's own converter, so the two paths cannot give
// different Workloads for the same object.
func typed[T any](convert func(*T) Workload) workloadDecoder {
	return func(raw []byte) (Workload, error) {
		var obj T
		if err := json.Unmarshal(raw, &obj); err != nil {
			return Workload{}, err
		}
		return convert(&obj), nil
	}
}

// typedDecoders maps (group, kind) to a decoder. The API version is not
// part of the key: every version of these kinds keeps the pod template at
// the same path with the same shape, and a chart that still says
// apps/v1beta2 must be scanned, not skipped. `extensions` is where
// Deployment and DaemonSet lived before `apps`.
//
// The group IS part of the key: `kind: Job` in batch.volcano.sh is a
// different object with a different shape, and must not be decoded as a
// batch/v1 Job with an empty pod spec.
var typedDecoders = map[schema.GroupKind]workloadDecoder{
	{Group: "", Kind: "Pod"}:                  typed(workloadFromPod),
	{Group: "apps", Kind: "Deployment"}:       typed(workloadFromDeployment),
	{Group: "extensions", Kind: "Deployment"}: typed(workloadFromDeployment),
	{Group: "apps", Kind: "StatefulSet"}:      typed(workloadFromStatefulSet),
	{Group: "apps", Kind: "DaemonSet"}:        typed(workloadFromDaemonSet),
	{Group: "extensions", Kind: "DaemonSet"}:  typed(workloadFromDaemonSet),
	{Group: "batch", Kind: "Job"}:             typed(workloadFromJob),
	{Group: "batch", Kind: "CronJob"}:         typed(workloadFromCronJob),
}

// kustomizeGroup is the API group of the Kustomization and Component objects
// in a kustomization.yaml. (Flux's Kustomization is a different object in
// kustomize.toolkit.fluxcd.io: a custom resource that is applied to a
// cluster, not a build file.)
const kustomizeGroup = "kustomize.config.k8s.io"

// objectShape is what decodeWorkload found in one object.
type objectShape int

const (
	// shapeOther: no pod template. A Service, a ConfigMap, most custom
	// resources.
	shapeOther objectShape = iota

	// shapeWorkload: a pod template with one container or more.
	shapeWorkload

	// shapePartial: a workload kind with no containers. See
	// ManifestPartial.
	shapePartial
)

// decodeWorkload decodes one object and says what shape it has. The
// Workload is filled for shapeWorkload and shapePartial (for the second,
// only its Kind and what there is of the pod spec are of use).
func decodeWorkload(raw []byte, gk schema.GroupKind) (Workload, objectShape, error) {
	if decode := typedDecoders[gk]; decode != nil {
		w, err := decode(raw)
		if err != nil {
			return Workload{}, shapeOther, err
		}
		if len(w.PodSpec.Containers) == 0 {
			return w, shapePartial, nil
		}
		return w, shapeWorkload, nil
	}
	w, shape := workloadFromPodTemplate(raw, gk)
	return w, shape, nil
}

// podTemplateProbe finds a pod template in an object of a kind this package
// has no Go type for.
type podTemplateProbe struct {
	Metadata metav1.ObjectMeta `json:"metadata"`
	Spec     struct {
		Template    *corev1.PodTemplateSpec `json:"template"`
		JobTemplate *struct {
			Spec struct {
				Template *corev1.PodTemplateSpec `json:"template"`
			} `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
}

// workloadFromPodTemplate handles every kind that is not in typedDecoders
// but has a pod template at spec.template (Argo Rollout, ReplicaSet,
// ReplicationController, OpenKruise CloneSet, Knative Service) or at
// spec.jobTemplate.spec.template.
//
// "Has a pod template" means: that path decodes as a PodTemplateSpec AND
// lists one container or more. A decode error is not reported as a problem
// here. Plenty of CRDs have a spec.template that is something else (a
// Cluster API MachineTemplate); for those the right answer is "not a
// workload", not "broken manifest".
//
// A template with no containers that sets runtimeClassName is partial: no
// field of that name exists on the look-alike templates, and a patch that
// puts an Argo Rollout in a sandbox has exactly this shape.
func workloadFromPodTemplate(raw []byte, gk schema.GroupKind) (Workload, objectShape) {
	var probe podTemplateProbe
	if err := json.Unmarshal(raw, &probe); err != nil {
		return Workload{}, shapeOther
	}
	template := probe.Spec.Template
	if template == nil && probe.Spec.JobTemplate != nil {
		template = probe.Spec.JobTemplate.Spec.Template
	}
	if template == nil {
		return Workload{}, shapeOther
	}
	spec := template.Spec
	w := Workload{
		Kind:        gk.String(),
		PodSpec:     spec,
		Labels:      copyMap(probe.Metadata.Labels),
		Annotations: copyMap(probe.Metadata.Annotations),
		ImageRefs:   imageRefsOf(spec),
	}
	switch {
	case len(spec.Containers) > 0:
		return w, shapeWorkload
	case spec.RuntimeClassName != nil && *spec.RuntimeClassName != "":
		return w, shapePartial
	default:
		return Workload{}, shapeOther
	}
}

// groupKindOf splits apiVersion ("apps/v1", "v1", "argoproj.io/v1alpha1")
// into its group and pairs it with the kind. A malformed apiVersion yields
// an empty group, which for the six known kinds simply means "not one of
// them" unless the kind is Pod.
func groupKindOf(apiVersion, kind string) schema.GroupKind {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupKind{Kind: kind}
	}
	return schema.GroupKind{Group: gv.Group, Kind: kind}
}

// ---------------------------------------------------------------------------
// Splitting text into documents, with line numbers.
// ---------------------------------------------------------------------------

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// yamlDocument is one `---`-separated chunk of a YAML stream.
type yamlDocument struct {
	data           []byte
	index          int    // 1-based position in the stream
	firstLine      int    // 1-based line of the first line of data
	line           int    // 1-based line of the first content line
	templateSource string // from a leading "# Source: <path>" comment
}

// helmSourcePrefix is the comment `helm template` writes above each
// rendered document.
const helmSourcePrefix = "# Source: "

// splitYAMLDocuments splits a YAML stream on document separators and
// records where each document starts. A document with no content (blank
// lines and comments only) is left out.
//
// k8s.io/apimachinery has a YAML document reader, but it does not report
// line numbers, and a CI annotation needs one. The separator rule here is
// the same one that reader and kubectl use: a line that starts with "---"
// and has nothing else on it (a trailing comment is allowed).
//
// A line that starts with "---" and has other text ("--- !tag", "----") is
// returned in badSeparators, and the caller reports it. kubectl refuses such
// a line too. Passing it on as content would be worse than refusing it: the
// YAML parser reads only the first document of what it is given, so anything
// after a "--- {...}" line would be dropped with no message, and a workload
// that is dropped with no message is a check that silently passed.
func splitYAMLDocuments(data []byte) (docs []yamlDocument, badSeparators []int) {
	var (
		current yamlDocument
		buf     bytes.Buffer
		index   = 1
		lineNo  = 0
	)
	flush := func() {
		if current.line != 0 { // the document has a content line
			current.data = append([]byte(nil), buf.Bytes()...)
			current.index = index
			docs = append(docs, current)
		}
		buf.Reset()
		current = yamlDocument{}
		index++
	}

	for len(data) > 0 {
		var line []byte
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			line, data = data[:i+1], data[i+1:]
		} else {
			line, data = data, nil
		}
		lineNo++

		text := strings.TrimRight(string(line), "\r\n")
		if strings.HasPrefix(text, "---") {
			if !isYAMLSeparator(text) {
				badSeparators = append(badSeparators, lineNo)
			}
			flush()
			continue
		}
		if current.firstLine == 0 {
			current.firstLine = lineNo
		}
		buf.Write(line)

		trimmed := strings.TrimSpace(text)
		switch {
		case trimmed == "":
			// blank
		case strings.HasPrefix(trimmed, "#"):
			if current.line == 0 && strings.HasPrefix(trimmed, helmSourcePrefix) {
				current.templateSource = strings.TrimSpace(strings.TrimPrefix(trimmed, helmSourcePrefix))
			}
		default:
			if current.line == 0 {
				current.line = lineNo
			}
		}
	}
	flush()
	return docs, badSeparators
}

// isYAMLSeparator reports whether a line that starts with "---" (and has no
// line ending) is a well-formed document separator: "---", optionally
// followed by spaces and a comment.
func isYAMLSeparator(line string) bool {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "---"))
	return rest == "" || strings.HasPrefix(rest, "#")
}

// firstNonSpace returns the first byte that is not JSON/YAML whitespace, or
// 0 when there is none.
func firstNonSpace(data []byte) byte {
	for _, b := range data {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b
		}
	}
	return 0
}

// lineAt returns the 1-based line of the first non-space byte at or after
// offset.
func lineAt(data []byte, offset int) int {
	for offset < len(data) {
		switch data[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
			continue
		}
		break
	}
	if offset > len(data) {
		offset = len(data)
	}
	return 1 + bytes.Count(data[:offset], []byte{'\n'})
}

// yamlErrorLine matches the position at the start of a YAML syntax error:
// "yaml: line 3: did not find expected key".
var yamlErrorLine = regexp.MustCompile(`^yaml: line (\d+): `)

// cleanYAMLError makes the YAML library's error text fit for a user.
//
// The library prefixes every message with "error converting YAML to JSON: ",
// which describes its internals, not the user's file. And the line it names
// counts from the start of the document it was given, which is not the line
// in a multi-document file. This function drops the prefix and converts the
// line: firstLine is the file line of the document's first line.
//
// It returns line 0 when the message names no line; the caller then keeps
// the line of the document.
func cleanYAMLError(err error, firstLine int) (line int, cleaned error) {
	text := strings.TrimPrefix(err.Error(), "error converting YAML to JSON: ")
	if m := yamlErrorLine.FindStringSubmatch(text); m != nil {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil && n > 0 {
			return firstLine + n - 1, errors.New(strings.TrimPrefix(text, m[0]))
		}
	}
	// "invalid map key: map[interface {}]interface {}{...}" is what the
	// library says when a key is itself a mapping, and it prints the Go
	// value. The usual cause is `name: {{ .Values.name }}`: to YAML, {{ }}
	// is a flow mapping inside a flow mapping.
	text = strings.TrimPrefix(text, "yaml: ")
	if strings.HasPrefix(text, "invalid map key:") {
		return 0, errors.New("a mapping key is itself a mapping, not a string (YAML reads {{ ... }} that way)")
	}
	return 0, errors.New(text)
}

// cleanJSONError does the same for encoding/json, whose type errors name
// the Go struct ("Go struct field Deployment.spec.replicas of type int32").
// The field path is the useful part; the "Go struct field" wording is not.
func cleanJSONError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) && typeErr.Field != "" {
		return fmt.Errorf("field %s: got a %s, want %s", typeErr.Field, typeErr.Value, typeErr.Type)
	}
	return err
}
