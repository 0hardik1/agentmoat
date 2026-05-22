// Tests for the Explain orchestrator. Verify orchestrator tests live in
// verify_test.go.
package agentmoat

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/explainer"
)

// TestExplainListMode asserts that calling Explain with no topic returns
// the canonical envelope with Spec.Topics populated and Spec.Content empty.
func TestExplainListMode(t *testing.T) {
	t.Parallel()
	doc, err := Explain(ExplainOptions{})
	if err != nil {
		t.Fatalf("Explain: %v", err)
	}
	if doc.Kind != schema.KindExplainDocument {
		t.Errorf("Kind: got %q, want %q", doc.Kind, schema.KindExplainDocument)
	}
	if doc.Spec.Topic != "" {
		t.Errorf("Topic should be empty in list mode, got %q", doc.Spec.Topic)
	}
	if doc.Spec.Content != "" {
		t.Errorf("Content should be empty in list mode, got len=%d", len(doc.Spec.Content))
	}
	if len(doc.Spec.Topics) == 0 {
		t.Errorf("Topics list should be non-empty")
	}
}

// TestExplainTopicMode asserts every valid topic round-trips through the
// orchestrator with identical content to a direct pkg/explainer.Explain
// call. Catches accidental wrapping or post-processing in the orchestrator.
func TestExplainTopicMode(t *testing.T) {
	t.Parallel()
	for _, topic := range explainer.Topics() {
		topic := topic
		t.Run(topic, func(t *testing.T) {
			t.Parallel()
			doc, err := Explain(ExplainOptions{Topic: topic})
			if err != nil {
				t.Fatalf("Explain(%q): %v", topic, err)
			}
			direct, err := explainer.Explain(topic)
			if err != nil {
				t.Fatalf("explainer.Explain(%q): %v", topic, err)
			}
			if doc.Spec.Content != direct {
				t.Errorf("content mismatch for %q: orchestrator differs from explainer", topic)
			}
			if doc.Spec.Topic != topic {
				t.Errorf("Spec.Topic: got %q, want %q", doc.Spec.Topic, topic)
			}
		})
	}
}

// TestExplainUnknownTopic asserts unknown topics surface the explainer's
// error message verbatim (which lists every valid topic for self-correction).
func TestExplainUnknownTopic(t *testing.T) {
	t.Parallel()
	_, err := Explain(ExplainOptions{Topic: "bogus-topic"})
	if err == nil {
		t.Fatalf("expected error for unknown topic")
	}
	for _, want := range explainer.Topics() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list valid topic %q: %v", want, err)
		}
	}
}
