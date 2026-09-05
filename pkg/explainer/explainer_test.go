// Tests for pkg/explainer.
//
// Each test function maps 1:1 to a case in the Phase 3 handoff so the test
// output reads like a checklist. Splitting into top-level Test* functions
// (rather than one parent with t.Run subtests) keeps each function under
// the gocyclo threshold while preserving the same coverage.
//
// The fixtures live in /docs/ and are reached via the docs.FS embed, so
// these tests are hermetic: no testdata copy, no runtime file I/O.
package explainer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/docs"
)

// TestListsAllTopics pins the topic vocabulary. The hard-coded want list is
// deliberate: a missing embedded doc that quietly drops from the map gets
// caught here, not at runtime when an operator asks for it.
func TestListsAllTopics(t *testing.T) {
	got := Topics()
	want := []string{
		"compatibility",
		"gpu",
		"gvisor",
		"performance",
		"preflight",
		"runtimeclass",
		"threat-model",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Topics() = %v, want %v", got, want)
	}
}

// TestEachTopicReturnsNonemptyContent table-drives over every topic and
// asserts the content is at least minimally well-formed: > 200 bytes and
// starts with a `# ` markdown title.
func TestEachTopicReturnsNonemptyContent(t *testing.T) {
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
				firstLine := content
				if idx := strings.IndexByte(content, '\n'); idx >= 0 {
					firstLine = content[:idx]
				}
				t.Errorf("Explain(%q) content does not start with '# ' "+
					"markdown title; first line = %q", topic, firstLine)
			}
		})
	}
}

// TestUnknownTopicReturnsError asserts the error message lists every valid
// topic so the user can self-correct without re-running help.
func TestUnknownTopicReturnsError(t *testing.T) {
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
}

// TestCaseInsensitiveTopicLookup confirms that case folding and whitespace
// trimming all resolve to the same content. Operators muscle-memorying
// "RuntimeClass" should not get an unknown-topic error.
func TestCaseInsensitiveTopicLookup(t *testing.T) {
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
				t.Errorf("Explain(%q) content differs from canonical lookup", v)
			}
		})
	}
}

// TestEmbedFSSmoke verifies the embed.FS actually contains the files the
// topic map references. Catches the "added a topic, forgot to create the
// markdown" failure mode at test time rather than at runtime.
func TestEmbedFSSmoke(t *testing.T) {
	for _, fname := range topicFiles {
		if _, err := docs.FS.ReadFile(fname); err != nil {
			t.Errorf("embedded doc missing: %s: %v", fname, err)
		}
	}
}
