// Package output: style layer.
//
// This file centralizes the "do we use color?" decision and exposes a small
// semantic palette built on lipgloss. The rest of the package (table.go in
// particular) consumes the Styles struct and never reaches for lipgloss
// constructors directly, which keeps the styling decisions in one place.
//
// Color decision
//
// Per the no-color.org spec and the project plan, color is opt-out:
//
//   - --no-color flag forces color off, even on a TTY.
//   - NO_COLOR env var (any non-empty value) does the same.
//   - When neither is set, color is on only if the output writer is a real
//     terminal. A bytes.Buffer, an os.Pipe, or any non-*os.File writer is
//     treated as non-TTY so that tests, pipelines, and CI get deterministic
//     plain text.
//
// Plain mode
//
// When useColor is false NewStyles returns a Styles where every field is a
// zero-value lipgloss.Style. A zero-value Style has no color, no bold, no
// padding etc., so Render(s) returns s unchanged. That means callers do not
// have to check useColor before every Render call: they style unconditionally
// and the right thing happens in both modes.

package output

import (
	"io"
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

// Styles is the semantic palette. Renderers reach for these by intent
// ("success", "danger") rather than by concrete color so a future theme
// swap is one place.
type Styles struct {
	// Success is the positive outcome color (applied, compatible).
	Success lipgloss.Style
	// Warn is the attention-but-not-blocking color (needs-review, mismatch).
	Warn lipgloss.Style
	// Danger is the blocking-failure color (incompatible, failed, error).
	Danger lipgloss.Style
	// Info is the informational color (already-applied, neutral hints).
	Info lipgloss.Style
	// Muted is the de-emphasized color (skipped steps, empty-state text,
	// metadata chips).
	Muted lipgloss.Style
	// Bold is plain bold, used for titles and headers.
	Bold lipgloss.Style
}

// NewStyles returns a Styles palette. When useColor is false every field is
// a zero-value lipgloss.Style; Render is then a no-op so the rest of the
// package can call .Render unconditionally.
//
// The color choices below are lipgloss.Color codes (ANSI 16-color basic
// palette where possible) so they degrade gracefully on dumb terminals.
func NewStyles(useColor bool) *Styles {
	if !useColor {
		// Zero-value Styles: .Render(s) returns s unchanged.
		return &Styles{}
	}
	return &Styles{
		// Green: success, applied, compatible.
		Success: lipgloss.NewStyle().Foreground(lipgloss.Color("2")),
		// Yellow: needs-review, partial mismatch.
		Warn: lipgloss.NewStyle().Foreground(lipgloss.Color("3")),
		// Red: incompatible, failed, error.
		Danger: lipgloss.NewStyle().Foreground(lipgloss.Color("1")),
		// Cyan: informational (already-applied, hints).
		Info: lipgloss.NewStyle().Foreground(lipgloss.Color("6")),
		// Bright black (grey): de-emphasized text.
		Muted: lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		// Plain bold for titles and table headers.
		Bold: lipgloss.NewStyle().Bold(true),
	}
}

// ColorEnabled decides whether the renderer should emit ANSI escape codes
// for the given writer. Callers pass the same writer they will eventually
// hand to Render.
//
// Decision order (any one of these forces color off):
//  1. noColor flag is true.
//  2. NO_COLOR env var is non-empty (per no-color.org).
//  3. w is not an *os.File, or it is but isatty reports it is not a TTY.
//
// The non-*os.File case (bytes.Buffer in tests, an io.Pipe in piped output)
// returns false so tests get plain text without further setup.
func ColorEnabled(w io.Writer, noColor bool) bool {
	if noColor {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// Unicode glyphs used by StatusBadge. Defined as named constants (not
// inline rune literals) so the symbol→meaning mapping is one place to
// grep for. These are plain Unicode (not emoji codepoints), so they
// render in single-cell width on every common terminal.
const (
	symbolOK      = "✓" // ✓ U+2713 CHECK MARK
	symbolFail    = "✗" // ✗ U+2717 BALLOT X
	symbolWarn    = "⚠" // ⚠ U+26A0 WARNING SIGN
	symbolDot     = "•" // • U+2022 BULLET
	symbolArrow   = "→" // → U+2192 RIGHTWARDS ARROW
	symbolFinding = "▸" // ▸ U+25B8 BLACK RIGHT-POINTING SMALL TRIANGLE
)

// StatusBadge returns a symbol+space+colored-text rendering of a status
// string for use in table cells and inline summaries.
//
// The mapping mirrors the Symbol-and-color table in the plan:
//
//	scan:    compatible    -> ✓ Success
//	scan:    needs-review  -> ⚠ Warn  (alias: "review")
//	scan:    incompatible  -> ✗ Danger
//	apply:   applied       -> ✓ Success
//	apply:   already-applied -> → Info
//	apply:   skipped       -> • Muted
//	apply:   failed        -> ✗ Danger
//	verify:  ok            -> ✓ Success
//	verify:  mismatch      -> ⚠ Warn
//	verify:  error         -> ✗ Danger
//	preflight: ready       -> ✓ Success
//	preflight: blocked     -> ✗ Danger
//	finding: warn          -> ⚠ Warn   (error shares the verify mapping)
//	finding: info          -> • Info
//
// Unknown statuses fall through to the raw status string with no symbol
// and no color: that way new schema values surface in the table during
// development rather than silently disappearing.
func StatusBadge(s *Styles, status string) string {
	switch status {
	// scan verdicts
	case "compatible":
		return symbolOK + " " + s.Success.Render(status)
	case "needs-review", "review":
		return symbolWarn + " " + s.Warn.Render(status)
	case "incompatible":
		return symbolFail + " " + s.Danger.Render(status)
	// apply / rollback step statuses
	case "applied":
		return symbolOK + " " + s.Success.Render(status)
	case "already-applied":
		return symbolArrow + " " + s.Info.Render(status)
	case "skipped":
		return symbolDot + " " + s.Muted.Render(status)
	case "failed":
		return symbolFail + " " + s.Danger.Render(status)
	// verify step statuses (defined here so the badge mapping stays the
	// single source of truth for rendering, regardless of which
	// package produces the status)
	case "ok":
		return symbolOK + " " + s.Success.Render(status)
	case "mismatch":
		return symbolWarn + " " + s.Warn.Render(status)
	case "error":
		return symbolFail + " " + s.Danger.Render(status)
	// preflight readiness and finding severities
	case "ready":
		return symbolOK + " " + s.Success.Render(status)
	case "blocked":
		return symbolFail + " " + s.Danger.Render(status)
	case "warn":
		return symbolWarn + " " + s.Warn.Render(status)
	case "info":
		return symbolDot + " " + s.Info.Render(status)
	default:
		return status
	}
}
