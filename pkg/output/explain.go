// Package output: ExplainDocument renderer.
//
// This file holds the markdown renderer for `agentmoat explain <topic>` and
// the list-mode renderer for `agentmoat explain` (no argument). The renderer
// was extracted out of table.go because its mechanics (markdown vs columnar
// rows) are different enough that mixing them blurred both files.
//
// Behavior by mode:
//
//   - useColor == true and Spec.Content != "": run the markdown through
//     glamour.Render(content, "auto"). On glamour error, fall back to raw
//     markdown so a corrupt or unusual topic file still prints something
//     useful and the command does not fail.
//   - useColor == false and Spec.Content != "": print the raw markdown
//     verbatim. Preserves the pre-existing "piped output is the markdown
//     bytes" contract that scripts and the e2e suite rely on.
//   - Spec.Content == "" (list mode): print a header line plus one topic
//     per line. Header uses the Bold style; when useColor is false, Bold
//     is a no-op zero-value Style and the header reads as plain text.

package output

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/glamour"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// renderExplainDocumentTable prints either the topic content (markdown,
// glamour-styled when useColor) or the list of available topics. Both
// shapes are produced by the agentmoat.Explain orchestrator.
//
// useColor only affects topic-mode rendering: glamour kicks in only when
// the caller is sure the output writer is a styled TTY. In all other
// contexts (--no-color, NO_COLOR, piped output) we emit raw markdown so
// pagers, jq, and the existing e2e jq path stay happy.
func renderExplainDocumentTable(doc *schema.ExplainDocument, w io.Writer, useColor bool) error {
	if doc.Spec.Content != "" {
		return writeTopicContent(doc.Spec.Content, w, useColor)
	}

	// List mode: a short header plus one topic name per line. The header
	// names the binary so an operator who pipes `agentmoat explain` into
	// less/grep still has the context.
	s := NewStyles(useColor)
	if _, err := fmt.Fprintln(w, s.Bold.Render("agentmoat explain: available topics")); err != nil {
		return err
	}
	for _, t := range doc.Spec.Topics {
		if _, err := fmt.Fprintf(w, "  %s\n", t); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(w, ""); err != nil {
		return err
	}
	_, err := fmt.Fprintln(w, "Run `agentmoat explain <topic>` to print the topic content.")
	return err
}

// writeTopicContent emits the topic markdown to w. When useColor is true it
// runs the content through glamour with the "auto" style (light/dark
// detection). Any glamour error degrades to raw markdown rather than
// failing the whole command, because the user's intent ("show me the
// topic") is satisfied as long as the bytes reach the terminal.
//
// In raw mode we ensure a trailing newline so the shell prompt does not
// glue itself to the last line of content. Glamour already terminates its
// output with a newline so we only append in the raw branch.
func writeTopicContent(content string, w io.Writer, useColor bool) error {
	if useColor {
		rendered, err := glamour.Render(content, "auto")
		if err == nil {
			_, werr := fmt.Fprint(w, rendered)
			return werr
		}
		// Glamour failed (rare: usually a malformed style or a broken
		// terminfo). Fall through to raw markdown rather than erroring;
		// the user will still see the content they asked for.
	}

	// Raw markdown path: print verbatim, append a newline if missing.
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	_, err := fmt.Fprint(w, content)
	return err
}
