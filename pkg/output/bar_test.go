// Tests for the horizontal bar chart helper (bar.go). The renderer is a
// pure function over a slice of barItems, so the assertions here are
// exact substring/length checks on the produced string. All tests use
// useColor=false (matching the rest of the package: bytes.Buffer is not
// an *os.File, so production codepaths arrive at the renderer in plain
// mode too) which keeps the fill characters and layout deterministic.

package output

import (
	"bytes"
	"fmt"
	"math"
	"strings"
	"testing"
	"unicode/utf8"
)

// fillCount counts how many times one of the bar-fill runes appears in s.
// We accept any of '█', '▓', '▒', '░' because no-color mode uses a
// per-item shade rune and we want a single helper for all of them.
func fillCount(s string) int {
	n := 0
	for _, r := range s {
		switch r {
		case '█', '▓', '▒', '░':
			n++
		}
	}
	return n
}

func TestRenderSummaryBar_Basic(t *testing.T) {
	t.Parallel()
	s := NewStyles(false) // zero-value styles, Render is a no-op
	items := []barItem{
		{Symbol: symbolOK, Label: "compatible", Count: 7, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolWarn, Label: "review", Count: 2, Style: s.Warn, NoColorFill: '▓'},
		{Symbol: symbolFail, Label: "incompatible", Count: 1, Style: s.Danger, NoColorFill: '▒'},
	}
	got := renderSummaryBar(items, 40, false)

	wantSubs := []string{
		// Symbol + label for every row.
		"✓ compatible",
		"⚠ review",
		"✗ incompatible",
		// Trailing count + percent for every row. 7/10 = 70%, 2/10 = 20%, 1/10 = 10%.
		"   7 (70%)",
		"   2 (20%)",
		"   1 (10%)",
		// Each row uses its NoColorFill rune.
		"█",
		"▓",
		"▒",
	}
	for _, sub := range wantSubs {
		if !strings.Contains(got, sub) {
			t.Errorf("renderSummaryBar output missing %q\nfull output:\n%s", sub, got)
		}
	}

	// Three rows joined by '\n' means exactly two newlines.
	if n := strings.Count(got, "\n"); n != 2 {
		t.Errorf("expected 2 newlines (3 rows joined), got %d:\n%s", n, got)
	}
}

func TestRenderSummaryBar_BarColumnWidth(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)

	// Across these cases, every row's bar (fill + spaces) must be exactly
	// `width` cells wide. The string we render is the full line; we slice
	// out the bar column by counting fill runes + trailing spaces between
	// the label and the count.
	tests := []struct {
		name  string
		items []barItem
		width int
	}{
		{
			name: "even distribution",
			items: []barItem{
				{Symbol: symbolOK, Label: "a", Count: 5, Style: s.Success, NoColorFill: '█'},
				{Symbol: symbolWarn, Label: "b", Count: 5, Style: s.Warn, NoColorFill: '▓'},
			},
			width: 30,
		},
		{
			name: "one dominant one tiny",
			items: []barItem{
				{Symbol: symbolOK, Label: "a", Count: 99, Style: s.Success, NoColorFill: '█'},
				{Symbol: symbolWarn, Label: "b", Count: 1, Style: s.Warn, NoColorFill: '▓'},
			},
			width: 40,
		},
		{
			name: "single category",
			items: []barItem{
				{Symbol: symbolOK, Label: "a", Count: 3, Style: s.Success, NoColorFill: '█'},
			},
			width: 20,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderSummaryBar(tc.items, tc.width, false)
			lines := strings.Split(got, "\n")
			// Compute labelW the same way the renderer does so we can
			// solve back to the per-row bar column width from the
			// rendered line length.
			labelW := 0
			total := 0
			for _, it := range tc.items {
				if n := len(it.Label); n > labelW {
					labelW = n
				}
				total += it.Count
			}
			for i, line := range lines {
				// Format breakdown: "  <symbol> <label-padded>  <bar>  <count4> (<pct2>%)"
				// In visual cells (assuming single-cell symbol runes,
				// which is the case for ✓/⚠/✗/•/→): 2 + 1 + 1 + labelW
				// + 2 + width + 2 + 4 + 2 + (pct chars) + 2.
				// pct uses %2d, so it is at least 2 chars and grows when
				// the value is >= 100.
				pct := int(0)
				if total > 0 {
					pct = roundPercent(tc.items[i].Count, total)
				}
				pctStr := fmt.Sprintf("%2d", pct)
				wantLen := 2 + 1 + 1 + labelW + 2 + tc.width + 2 + 4 + 2 + len(pctStr) + 2
				gotLen := utf8.RuneCountInString(line)
				if gotLen != wantLen {
					t.Errorf("row %d: total rune count = %d, want %d (labelW=%d, width=%d, pct=%q). line=%q",
						i, gotLen, wantLen, labelW, tc.width, pctStr, line)
				}
			}
		})
	}
}

// roundPercent mirrors the percent rounding in renderSummaryBar so the
// test can predict the trailing "(NN%)" string width.
func roundPercent(count, total int) int {
	return int(math.Round(float64(count) / float64(total) * 100))
}

func TestRenderSummaryBar_ZeroTotalReturnsEmpty(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)
	items := []barItem{
		{Symbol: symbolOK, Label: "compatible", Count: 0, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolWarn, Label: "review", Count: 0, Style: s.Warn, NoColorFill: '▓'},
	}
	got := renderSummaryBar(items, 40, false)
	if got != "" {
		t.Errorf("expected empty string when all counts are zero, got: %q", got)
	}
}

