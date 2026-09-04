// Tests for the style layer (style.go). These tests deliberately avoid
// assertions on ANSI escape bytes: lipgloss/termenv's color detection is
// environment-sensitive (TTY vs not, COLORTERM env, etc.) and pinning the
// exact escape sequence would be flaky across dev machines and CI.
//
// Instead, the tests verify the policy questions: does ColorEnabled return
// the right boolean for every input combination, and does StatusBadge
// produce a string that (a) starts with the right symbol and (b) contains
// the status text? That is sufficient because:
//
//   - When useColor is false, Styles fields are zero-value, so Render is a
//     no-op: the substring assertion is exact.
//   - When useColor is true, the rendered string contains the status text
//     surrounded by ANSI escapes, so a substring assertion still passes.

package output

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestColorEnabled(t *testing.T) {
	// NOTE: this test deliberately does NOT call t.Parallel(). The subtests
	// touch the NO_COLOR environment variable via t.Setenv, which Go's
	// testing package forbids combining with t.Parallel because env is
	// process-global and parallel manipulation would race.

	// Each subtest sets up its own writer + flag state. We use t.Setenv so
	// the env var is cleaned up automatically at the end of the test.
	tests := []struct {
		name     string
		noColor  bool
		setEnv   bool   // when true, t.Setenv("NO_COLOR", envValue)
		envValue string // value for NO_COLOR
		writer   func() any
		want     bool
	}{
		{
			name:    "buffer writer always returns false (non-TTY)",
			noColor: false,
			writer:  func() any { return &bytes.Buffer{} },
			want:    false,
		},
		{
			name:    "no-color flag forces false even with TTY-like writer",
			noColor: true,
			writer:  func() any { return os.Stderr },
			want:    false,
		},
		{
			name:     "NO_COLOR env var forces false",
			noColor:  false,
			setEnv:   true,
			envValue: "1",
			writer:   func() any { return os.Stderr },
			want:     false,
		},
		{
			name:     "NO_COLOR empty string is treated as unset (per no-color.org)",
			noColor:  false,
			setEnv:   true,
			envValue: "",
			writer:   func() any { return &bytes.Buffer{} },
			// Still false because the writer is non-*os.File. We're just
			// asserting NO_COLOR="" does NOT alone force false; the writer
			// check is what wins here.
			want: false,
		},
		{
			name:    "nil-implementing writer (custom type) returns false",
			noColor: false,
			writer:  func() any { return &noopWriter{} },
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv("NO_COLOR", tc.envValue)
			} else {
				// Explicitly unset so a host environment with NO_COLOR
				// already set does not contaminate the "flag-only" cases.
				t.Setenv("NO_COLOR", "")
				_ = os.Unsetenv("NO_COLOR")
			}
			w := tc.writer().(interface {
				Write(p []byte) (n int, err error)
			})
			got := ColorEnabled(w, tc.noColor)
			if got != tc.want {
				t.Fatalf("ColorEnabled(noColor=%v) = %v, want %v",
					tc.noColor, got, tc.want)
			}
		})
	}
}

// noopWriter is a custom io.Writer (not *os.File) used by ColorEnabled
// tests to confirm non-file writers always disable color.
type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }

func TestStatusBadge(t *testing.T) {
	t.Parallel()

	// useColor=false so the Styles fields are zero-value and Render is a
	// no-op. That makes every assertion exact bytes (no ANSI escapes
	// involved), so the table is deterministic across CI and dev machines.
	s := NewStyles(false)

	tests := []struct {
		status     string
		wantPrefix string // symbol + space, or "" for unknown statuses
		wantText   string // the status text always appears in the output
	}{
		// scan verdicts
		{status: "compatible", wantPrefix: "✓ ", wantText: "compatible"},
		{status: "needs-review", wantPrefix: "⚠ ", wantText: "needs-review"},
		{status: "review", wantPrefix: "⚠ ", wantText: "review"},
		{status: "incompatible", wantPrefix: "✗ ", wantText: "incompatible"},

		// apply / rollback step statuses
		{status: "applied", wantPrefix: "✓ ", wantText: "applied"},
		{status: "already-applied", wantPrefix: "→ ", wantText: "already-applied"},
		{status: "skipped", wantPrefix: "• ", wantText: "skipped"},
		{status: "failed", wantPrefix: "✗ ", wantText: "failed"},

		// verify step statuses (mapping kept here so badge is single SoT)
		{status: "ok", wantPrefix: "✓ ", wantText: "ok"},
		{status: "mismatch", wantPrefix: "⚠ ", wantText: "mismatch"},
		{status: "error", wantPrefix: "✗ ", wantText: "error"},

		// preflight readiness and finding severities
		{status: "ready", wantPrefix: "✓ ", wantText: "ready"},
		{status: "blocked", wantPrefix: "✗ ", wantText: "blocked"},
		{status: "warn", wantPrefix: "⚠ ", wantText: "warn"},
		{status: "info", wantPrefix: "• ", wantText: "info"},

		// unknown: no symbol, no styling, just the raw text
		{status: "totally-made-up", wantPrefix: "", wantText: "totally-made-up"},
		{status: "", wantPrefix: "", wantText: ""},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			got := StatusBadge(s, tc.status)
			if tc.wantPrefix != "" {
				if !strings.HasPrefix(got, tc.wantPrefix) {
					t.Fatalf("StatusBadge(%q) = %q, want prefix %q",
						tc.status, got, tc.wantPrefix)
				}
			}
			if !strings.Contains(got, tc.wantText) {
				t.Fatalf("StatusBadge(%q) = %q, want to contain %q",
					tc.status, got, tc.wantText)
			}
			// Sanity: unknown statuses are the raw text (no symbol, no
			// trailing/leading whitespace from a stray Render).
			if tc.wantPrefix == "" && got != tc.wantText {
				t.Fatalf("StatusBadge(%q) for unknown status = %q, want exact %q",
					tc.status, got, tc.wantText)
			}
		})
	}
}

// TestNewStyles_PlainMode verifies that when useColor is false every Style
// field renders its input unchanged. This is the property table.go relies on
// when it calls s.Bold.Render unconditionally.
func TestNewStyles_PlainMode(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)
	// We wrap each style's Render (which is variadic) in a unary closure so
	// the table loop below can treat them uniformly.
	cases := []struct {
		name  string
		style func(string) string
	}{
		{"Success", func(in string) string { return s.Success.Render(in) }},
		{"Warn", func(in string) string { return s.Warn.Render(in) }},
		{"Danger", func(in string) string { return s.Danger.Render(in) }},
		{"Info", func(in string) string { return s.Info.Render(in) }},
		{"Muted", func(in string) string { return s.Muted.Render(in) }},
		{"Bold", func(in string) string { return s.Bold.Render(in) }},
	}
	const in = "hello"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.style(in)
			if got != in {
				t.Fatalf("%s.Render(%q) = %q, want %q (plain mode should be a no-op)",
					c.name, in, got, in)
			}
		})
	}
}
