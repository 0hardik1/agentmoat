// Tests for pkg/explainer.
//
// Each subtest case name maps 1:1 to the Phase 3 handoff so the test output
// reads like a checklist. The fixtures live in /docs/ and are reached via
// the docs.FS embed, so these tests are hermetic: no testdata copy, no
// runtime file I/O.
package explainer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/docs"
)

func TestExplainer(t *testing.T) {
	t.Run("lists_all_topics", func(t *testing.T) {
		got := Topics()
		want := []string{
			"compatibility",
			"gvisor",
			"performance",
			"runtimeclass",
			"threat-model",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Topics() = %v, want %v", got, want)
		}
	})

	t.Run("each_topic_returns_nonempty_content", func(t *testing.T) {
		for _, topic := range Topics() {
			topic := topic
			t.Run(topic, func(t *testing.T) {
				content, err := Explain(topic)
				if err != nil {
					t.Fatalf("Explain(%q) returned error: %v", topic, err)
				}
				if len(content) <= 200 {
					t.Errorf("Explain(%q) content length = %d, want > 200",
						topic, len(content))
				}
				if !strings.HasPrefix(content, "# ") {
					// Show just the first line in the failure so the diff
					// is readable in CI output.
					firstLine := content
					if idx := strings.IndexByte(content, '\n'); idx >= 0 {
						firstLine = content[:idx]
					}
					t.Errorf("Explain(%q) content does not start with '# ' "+
						"markdown title; first line = %q", topic, firstLine)
				}
			})
		}
	})

	t.Run("unknown_topic_returns_error", func(t *testing.T) {
		_, err := Explain("bogus")
		if err == nil {
			t.Fatal("Explain(\"bogus\") returned nil error, want non-nil")
		}
		msg := err.Error()
		for _, topic := range Topics() {
			if !strings.Contains(msg, topic) {
				t.Errorf("error message does not list topic %q; got: %s",
					topic, msg)
			}
		}
	})

	t.Run("case_insensitive_topic_lookup", func(t *testing.T) {
		canonical, err := Explain("runtimeclass")
		if err != nil {
			t.Fatalf("Explain(\"runtimeclass\") returned error: %v", err)
		}

		variants := []string{
			"RuntimeClass",
			"runtimeclass",
			"RUNTIMECLASS",
			" runtimeclass ",
		}
		for _, v := range variants {
			v := v
			t.Run(v, func(t *testing.T) {
				got, err := Explain(v)
				if err != nil {
					t.Fatalf("Explain(%q) returned error: %v", v, err)
				}
				if got != canonical {
					t.Errorf("Explain(%q) content differs from canonical "+
						"lookup", v)
				}
			})
		}
	})

	t.Run("embed_fs_smoke", func(t *testing.T) {
		for _, fname := range topicFiles {
			if _, err := docs.FS.ReadFile(fname); err != nil {
				t.Errorf("embedded doc missing: %s: %v", fname, err)
			}
		}
	})
}
