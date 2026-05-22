// Package output: horizontal bar chart helper for SUMMARY blocks.
//
// Every table-mode renderer (scan, plan, apply/rollback, verify) prints
// a SUMMARY block above its body table. The plain "label N  label N"
// text reads as a flat list; a horizontal bar lets the eye see ratios
// directly.
//
// Design
//
//   - Per-category bars (not stacked) so each category has its own ruler
//     and the smallest non-zero bucket is still visible.
//   - Bar length is proportional to max(count) so a long-tail category
//     doesn't collapse to one pixel; percent in the trailing label
//     references total, so both signals are present.
//   - In color mode every row uses U+2588 FULL BLOCK; the semantic style
//     carries the per-category signal. In no-color mode each row uses a
//     distinct shade glyph so the categories stay visually scannable.
//   - Width is dynamic for TTY writers and a fixed 40 for pipes / tests
//     so output stays deterministic.

package output

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"
)

// barItem is one row in a horizontal bar chart.
type barItem struct {
	Symbol      string
	Label       string
	Count       int
	Style       lipgloss.Style
	NoColorFill rune // shade glyph used when useColor is false
}

const colorFill = '█'

// renderSummaryBar formats items as a multi-row horizontal bar chart.
// width is the bar-column width in cells. Returns the empty string when
// every count is zero so callers can print their own placeholder.
func renderSummaryBar(items []barItem, width int, useColor bool) string {
	if len(items) == 0 || width <= 0 {
		return ""
	}
	maxCount, total, labelW := 0, 0, 0
	for _, it := range items {
		if it.Count > maxCount {
			maxCount = it.Count
		}
		total += it.Count
		if n := len(it.Label); n > labelW {
			labelW = n
		}
	}
	if total == 0 {
		return ""
	}
	var b strings.Builder
	for i, it := range items {
		var cells int
		if it.Count > 0 {
			cells = int(math.Round(float64(it.Count) / float64(maxCount) * float64(width)))
			if cells < 1 {
				cells = 1
			}
			if cells > width {
				cells = width
			}
		}
		fill := colorFill
		if !useColor {
			fill = it.NoColorFill
		}
		bar := strings.Repeat(string(fill), cells) + strings.Repeat(" ", width-cells)
		styledBar := it.Style.Render(bar)
		pct := int(math.Round(float64(it.Count) / float64(total) * 100))
		fmt.Fprintf(&b, "  %s %-*s  %s  %4d (%2d%%)",
			it.Symbol, labelW, it.Label, styledBar, it.Count, pct)
		if i < len(items)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// summaryBarWidth returns the bar-column width to use for w. When w is
// an *os.File pointing at a TTY we ask the OS for the terminal width and
// clamp the bar so it fits without wrapping the trailing label.
// Otherwise we return a fixed 40 so pipes, CI capture, and bytes.Buffer
// tests are deterministic regardless of host terminal.
func summaryBarWidth(w io.Writer) int {
	const (
		minBar        = 20
		maxBar        = 50
		defaultBar    = 40
		labelOverhead = 30
	)
	f, ok := w.(*os.File)
	if !ok {
		return defaultBar
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil || cols <= 0 {
		return defaultBar
	}
	w2 := cols - labelOverhead
	switch {
	case w2 < minBar:
		return minBar
	case w2 > maxBar:
		return maxBar
	default:
		return w2
	}
}
