// Package patcher adds `runtimeClassName` to the pod spec of an object in
// manifest text. It is the edit behind `agentmoat patch`: the move to gVisor
// as a change to the files in a repository, where a pull request can review
// it and `git revert` can undo it.
//
// # One added line, and nothing else
//
// A YAML library can read a file and write it back, but the file that comes
// back is not the file that went in: comments move, quotes change, long
// lines are folded again, and a reviewer gets a diff of two hundred lines for
// a change of one. So this package does not write YAML. It uses a YAML
// parser (go.yaml.in/yaml/v3, which reports a line and a column for each
// node) only to find one place, and then it adds one line of text there:
//
//	spec:
//	  runtimeClassName: gvisor      <- the added line
//	  # a comment that stays with its key
//	  containers:
//
// The line goes directly after the line of the `spec:` key of the pod spec,
// with the indent of the first key of that mapping. That place is always
// inside the mapping, and it does not come between a comment and the key
// that the comment is about.
//
// # What is not edited
//
// The edit is for block-style YAML. A target in one of these forms is
// returned with a reason, and its text is not changed:
//
//   - JSON, and flow-style YAML (`spec: {containers: [...]}`). A line cannot
//     be added to them.
//   - A pod spec that is behind a YAML anchor or alias. The edit would
//     change every object that shares the text.
//   - A pod spec that already has a `runtimeClassName` key, with no name as
//     its value (empty, or null). A second key of the same name is not
//     valid.
//
// # The edit checks itself
//
// A text edit that is found with one parser and read by another can be
// wrong in ways that a test did not think of. So AddRuntimeClass proves its
// result before it returns it (check.go): it reads the new text with the
// decoder of the scanner, and requires that each target now has the class
// and that nothing else in the file is different. If the proof fails, the
// text is returned as it was, with the reason. A caller never gets a text
// that this package could not prove.
//
// The package does no I/O. pkg/agentmoat reads the files, picks the targets,
// and writes the result.
package patcher

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	yamlv3 "go.yaml.in/yaml/v3"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// Target names one object of a manifest text by the position that the
// scanner gave it (scanner.ManifestOrigin). The position does not change
// when a line is added to the text, and it is exact for two objects of one
// List that have the same name.
type Target struct {
	// Document and Object are ManifestOrigin.Document and
	// ManifestOrigin.Object.
	Document int
	Object   int

	// Kind is scanner.Workload.Kind: "Deployment", "Pod", "CronJob", or
	// the group-kind text of a kind that the scanner has no type for
	// ("Rollout.argoproj.io"). It says where the pod spec is.
	Kind string
}

// Outcome is what happened to one Target.
type Outcome struct {
	Target Target

	// Edited is true when Result.Text has the added line for this target.
	Edited bool

	// Line is the 1-based line of the added line in Result.Text. Zero when
	// Edited is false.
	Line int

	// Reason says why the target was not edited. Empty when Edited is true.
	Reason string
}

// Result is the return value of AddRuntimeClass.
type Result struct {
	// Text is the new text. It is the input, byte for byte, when no target
	// was edited.
	Text []byte

	// Outcomes is parallel to the targets that were given.
	Outcomes []Outcome

	// Added lists the lines of Text that were added (1-based, ascending).
	Added []int
}

// ValidClassName reports whether name can be the name of a RuntimeClass (a
// DNS subdomain name, as the Kubernetes API requires). The check is also
// what keeps a line break or a `#` out of the line that is written.
func ValidClassName(name string) error {
	if name == "" {
		return fmt.Errorf("the RuntimeClass name is empty")
	}
	if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
		return fmt.Errorf("%q is not a valid RuntimeClass name: %s", name, strings.Join(problems, "; "))
	}
	return nil
}

