// Tests for the per-rule documentation: every built-in classifier rule has
// a title and an explanation file.
//
// Two outputs depend on that. `explain workload` prints the explanation,
// and the SARIF output puts the title and the explanation in the rule entry
// that a code scanning service shows next to each alert. A rule that is
// added to the classifier and not here would show its bare ID there.

package docs_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/docs"
	"github.com/0hardik1/agentmoat/pkg/classifier"
)

func TestEveryBuiltInRuleHasATitleAndAnExplanation(t *testing.T) {
	t.Parallel()
	registry := classifier.NewRegistry()
	classifier.RegisterBuiltins(registry)
	var ids []string
	for _, rule := range registry.Rules() {
		ids = append(ids, rule.ID)
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		t.Fatal("the default registry has no rules")
	}

	for _, id := range ids {
		if title := docs.RuleTitle(id); title == id || title == "" {
			t.Errorf("rule %q has no title in docs/rules.go", id)
		}
		explanation := docs.RuleExplanation(id)
		if explanation == "" {
			t.Errorf("rule %q has no docs/explanations/%s.md", id, id)
			continue
		}
		// The files differ by severity: an incompatible rule has "What to
		// do instead", a review rule has "How to validate", a hint has
		// "How to measure". All of them start with a "Why" section, and
		// the SARIF output takes its one-line rule description from the
		// first paragraph under it.
		if !strings.HasPrefix(explanation, "## Why") {
			t.Errorf("docs/explanations/%s.md must start with a \"## Why ...\" section", id)
		}
		if strings.Count(explanation, "\n## ") < 2 {
			t.Errorf("docs/explanations/%s.md must have a section on what to do, after the \"Why\" section", id)
		}
	}

	// And the other way: no title for a rule that does not exist.
	if got := docs.RuleIDs(); strings.Join(got, ",") != strings.Join(ids, ",") {
		t.Errorf("docs.RuleIDs() = %v\nthe classifier has %v", got, ids)
	}
}

func TestRuleLookupFallbacks(t *testing.T) {
	t.Parallel()
	if got := docs.RuleTitle("rule-from-the-future"); got != "rule-from-the-future" {
		t.Errorf("RuleTitle of an unknown rule = %q, want the ID", got)
	}
	for _, id := range []string{"", "rule-from-the-future", "../embed.go"} {
		if got := docs.RuleExplanation(id); got != "" {
			t.Errorf("RuleExplanation(%q) = %d bytes, want none", id, len(got))
		}
	}
}
