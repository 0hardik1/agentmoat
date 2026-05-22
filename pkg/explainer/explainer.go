// Package explainer renders the agentmoat educational content for the
// `agentmoat explain <topic>` subcommand. Content is loaded from the
// `docs.FS` embed, keyed by a small, hand-curated topic-to-filename map so
// the CLI exposes a stable vocabulary even if the underlying filenames
// evolve (e.g. "runtimeclass" -> "runtimeclass-101.md").
//
// Design notes for a K8s engineer reading this for the first time:
//
//   - Embedding the markdown (rather than reading files at runtime) means
//     the binary is hermetic: `agentmoat explain` works inside an
//     air-gapped cluster or a scratch container with no filesystem.
//   - The map is the source of truth for which topics the CLI advertises.
//     Adding a new topic is a two-line change: add the .md file to docs/
//     and add the entry below. The `embed_fs_smoke` test catches the case
//     where the .md file is missing.
//   - Topic lookups are case- and whitespace-insensitive so operators do
//     not need to remember the exact spelling.
package explainer

import (
	"fmt"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/docs"
)

// topicFiles is the canonical mapping from CLI topic names to embedded
// markdown filenames. Add a new topic by adding an entry here AND a matching
// docs/<file>.md.
var topicFiles = map[string]string{
	"compatibility": "compatibility-checklist.md",
	"gvisor":        "gvisor-101.md",
	"performance":   "performance.md",
	"runtimeclass":  "runtimeclass-101.md",
	"threat-model":  "threat-model.md",
}

// Topics returns the list of available topic names, sorted lexicographically.
// The CLI uses this list when the user invokes `explain` with no positional
// argument, and the unknown-topic error message lists every entry so the
// user can self-correct.
func Topics() []string {
	out := make([]string, 0, len(topicFiles))
	for k := range topicFiles {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Explain returns the raw markdown content for the named topic. The lookup is
// case-insensitive (`RuntimeClass`, `runtimeclass`, and `RUNTIMECLASS` all
// resolve to the same content) and tolerates surrounding whitespace, so
// operators do not need to remember an exact spelling. An unknown topic
// returns an error whose message lists every valid topic so the caller can
// surface a self-correcting hint.
func Explain(topic string) (string, error) {
	key := strings.ToLower(strings.TrimSpace(topic))
	filename, ok := topicFiles[key]
	if !ok {
		return "", fmt.Errorf("unknown topic %q. Available topics: %s",
			topic, strings.Join(Topics(), ", "))
	}
	data, err := docs.FS.ReadFile(filename)
	if err != nil {
		return "", fmt.Errorf("reading embedded doc %s: %w", filename, err)
	}
	return string(data), nil
}