func TestRenderSummaryBar_SingleCategoryFullWidth(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)
	items := []barItem{
		{Symbol: symbolOK, Label: "compatible", Count: 5, Style: s.Success, NoColorFill: '█'},
	}
	got := renderSummaryBar(items, 30, false)

	// Exactly one row: no newline.
	if strings.Contains(got, "\n") {
		t.Errorf("single-category output should be one line, got:\n%s", got)
	}
	// With max=count, the bar fills the full width.
	if n := fillCount(got); n != 30 {
		t.Errorf("expected 30 fill runes, got %d. output:\n%s", n, got)
	}
	// 100% trailing label.
	if !strings.Contains(got, "(100%)") {
		t.Errorf("expected '(100%%)' in trailing label, got:\n%s", got)
	}
	// Count cell formatted with %4d.
	if !strings.Contains(got, "   5 (100%)") {
		t.Errorf("expected '   5 (100%%)' suffix, got:\n%s", got)
	}
}

func TestRenderSummaryBar_NoColorUsesPerItemFill(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)
	items := []barItem{
		{Symbol: symbolOK, Label: "a", Count: 5, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolWarn, Label: "b", Count: 5, Style: s.Warn, NoColorFill: '▓'},
		{Symbol: symbolDot, Label: "c", Count: 5, Style: s.Muted, NoColorFill: '░'},
	}
	got := renderSummaryBar(items, 20, false)

	lines := strings.Split(got, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), got)
	}
	// Each line should contain its own NoColorFill rune, not the others.
	checks := []struct {
		line string
		want rune
		skip []rune // other fills must not appear on this line
	}{
		{lines[0], '█', []rune{'▓', '░'}},
		{lines[1], '▓', []rune{'█', '░'}},
		{lines[2], '░', []rune{'█', '▓'}},
	}
	for i, c := range checks {
		if !strings.ContainsRune(c.line, c.want) {
			t.Errorf("row %d should contain %q. got: %q", i, c.want, c.line)
		}
		for _, r := range c.skip {
			if strings.ContainsRune(c.line, r) {
				t.Errorf("row %d should NOT contain %q. got: %q", i, r, c.line)
			}
		}
	}
}

func TestRenderSummaryBar_ColorModeUsesFullBlock(t *testing.T) {
	t.Parallel()
	s := NewStyles(false) // styles fields are still zero-value, but useColor=true
	// forces the renderer to pick colorFill ('█') for every row regardless
	// of NoColorFill.
	items := []barItem{
		{Symbol: symbolOK, Label: "a", Count: 5, Style: s.Success, NoColorFill: '▓'},
		{Symbol: symbolWarn, Label: "b", Count: 5, Style: s.Warn, NoColorFill: '▒'},
	}
	got := renderSummaryBar(items, 20, true)

	// '█' is used for both rows; the per-item NoColorFill ('▓', '▒') must
	// not leak into the output in color mode.
	if !strings.ContainsRune(got, '█') {
		t.Errorf("color mode should use '█' fill, got:\n%s", got)
	}
	if strings.ContainsRune(got, '▓') || strings.ContainsRune(got, '▒') {
		t.Errorf("color mode should NOT use per-item NoColorFill, got:\n%s", got)
	}
}

func TestRenderSummaryBar_MinCellInvariant(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)
	// 1 vs 999 at width=40: 1 rounds to math.Round(1/999*40) = 0, but the
	// renderer must clamp to 1 so the tiny bucket is still visible.
	items := []barItem{
		{Symbol: symbolOK, Label: "big", Count: 999, Style: s.Success, NoColorFill: '█'},
		{Symbol: symbolWarn, Label: "tiny", Count: 1, Style: s.Warn, NoColorFill: '▓'},
	}
	got := renderSummaryBar(items, 40, false)

	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), got)
	}
	// The tiny row must have at least one '▓'.
	tinyFills := strings.Count(lines[1], "▓")
	if tinyFills < 1 {
		t.Errorf("expected at least 1 fill cell on tiny row, got %d. line: %q", tinyFills, lines[1])
	}
}

func TestRenderSummaryBar_EmptyItems(t *testing.T) {
	t.Parallel()
	if got := renderSummaryBar(nil, 40, false); got != "" {
		t.Errorf("nil items: expected empty string, got %q", got)
	}
	if got := renderSummaryBar([]barItem{}, 40, false); got != "" {
		t.Errorf("empty items: expected empty string, got %q", got)
	}
}

func TestRenderSummaryBar_NonPositiveWidth(t *testing.T) {
	t.Parallel()
	s := NewStyles(false)
	items := []barItem{
		{Symbol: symbolOK, Label: "a", Count: 5, Style: s.Success, NoColorFill: '█'},
	}
	if got := renderSummaryBar(items, 0, false); got != "" {
		t.Errorf("width=0: expected empty string, got %q", got)
	}
	if got := renderSummaryBar(items, -3, false); got != "" {
		t.Errorf("width=-3: expected empty string, got %q", got)
	}
}

func TestSummaryBarWidth_BufferIsDeterministic(t *testing.T) {
	t.Parallel()
	// A bytes.Buffer is not an *os.File, so summaryBarWidth must fall
	// through to its defaultBar constant (40) regardless of the host
	// terminal size. This is what keeps tests and pipes deterministic.
	if got := summaryBarWidth(&bytes.Buffer{}); got != 40 {
		t.Errorf("summaryBarWidth(*bytes.Buffer) = %d, want 40", got)
	}
}