// AddRuntimeClass adds `runtimeClassName: <className>` to the pod spec of
// each target in text. See the package comment for the rules.
//
// It does not fail: a target that cannot be edited has the reason in its
// Outcome. When the proof of the edit fails (check.go), no target of the
// text is edited.
func AddRuntimeClass(text []byte, className string, targets []Target) Result {
	res := Result{Text: text, Outcomes: make([]Outcome, len(targets))}
	for i, t := range targets {
		res.Outcomes[i].Target = t
	}
	refuseAll := func(reason string) Result {
		for i := range res.Outcomes {
			res.Outcomes[i].Reason = reason
		}
		return res
	}

	if err := ValidClassName(className); err != nil {
		return refuseAll(err.Error())
	}
	docs, isJSON := scanner.SplitDocuments(text)
	if isJSON {
		return refuseAll(reasonJSON)
	}

	finder := newFinder(docs)
	inserts := make([]insertion, 0, len(targets))
	for i, t := range targets {
		ins, reason := finder.locate(t)
		if reason != "" {
			res.Outcomes[i].Reason = reason
			continue
		}
		ins.outcome = i
		inserts = append(inserts, ins)
	}
	if reason := sameLine(inserts); reason != "" {
		return refuseAll(reason)
	}
	if len(inserts) == 0 {
		return res
	}

	newText := insertLines(text, inserts, className)
	edited := make([]Target, 0, len(inserts))
	for _, ins := range inserts {
		edited = append(edited, targets[ins.outcome])
	}
	if err := check(text, newText, className, edited); err != nil {
		reason := "the edit did not pass its own check, so the file is not changed: " + err.Error()
		for _, ins := range inserts {
			res.Outcomes[ins.outcome].Reason = reason
		}
		return res
	}

	res.Text = newText
	for _, ins := range inserts {
		res.Outcomes[ins.outcome].Edited = true
		res.Outcomes[ins.outcome].Line = ins.newLine
		res.Added = append(res.Added, ins.newLine)
	}
	sort.Ints(res.Added)
	return res
}

// The reasons for a target that is not edited. They are text for a person:
// each one says what the form is and what to do.
const (
	reasonJSON   = "JSON is not edited: add \"runtimeClassName\" to the pod spec by hand"
	reasonFlow   = "flow-style YAML ({...}) is not edited: add runtimeClassName to the pod spec by hand"
	reasonAnchor = "a YAML anchor or alias is on the way to the pod spec, so an edit there would change each object that shares the text: " +
		"add runtimeClassName by hand"
	reasonEmptyKey = "the pod spec already has a runtimeClassName key, with no name as its value: set the value by hand"
)

// insertion is one line to add.
type insertion struct {
	// afterLine is the 1-based line of the text that the new line follows:
	// the line of the `spec:` key of the pod spec.
	afterLine int

	// indent is the number of spaces before the key on the new line.
	indent int

	// outcome is the index of the target in the Outcomes.
	outcome int

	// newLine is the line of the added line in the new text. insertLines
	// sets it.
	newLine int
}

// sameLine is a guard: two targets must not give the same line. It cannot
// occur for two objects of valid block-style YAML, so a hit means that the
// search is wrong for this text, and then no edit is safe.
func sameLine(inserts []insertion) string {
	seen := map[int]bool{}
	for _, ins := range inserts {
		if seen[ins.afterLine] {
			return fmt.Sprintf("two objects have their pod spec on line %d, so the file is not changed", ins.afterLine)
		}
		seen[ins.afterLine] = true
	}
	return ""
}

// insertLines returns text with the lines added, and sets newLine on each
// insertion. Each new line has the line ending of the line that it follows,
// so a file with CRLF line endings keeps them. No other byte of the text is
// touched (a byte order mark, a missing last line ending).
func insertLines(text []byte, inserts []insertion, className string) []byte {
	after := make(map[int]*insertion, len(inserts))
	for i := range inserts {
		after[inserts[i].afterLine] = &inserts[i]
	}
	value := scalarText(className)

	var out bytes.Buffer
	out.Grow(len(text) + len(inserts)*(len(value)+40))
	lineNo, written := 0, 0
	for rest := text; len(rest) > 0; {
		var line []byte
		if i := bytes.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			line, rest = rest, nil
		}
		lineNo++
		written++
		out.Write(line)

		ins := after[lineNo]
		if ins == nil {
			continue
		}
		ending := "\n"
		if bytes.HasSuffix(line, []byte("\r\n")) {
			ending = "\r\n"
		}
		out.WriteString(strings.Repeat(" ", ins.indent))
		out.WriteString("runtimeClassName: ")
		out.WriteString(value)
		out.WriteString(ending)
		written++
		ins.newLine = written
	}
	return out.Bytes()
}

