// Package patcher: the unified diff of an edit.
//
// `agentmoat patch` is a dry-run by default, and what it shows is a diff: the
// form in which a reviewer reads a change. The edit only adds lines, and
// AddRuntimeClass knows which lines, so no diff algorithm is necessary: the
// hunks are the added lines with their context.
package patcher

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// contextLines is the number of unchanged lines around a change, as in
// `diff -u` and `git diff`.
const contextLines = 3

// noNewline is the line that a unified diff has after a last line with no
// line ending.
const noNewline = "\\ No newline at end of file\n"

// UnifiedDiff returns the unified diff from the text before an edit to
// newText, where added lists the lines of newText that the edit added
// (Result.Added). It returns "" when added is empty.
//
// The file headers are in the form of `git diff` (a/<path> and b/<path>) for
// a relative path, so that `git apply` takes the output. An absolute path is
// printed as it is.
func UnifiedDiff(path string, newText []byte, added []int) string {
	if len(added) == 0 {
		return ""
	}
	lines := splitLines(newText)
	isAdded := make(map[int]bool, len(added))
	sorted := append([]int(nil), added...)
	sort.Ints(sorted)
	for _, n := range sorted {
		isAdded[n] = true
	}

	var out strings.Builder
	oldName, newName := diffNames(path)
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", oldName, newName)

	addedBefore := 0 // the number of added lines above the current hunk
	for _, h := range hunks(sorted, len(lines)) {
		addedIn := 0
		for n := h.start; n <= h.end; n++ {
			if isAdded[n] {
				addedIn++
			}
		}
		newCount := h.end - h.start + 1
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", h.start-addedBefore, newCount-addedIn, h.start, newCount)
		for n := h.start; n <= h.end; n++ {
			line := lines[n-1]
			if isAdded[n] {
				out.WriteByte('+')
			} else {
				out.WriteByte(' ')
			}
			out.Write(line)
			if !bytes.HasSuffix(line, []byte("\n")) {
				out.WriteString("\n" + noNewline)
			}
		}
		addedBefore += addedIn
	}
	return out.String()
}

// hunk is a range of lines of the new text, 1-based and inclusive.
type hunk struct{ start, end int }

// hunks returns the ranges that cover each added line with its context. Two
// ranges that touch or overlap are one hunk.
func hunks(added []int, total int) []hunk {
	var out []hunk
	for _, n := range added {
		start, end := n-contextLines, n+contextLines
		if start < 1 {
			start = 1
		}
		if end > total {
			end = total
		}
		if last := len(out) - 1; last >= 0 && start <= out[last].end+1 {
			out[last].end = end
			continue
		}
		out = append(out, hunk{start, end})
	}
	return out
}

// splitLines splits text into lines. Each line keeps its line ending; the
// last one has none when the text does not end with one.
func splitLines(text []byte) [][]byte {
	var lines [][]byte
	for len(text) > 0 {
		i := bytes.IndexByte(text, '\n')
		if i < 0 {
			lines = append(lines, text)
			break
		}
		lines = append(lines, text[:i+1])
		text = text[i+1:]
	}
	return lines
}

// diffNames returns the two file names for the diff header. A control
// character in a file name (a line break is legal in one) becomes "?": the
// header is one line, and a name must not be able to add a second one.
func diffNames(path string) (oldName, newName string) {
	path = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, path)
	if filepath.IsAbs(path) {
		return path, path
	}
	clean := filepath.ToSlash(filepath.Clean(path))
	return "a/" + clean, "b/" + clean
}

// FileDiff returns the unified diff from oldText to newText for a file that
// the Kustomize form writes (kustomize.go). Such a file is new, or is a
// generated file with a small change, so a simple method is sufficient: the
// lines that the two texts share at the start and at the end are context,
// and everything between them is one hunk. The diff is correct for any two
// texts; for texts with more than one change it is not the shortest one.
//
// exists says if the file is on disk now. When it is not, the diff makes a
// new file, in the form of git (`--- /dev/null`). It returns "" when the
// file is there and has the new text already.
func FileDiff(path string, oldText, newText []byte, exists bool) string {
	if exists && bytes.Equal(oldText, newText) {
		return ""
	}
	oldName, newName := diffNames(path)
	var oldLines [][]byte
	if exists {
		oldLines = splitLines(oldText)
	} else {
		oldName = "/dev/null"
	}
	newLines := splitLines(newText)

	// The shared lines at the start, and at the end of what is left.
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && bytes.Equal(oldLines[prefix], newLines[prefix]) {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		bytes.Equal(oldLines[len(oldLines)-1-suffix], newLines[len(newLines)-1-suffix]) {
		suffix++
	}
	before, after := min(prefix, contextLines), min(suffix, contextLines)
	removed := oldLines[prefix : len(oldLines)-suffix]
	added := newLines[prefix : len(newLines)-suffix]

	// A range with no lines has the number of the line before it as its
	// start (0 for a new file): the rule of the unified format.
	oldCount, newCount := before+len(removed)+after, before+len(added)+after
	oldStart, newStart := prefix-before+1, prefix-before+1
	if oldCount == 0 {
		oldStart--
	}
	if newCount == 0 {
		newStart--
	}

	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n@@ -%d,%d +%d,%d @@\n", oldName, newName, oldStart, oldCount, newStart, newCount)
	write := func(marker byte, lines [][]byte) {
		for _, line := range lines {
			out.WriteByte(marker)
			out.Write(line)
			if !bytes.HasSuffix(line, []byte("\n")) {
				out.WriteString("\n" + noNewline)
			}
		}
	}
	write(' ', newLines[prefix-before:prefix])
	write('-', removed)
	write('+', added)
	write(' ', newLines[len(newLines)-suffix:len(newLines)-suffix+after])
	return out.String()
}

// DiffStat counts the lines that a unified diff of one file adds and
// removes. The two header lines (`--- ` and `+++ `) are not counted.
func DiffStat(diff string) (added, removed int) {
	lines := strings.Split(diff, "\n")
	if len(lines) < 2 {
		return 0, 0
	}
	for _, line := range lines[2:] {
		switch {
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}
