// Package output renders an agentmoat report (a ScanReport, MigrationPlan,
// ApplyResult, or RollbackResult) into one of three formats: a human-friendly
// columnar table, a stable JSON document, or a stable YAML document.
//
// Why this is its own package
//
//   - Keep formatting logic out of cmd/ so the MCP server can call the
//     renderers directly when producing tool output.
//   - Keep schema (internal/schema) free of rendering imports so the wire
//     types stay portable.
//
// JSON and YAML formats share the exact same field names so dashboards and
// scripts can switch between them with no schema translation. Table output
// is bespoke per Kind because each shape (scan summary, ordered plan steps,
// apply result table) has its own most-useful columns.
package output

import (
	"fmt"
	"io"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// Format is the rendering format.
type Format string

const (
	// FormatTable: human-readable columnar view. Default for terminals.
	FormatTable Format = "table"
	// FormatJSON: encoding/json. Stable order, RFC8259 compliant.
	FormatJSON Format = "json"
	// FormatYAML: sigs.k8s.io/yaml. Same field names as JSON.
	FormatYAML Format = "yaml"
)

// Parse maps a CLI string to a Format. Returns an error for unknown
// values; the CLI catches this at flag-parse time before the work runs.
func Parse(s string) (Format, error) {
	switch s {
	case "table", "":
		return FormatTable, nil
	case "json":
		return FormatJSON, nil
	case "yaml", "yml":
		return FormatYAML, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want table|json|yaml)", s)
	}
}

// Render writes any of the four agentmoat envelope types to w in the given
// format. The function dispatches on the concrete type: JSON and YAML are
// generic (encoding via struct tags), while the table renderer is bespoke
// per Kind because each shape needs a different column layout.
//
// Pass either a pointer or a value; the helper unwraps both.
func Render(doc any, format Format, w io.Writer) error {
	switch format {
	case FormatJSON:
		return renderJSONAny(doc, w)
	case FormatYAML:
		return renderYAMLAny(doc, w)
	case FormatTable:
		return renderTableAny(doc, w)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

// renderTableAny dispatches the document on Kind / concrete type to the
// matching table renderer.
func renderTableAny(doc any, w io.Writer) error {
	switch d := doc.(type) {
	case *schema.ScanReport:
		return renderScanReportTable(d, w)
	case *schema.MigrationPlan:
		return renderMigrationPlanTable(d, w)
	case *schema.ApplyResult:
		return renderApplyResultTable(d, w, "apply")
	case *schema.RollbackResult:
		return renderRollbackResultTable(d, w)
	default:
		return fmt.Errorf("output: no table renderer for %T", doc)
	}
}