// scalarText returns the class name as a YAML scalar. A name such as "on",
// "123", or "1e3" is a valid RuntimeClass name, and as a plain scalar it is
// a boolean or a number, which a pod spec refuses. The decoder that decides
// is the one that Kubernetes tools use (sigs.k8s.io/yaml, YAML 1.1 rules),
// not the parser of this package. A name that does not come back as the
// same string gets double quotes; a valid name has no character that needs
// an escape inside them.
func scalarText(className string) string {
	var probe map[string]any
	if err := yaml.Unmarshal([]byte("k: "+className+"\n"), &probe); err == nil {
		if s, ok := probe["k"].(string); ok && s == className {
			return className
		}
	}
	return `"` + className + `"`
}

// ---------------------------------------------------------------------------
// Finding the place: one object of one document, then its pod spec.
// ---------------------------------------------------------------------------

// finder locates targets in the documents of one text. It parses a document
// one time, also when more than one target is in it.
type finder struct {
	docs   map[int]scanner.Document
	parsed map[int]*parsedDocument
}

// parsedDocument is the objects of one document, in the order of
// ManifestOrigin.Object, or the reason why the document could not be parsed.
type parsedDocument struct {
	objects []objectNode
	err     error
}

// objectNode is one object of a document.
type objectNode struct {
	node *yamlv3.Node

	// shared is true when the object, or a list that it is in, has an
	// anchor or is the target of an alias: its text can be the text of
	// another object too.
	shared bool
}

func newFinder(docs []scanner.Document) *finder {
	f := &finder{docs: make(map[int]scanner.Document, len(docs)), parsed: map[int]*parsedDocument{}}
	for _, d := range docs {
		f.docs[d.Index] = d
	}
	return f
}

// locate returns where the line for one target goes, or the reason why the
// target is not edited.
func (f *finder) locate(t Target) (insertion, string) {
	doc, ok := f.docs[t.Document]
	if !ok {
		return insertion{}, fmt.Sprintf("document %d is not in the text", t.Document)
	}
	parsed := f.parse(doc)
	if parsed.err != nil {
		// The scanner read this document, so it is valid for the parser of
		// Kubernetes tools. The parser of this package is another one and
		// is more strict in some cases (a key that is there two times).
		return insertion{}, "the document cannot be read for an edit (" + oneLine(parsed.err.Error()) + "): add runtimeClassName by hand"
	}
	if t.Object < 1 || t.Object > len(parsed.objects) {
		return insertion{}, fmt.Sprintf("object %d is not in document %d (it has %d)", t.Object, t.Document, len(parsed.objects))
	}
	obj := parsed.objects[t.Object-1]
	if obj.shared {
		return insertion{}, reasonAnchor
	}

	key, spec, reason := podSpecOf(obj.node, t.Kind)
	if reason != "" {
		return insertion{}, reason
	}
	for i := 0; i+1 < len(spec.Content); i += 2 {
		if spec.Content[i].Value == "runtimeClassName" {
			return insertion{}, reasonEmptyKey
		}
	}
	first := spec.Content[0]
	if first.Line <= key.Line || first.Column < 2 {
		// A block mapping starts on a line after its key, with an indent.
		// If the parser says another thing, the place is not known.
		return insertion{}, "the pod spec does not start on a line of its own: add runtimeClassName by hand"
	}
	return insertion{afterLine: doc.FirstLine + key.Line - 1, indent: first.Column - 1}, ""
}

func (f *finder) parse(doc scanner.Document) *parsedDocument {
	if p, ok := f.parsed[doc.Index]; ok {
		return p
	}
	p := &parsedDocument{}
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(doc.Data, &root); err != nil {
		p.err = err
	} else {
		collectObjects(&root, false, &p.objects)
	}
	f.parsed[doc.Index] = p
	return p
}

// collectObjects lists the objects of a document in the order in which the
// scanner counts them (manifestCollector.readValue): the items of a sequence
// and of a List are visited in order, a mapping that is not a List is one
// object, and a scalar that is not null is one object too (the scanner
// counts it and then skips it).
//
// The two walks are two programs, one on JSON and one on YAML nodes, and
// they must agree. The check of the edit (check.go) is what proves that
// they did: it finds the edited object again by the count of the scanner.
func collectObjects(n *yamlv3.Node, shared bool, out *[]objectNode) {
	if n.Kind == yamlv3.AliasNode {
		if n.Alias == nil {
			return
		}
		n, shared = n.Alias, true
	}
	if n.Anchor != "" {
		shared = true
	}
	switch n.Kind {
	case yamlv3.DocumentNode, yamlv3.SequenceNode:
		for _, item := range n.Content {
			collectObjects(item, shared, out)
		}
	case yamlv3.MappingNode:
		if items := listItems(n); items != nil {
			collectObjects(items, shared, out)
			return
		}
		*out = append(*out, objectNode{node: n, shared: shared})
	case yamlv3.ScalarNode:
		if n.ShortTag() != "!!null" {
			*out = append(*out, objectNode{node: n, shared: shared})
		}
	}
}

