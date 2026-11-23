// Package output renders an agentmoat report (a ScanReport, PreflightReport,
// MigrationPlan, ApplyResult, RollbackResult, VerifyReport, or
// ExplainDocument) into one of five formats: a human-friendly columnar
// table, a stable JSON document, a stable YAML document, Markdown for a CI
// job summary or a pull-request comment, or SARIF for code scanning.
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
//
// Color contract: only the table format ever emits ANSI escape codes. JSON
// and YAML are byte-identical regardless of the RenderOptions argument, so
// machine consumers (jq, scripts, MCP clients) can rely on stable bytes.
//
// Not every format fits every document: SARIF is a list of findings with
// locations, and only a ScanReport has those. Format.Supports is the list,
// and ParseFor lets a command refuse a format before it does any work.
package output

import (
	"fmt"
	"io"
	"strings"

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
	// FormatMarkdown: GitHub-flavored Markdown, for a place that renders it
	// and does not show stderr: a CI job summary, a pull-request comment, a
	// merge-request note. See markdown.go.
	FormatMarkdown Format = "markdown"
	// FormatSARIF: SARIF 2.1.0, the interchange format of code scanning
	// tools. ScanReport only. See sarif.go.
	FormatSARIF Format = "sarif"
)

// formatKinds lists the document kinds a format can render, for the formats
// that cannot render every kind. A format that is not a key here (table,
// JSON, YAML) renders every kind.
//
// ExplainDocument is in neither list: its content is prose that the table
// form already prints, and a job summary has no use for it.
var formatKinds = map[Format][]string{
	FormatMarkdown: {
		schema.KindScanReport,
		schema.KindMigrationPlan,
		schema.KindApplyResult,
		schema.KindRollbackResult,
		schema.KindVerifyReport,
		schema.KindPreflightReport,
		schema.KindPatchResult,
	},
	FormatSARIF: {
		schema.KindScanReport,
	},
}

// allFormats is every format, in the order help text lists them.
var allFormats = []Format{FormatTable, FormatJSON, FormatYAML, FormatMarkdown, FormatSARIF}

// Supports reports whether the format can render a document of the given
// kind (a schema.Kind* constant).
func (f Format) Supports(kind string) bool {
	kinds, limited := formatKinds[f]
	if !limited {
		return true
	}
	for _, k := range kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// GateOutcome is the decision of the scan gate, in the form a renderer
// needs. The values are the same text as agentmoat.GateOutcome; the type is
// repeated here so that this package does not import the orchestrator.
type GateOutcome string

// The gate outcomes. See pkg/agentmoat/gate.go for what each one means.
const (
	GateOff            GateOutcome = "off"
	GatePassed         GateOutcome = "passed"
	GateFailed         GateOutcome = "failed"
	GateNothingChecked GateOutcome = "nothing-checked"
	GateRefused        GateOutcome = "refused"
)

// Gate is the result of the scan gate (`agentmoat scan --fail-on`), given
// to the renderer by the caller. The gate is not part of the ScanReport on
// the wire; two formats still need it:
//
//   - Markdown shows it first. A job summary does not show stderr, and a
//     report with no gate line reads as a pass.
//   - SARIF uses Failing to set levels. See sarif.go.
type Gate struct {
	// FailOn is the --fail-on setting ("incompatible", "sandboxed", "none").
	FailOn string
	// Outcome is the decision.
	Outcome GateOutcome
	// Summary is one sentence that states the outcome.
	Summary string
	// Details has one line for each workload that failed, when the gate
	// lists them.
	Details []string
	// Failing lists the workloads that make the gate fail.
	Failing []schema.WorkloadRef
}

// RenderOptions captures the optional knobs that influence how a document is
// rendered. Zero-value is meaningful: auto-detect color from the writer.
//
// JSON and YAML ignore these options, so that machine-readable output stays
// byte-identical regardless of CLI flags.
type RenderOptions struct {
	// NoColor, when true, forces ANSI escapes off even on a TTY. The CLI
	// sets this when --no-color is passed or NO_COLOR is in the env. See
	// pkg/output/style.go: ColorEnabled. Only the table format reads it.
	NoColor bool

	// Gate is the result of the scan gate, for a ScanReport. Only the
	// Markdown and SARIF formats read it. Nil means the caller evaluated
	// no gate: Markdown then prints no gate line, and SARIF sets levels as
	// the default gate (--fail-on incompatible) would.
	Gate *Gate
}

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
	case "markdown", "md":
		return FormatMarkdown, nil
	case "sarif":
		return FormatSARIF, nil
	default:
		return "", fmt.Errorf("unknown output format %q (want table|json|yaml|markdown|sarif)", s)
	}
}

