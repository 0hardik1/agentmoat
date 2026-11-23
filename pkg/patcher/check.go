// Package patcher: the proof of an edit.
//
// AddRuntimeClass finds the place for its line with one YAML parser
// (go.yaml.in/yaml/v3, for the line and column of a node). The tools that
// read the file later use another one (sigs.k8s.io/yaml, which is what
// kubectl, the API machinery, and the scanner of this project use). The two
// agree on normal manifests. "Normal" is not a guarantee, and the file is in
// a repository of a user.
//
// So the edit is not trusted. check reads the old text and the new text with
// the decoder of the scanner and requires two things:
//
//  1. Nothing else changed. Document for document, the decoded value of the
//     new text is the decoded value of the old text plus keys with the name
//     `runtimeClassName` and the class as the value, one for each edit. A
//     value that changed, a key that went away, or another new key fails
//     the check.
//  2. The new keys are where they must be. The scanner reads the new text,
//     and each edited object (found by its position, not by a line) has the
//     class in its pod spec.
//
// The first rule alone would pass a line that went into the wrong mapping.
// The second alone would pass an edit that also broke the object next to
// it. The two together say: these objects have the class now, and that is
// the only difference.
package patcher

import (
	"fmt"
	"reflect"

	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// check returns nil when newText is oldText plus the class on each edited
// target and nothing else. See the file comment.
func check(oldText, newText []byte, className string, edited []Target) error {
	oldDocs, _ := scanner.SplitDocuments(oldText)
	newDocs, isJSON := scanner.SplitDocuments(newText)
	if isJSON || len(oldDocs) != len(newDocs) {
		return fmt.Errorf("the text had %d documents and now has %d", len(oldDocs), len(newDocs))
	}

	added := 0
	for i := range oldDocs {
		if oldDocs[i].Index != newDocs[i].Index {
			return fmt.Errorf("document %d is now document %d", oldDocs[i].Index, newDocs[i].Index)
		}
		before, err := decode(oldDocs[i].Data)
		if err != nil {
			return fmt.Errorf("document %d before the edit: %w", oldDocs[i].Index, err)
		}
		after, err := decode(newDocs[i].Data)
		if err != nil {
			return fmt.Errorf("document %d after the edit: %w", newDocs[i].Index, err)
		}
		n, err := onlyClassAdded(before, after, className, "")
		if err != nil {
			return fmt.Errorf("document %d: %w", oldDocs[i].Index, err)
		}
		added += n
	}
	if added != len(edited) {
		return fmt.Errorf("%d runtimeClassName keys are new, for %d edits", added, len(edited))
	}
	return classOnTargets(newText, className, edited)
}

// decode reads one document as the scanner does (YAML to JSON), into plain
// Go values.
func decode(data []byte) (any, error) {
	var v any
	if err := yaml.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	return v, nil
}

// onlyClassAdded compares two decoded values. It returns the number of
// `runtimeClassName: <className>` keys that after has and before does not,
// and an error for each other difference. path is for the message.
func onlyClassAdded(before, after any, className, path string) (int, error) {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("%s changed its type", pathText(path))
		}
		return onlyClassAddedToMap(b, a, className, path)
	case []any:
		a, ok := after.([]any)
		if !ok || len(a) != len(b) {
			return 0, fmt.Errorf("%s changed its length or its type", pathText(path))
		}
		added := 0
		for i := range b {
			n, err := onlyClassAdded(b[i], a[i], className, fmt.Sprintf("%s[%d]", path, i))
			if err != nil {
				return 0, err
			}
			added += n
		}
		return added, nil
	default:
		if !reflect.DeepEqual(before, after) {
			return 0, fmt.Errorf("%s changed", pathText(path))
		}
		return 0, nil
	}
}

func onlyClassAddedToMap(before, after map[string]any, className, path string) (int, error) {
	added := 0
	for key, a := range after {
		b, was := before[key]
		if was {
			n, err := onlyClassAdded(b, a, className, path+"."+key)
			if err != nil {
				return 0, err
			}
			added += n
			continue
		}
		if s, ok := a.(string); key != "runtimeClassName" || !ok || s != className {
			return 0, fmt.Errorf("%s is new", pathText(path+"."+key))
		}
		added++
	}
	for key := range before {
		if _, still := after[key]; !still {
			return 0, fmt.Errorf("%s is gone", pathText(path+"."+key))
		}
	}
	return added, nil
}

func pathText(path string) string {
	if path == "" {
		return "the document"
	}
	return path
}

// classOnTargets reads the new text with the scanner and requires the class
// on the pod spec of each edited target. The targets are found by position
// (document, object), which the added lines do not move.
func classOnTargets(newText []byte, className string, edited []Target) error {
	found, err := scanner.FromManifests([]scanner.ManifestInput{{Name: "edit", Data: newText}}, scanner.ManifestOptions{})
	if err != nil {
		return fmt.Errorf("the scanner cannot read the new text: %w", err)
	}
	type position struct{ document, object int }
	classAt := make(map[position]string, len(found.Workloads))
	for i, w := range found.Workloads {
		class := ""
		if rc := w.PodSpec.RuntimeClassName; rc != nil {
			class = *rc
		}
		classAt[position{found.Origins[i].Document, found.Origins[i].Object}] = class
	}
	for _, t := range edited {
		got, ok := classAt[position{t.Document, t.Object}]
		switch {
		case !ok:
			return fmt.Errorf("object %d of document %d is not a workload in the new text", t.Object, t.Document)
		case got != className:
			return fmt.Errorf("object %d of document %d has runtimeClassName %q in the new text, not %q", t.Object, t.Document, got, className)
		}
	}
	return nil
}