// listItems returns the `items` sequence of a List object (`kind: List`, or
// a typed list such as DeploymentList), or nil when the mapping is not one.
// It is the rule of the scanner: the kind ends with "List" and items is an
// array.
func listItems(mapping *yamlv3.Node) *yamlv3.Node {
	kind := valueOf(mapping, "kind")
	if kind == nil || kind.Kind != yamlv3.ScalarNode || !strings.HasSuffix(kind.Value, "List") {
		return nil
	}
	items := valueOf(mapping, "items")
	if items == nil {
		return nil
	}
	resolved := items
	if resolved.Kind == yamlv3.AliasNode && resolved.Alias != nil {
		resolved = resolved.Alias
	}
	if resolved.Kind != yamlv3.SequenceNode {
		return nil
	}
	// The node as written: collectObjects sees an alias or an anchor on it
	// and marks the items as shared.
	return items
}

// valueOf returns the value node of a key in a mapping, or nil.
func valueOf(mapping *yamlv3.Node, key string) *yamlv3.Node {
	_, value := entryOf(mapping, key)
	return value
}

// entryOf returns the key node and the value node of a key in a mapping, or
// two nils.
func entryOf(mapping *yamlv3.Node, key string) (keyNode, valueNode *yamlv3.Node) {
	if mapping == nil || mapping.Kind != yamlv3.MappingNode {
		return nil, nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Kind == yamlv3.ScalarNode && mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// podSpecPath returns the keys from the top of an object to its pod spec.
// It is the rule of the scanner: a Pod has it at spec, a CronJob at
// spec.jobTemplate.spec.template.spec, and each other kind with a pod
// template at spec.template.spec. A kind that the scanner has no type for
// (its Kind has a group: "Rollout.argoproj.io") can have a jobTemplate and
// no template.
func podSpecPath(object *yamlv3.Node, kind string) []string {
	template := []string{"spec", "template", "spec"}
	jobTemplate := []string{"spec", "jobTemplate", "spec", "template", "spec"}
	switch kind {
	case "Pod":
		return []string{"spec"}
	case "CronJob":
		return jobTemplate
	case "Deployment", "StatefulSet", "DaemonSet", "Job":
		return template
	}
	if t := valueOf(valueOf(object, "spec"), "template"); t == nil || t.ShortTag() == "!!null" {
		return jobTemplate
	}
	return template
}

// podSpecOf walks from an object to its pod spec. It returns the node of the
// last `spec` key and the mapping that is its value, or a reason.
func podSpecOf(object *yamlv3.Node, kind string) (key, spec *yamlv3.Node, reason string) {
	cur := object
	for _, name := range podSpecPath(object, kind) {
		if cur.Kind != yamlv3.MappingNode {
			return nil, nil, "the object has no pod spec where its kind has one: add runtimeClassName by hand"
		}
		if cur.Style&yamlv3.FlowStyle != 0 {
			return nil, nil, reasonFlow
		}
		k, v := entryOf(cur, name)
		if v == nil {
			return nil, nil, "the object has no pod spec where its kind has one: add runtimeClassName by hand"
		}
		if v.Kind == yamlv3.AliasNode || v.Anchor != "" {
			return nil, nil, reasonAnchor
		}
		key, cur = k, v
	}
	switch {
	case cur.Kind != yamlv3.MappingNode || len(cur.Content) < 2:
		return nil, nil, "the pod spec is empty: add runtimeClassName by hand"
	case cur.Style&yamlv3.FlowStyle != 0:
		return nil, nil, reasonFlow
	}
	return key, cur, ""
}

// oneLine makes a parser message safe to put in a reason: one line, and not
// long.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	const limit = 160
	if len(s) > limit {
		s = s[:limit] + "..."
	}
	return s
}
