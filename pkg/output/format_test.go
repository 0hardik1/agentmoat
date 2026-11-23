// Tests for the list of formats: Parse, ParseFor, and Format.Supports.
//
// The point of ParseFor is that a command refuses a format before it does
// any work. That only holds when the list it reads (formatKinds) and the
// renderers that Render dispatches to are the same list. The last test here
// checks the two against each other for every kind and every format.

package output

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// everyKind is one empty document of each kind that a command renders.
func everyKind() map[string]any {
	return map[string]any{
		schema.KindScanReport:      schema.NewScanReport(),
		schema.KindMigrationPlan:   schema.NewMigrationPlan(),
		schema.KindApplyResult:     schema.NewApplyResult(),
		schema.KindRollbackResult:  schema.NewRollbackResult(),
		schema.KindVerifyReport:    schema.NewVerifyReport(),
		schema.KindPreflightReport: schema.NewPreflightReport(),
		schema.KindPatchResult:     schema.NewPatchResult(),
		schema.KindExplainDocument: schema.NewExplainDocument(),
	}
}

func TestParse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in      string
		want    Format
		wantErr bool
	}{
		{"", FormatTable, false},
		{"table", FormatTable, false},
		{"json", FormatJSON, false},
		{"yaml", FormatYAML, false},
		{"yml", FormatYAML, false},
		{"markdown", FormatMarkdown, false},
		{"md", FormatMarkdown, false},
		{"sarif", FormatSARIF, false},
		{"SARIF", "", true}, // exact text only, as for the other formats
		{"junit", "", true},
	}
	for _, tc := range tests {
		got, err := Parse(tc.in)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("Parse(%q) = %q, %v; want %q, error %v", tc.in, got, err, tc.want, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "markdown|sarif") {
			t.Errorf("Parse(%q) error = %q, want it to list the new formats", tc.in, err)
		}
	}
}

func TestParseFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		format  string
		kind    string
		wantErr string // "" means no error
	}{
		{"table", schema.KindExplainDocument, ""},
		{"json", schema.KindApplyResult, ""},
		{"markdown", schema.KindScanReport, ""},
		{"md", schema.KindMigrationPlan, ""},
		{"markdown", schema.KindApplyResult, ""},
		{"markdown", schema.KindRollbackResult, ""},
		{"markdown", schema.KindVerifyReport, ""},
		{"markdown", schema.KindPreflightReport, ""},
		{"sarif", schema.KindScanReport, ""},
		// SARIF is findings with locations. Only a scan has those.
		{"sarif", schema.KindApplyResult, "use one of: table, json, yaml, markdown"},
		{"sarif", schema.KindMigrationPlan, "use one of: table, json, yaml, markdown"},
		{"sarif", schema.KindPreflightReport, "use one of: table, json, yaml, markdown"},
		{"sarif", schema.KindExplainDocument, "use one of: table, json, yaml"},
		{"markdown", schema.KindExplainDocument, "use one of: table, json, yaml"},
		{"bogus", schema.KindScanReport, "unknown output format"},
	}
	for _, tc := range tests {
		_, err := ParseFor(tc.format, tc.kind)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("ParseFor(%q, %s) = %v, want no error", tc.format, tc.kind, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("ParseFor(%q, %s) = nil, want an error with %q", tc.format, tc.kind, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("ParseFor(%q, %s) = %q, want it to hold %q", tc.format, tc.kind, err, tc.wantErr)
		}
		// The error names the kind, so the user can see which output
		// the format does not fit.
		if tc.wantErr != "" && tc.format != "bogus" && !strings.Contains(err.Error(), tc.kind) {
			t.Errorf("ParseFor(%q, %s) = %q, want it to name the kind", tc.format, tc.kind, err)
		}
	}
}

// TestSupportsAgreesWithRender: for every kind and every format, Supports
// says yes when Render can write the document, and no when Render returns
// an error. If the two lists come apart, a command either refuses a format
// that works, or accepts one that fails after the work is done.
func TestSupportsAgreesWithRender(t *testing.T) {
	t.Parallel()
	for kind, doc := range everyKind() {
		for _, format := range allFormats {
			var buf bytes.Buffer
			err := Render(doc, format, &buf, RenderOptions{NoColor: true})
			supported := format.Supports(kind)
			switch {
			case supported && err != nil:
				t.Errorf("%s as %s: Supports is true, Render failed: %v", kind, format, err)
			case !supported && err == nil:
				t.Errorf("%s as %s: Supports is false, Render wrote %d bytes with no error", kind, format, buf.Len())
			case supported && buf.Len() == 0:
				t.Errorf("%s as %s: Render wrote nothing", kind, format)
			}
		}
	}
}

// TestEveryFormatIsListed: a format constant that is missing from
// allFormats would be left out of the "use one of" hint and of the test
// above.
func TestEveryFormatIsListed(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"table", "json", "yaml", "markdown", "sarif"} {
		format, err := Parse(text)
		if err != nil {
			t.Fatalf("Parse(%q): %v", text, err)
		}
		found := false
		for _, f := range allFormats {
			if f == format {
				found = true
			}
		}
		if !found {
			t.Errorf("format %q is not in allFormats", format)
		}
	}
	for format := range formatKinds {
		if _, err := Parse(string(format)); err != nil {
			t.Errorf("formatKinds has %q, which Parse does not accept", format)
		}
	}
}
