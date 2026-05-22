// Package output: JSON renderer.
//
// We use encoding/json with `MarshalIndent` (2-space indent) so the output
// is both machine-readable and reasonable to skim in a terminal. The schema
// types already carry the right `json:` tags, so the same function handles
// every envelope (ScanReport, MigrationPlan, ApplyResult, RollbackResult).

package output

import (
	"encoding/json"
	"fmt"
	"io"
)

// renderJSONAny encodes any document to w as indented JSON. The schema
// types own the field tags; this function is intentionally generic.
func renderJSONAny(doc any, w io.Writer) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %T as JSON: %w", doc, err)
	}
	if _, err := w.Write(out); err != nil {
		return err
	}
	// Trailing newline so the output plays nicely with shell redirection.
	_, err = w.Write([]byte("\n"))
	return err
}
