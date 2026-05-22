// Package output: YAML renderer.
//
// We use sigs.k8s.io/yaml (NOT the popular gopkg.in/yaml.v3) because:
//
//   - It speaks JSON tags. Our schema types only carry `json:` tags as
//     their canonical wire spec; sigs.k8s.io/yaml round-trips through JSON
//     internally so we never need to duplicate them as `yaml:` tags.
//   - It is already a transitive dep of client-go, so it is "free" in
//     terms of go.mod weight.
//
// The schema types DO carry `yaml:` tags for clarity, but sigs.k8s.io/yaml
// honors the `json:` tags and ignores the `yaml:` tags. The two are
// kept identical to avoid confusion.

package output

import (
	"fmt"
	"io"

	"sigs.k8s.io/yaml"
)

// renderYAMLAny encodes any document to w as YAML. The schema types own
// the field tags; this function is intentionally generic.
func renderYAMLAny(doc any, w io.Writer) error {
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encoding %T as YAML: %w", doc, err)
	}
	if _, err := w.Write(out); err != nil {
		return err
	}
	return nil
}
