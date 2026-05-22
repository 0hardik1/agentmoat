// Package docs embeds the agentmoat markdown documentation files for use by
// the `agentmoat explain` subcommand. The same files are read directly by
// the docs site, so embedding them (rather than re-templating) ensures the
// CLI output and the published docs cannot drift.
//
// The `//go:embed *.md explanations/*.md` directive captures every Markdown
// file at the docs/ directory root, plus the per-rule operator-facing prose
// under docs/explanations/ that the deep `agentmoat explain namespace` and
// `agentmoat explain workload` commands inline into their output. New topic
// files added in either location are automatically available to the
// explainer.
package docs

import "embed"

// FS is the embedded read-only filesystem rooted at docs/. Top-level topics
// are flat (e.g. "runtimeclass-101.md"); per-rule prose lives under the
// "explanations/" subdirectory (e.g. "explanations/host-network.md").
//
//go:embed *.md explanations/*.md
var FS embed.FS
