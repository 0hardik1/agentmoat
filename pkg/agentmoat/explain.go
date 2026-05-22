// Package agentmoat: Explain orchestrator.
//
// Explain is purely offline: it reads from `pkg/explainer` (which embeds
// `docs/*.md` at build time) and wraps the result in the versioned
// ExplainDocument envelope so the renderer and the MCP server treat it like
// every other agentmoat output. There is no Kubernetes client work here.
package agentmoat

import (
	"fmt"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/explainer"
)

// Explain returns an ExplainDocument for the requested topic. With an empty
// topic the envelope's Spec.Topics is populated and Spec.Content stays empty
// (list mode). With a non-empty topic, Spec.Topic and Spec.Content are filled
// (topic mode) AND Spec.Topics is still populated so JSON consumers see the
// full vocabulary in a single round-trip.
//
// Topic lookup is case-insensitive and trims surrounding whitespace; an
// unknown topic returns an error whose message lists every valid topic so the
// operator can self-correct.
func Explain(opts ExplainOptions) (*schema.ExplainDocument, error) {
	doc := schema.NewExplainDocument()
	doc.Metadata = schema.ExplainMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		AgentmoatVersion: Version,
	}
	doc.Spec.Topics = explainer.Topics()

	if opts.Topic == "" {
		// List mode: return the available topics, no content body.
		return doc, nil
	}

	content, err := explainer.Explain(opts.Topic)
	if err != nil {
		// Pass the explainer's error through unchanged: it already names
		// the valid topics.
		return nil, fmt.Errorf("explain: %w", err)
	}
	doc.Spec.Topic = opts.Topic
	doc.Spec.Content = content
	return doc, nil
}