// ParseFor is Parse for a command that renders one kind of document (a
// schema.Kind* constant). It also refuses a format that cannot render that
// kind.
//
// Commands call this before they do any work. Without it, a format that
// Render cannot write would fail after the work was done: `apply
// --dry-run=false -o sarif` would change the cluster and then print an
// error where the result belongs.
func ParseFor(s, kind string) (Format, error) {
	format, err := Parse(s)
	if err != nil {
		return "", err
	}
	if !format.Supports(kind) {
		var usable []string
		for _, f := range allFormats {
			if f.Supports(kind) {
				usable = append(usable, string(f))
			}
		}
		return "", fmt.Errorf("output format %q is not available for %s output; use one of: %s",
			format, kind, strings.Join(usable, ", "))
	}
	return format, nil
}

// Render writes any of the agentmoat envelope types to w in the given
// format. The function dispatches on the concrete type: JSON and YAML are
// generic (encoding via struct tags), while the table and Markdown renderers
// are bespoke per Kind because each shape needs a different column layout.
// A format that cannot render the document (see Format.Supports) is an
// error.
//
// Pass either a pointer or a value; the helper unwraps both.
func Render(doc any, format Format, w io.Writer, opts RenderOptions) error {
	switch format {
	case FormatJSON:
		// JSON/YAML branches deliberately ignore opts: their output must be
		// byte-identical so downstream tools (jq, yq, MCP, etc.) are stable.
		return renderJSONAny(doc, w)
	case FormatYAML:
		return renderYAMLAny(doc, w)
	case FormatMarkdown:
		return renderMarkdownAny(doc, w, opts)
	case FormatSARIF:
		return renderSARIFAny(doc, w, opts)
	case FormatTable:
		// Compute the color decision once and pass it down. We OR opts.NoColor
		// with ColorEnabled's full check (which also handles NO_COLOR env and
		// non-TTY writer) so the renderers themselves never branch on env or
		// writer type.
		useColor := !opts.NoColor && ColorEnabled(w, opts.NoColor)
		return renderTableAny(doc, w, useColor)
	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}

// renderTableAny dispatches the document on Kind / concrete type to the
// matching table renderer.
func renderTableAny(doc any, w io.Writer, useColor bool) error {
	switch d := doc.(type) {
	case *schema.ScanReport:
		return renderScanReportTable(d, w, useColor)
	case *schema.MigrationPlan:
		return renderMigrationPlanTable(d, w, useColor)
	case *schema.ApplyResult:
		return renderApplyResultTable(d, w, "apply", useColor)
	case *schema.RollbackResult:
		return renderRollbackResultTable(d, w, useColor)
	case *schema.VerifyReport:
		return renderVerifyReportTable(d, w, useColor)
	case *schema.PreflightReport:
		return renderPreflightReportTable(d, w, useColor)
	case *schema.PatchResult:
		return renderPatchResultTable(d, w, useColor)
	case *schema.ExplainDocument:
		return renderExplainDocumentTable(d, w, useColor)
	default:
		return fmt.Errorf("output: no table renderer for %T", doc)
	}
}
