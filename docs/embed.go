// Package docs embeds the agentmoat markdown documentation files for use by
// the `agentmoat explain` subcommand. The same files are read directly by
// the docs site, so embedding them (rather than re-templating) ensures the
// CLI output and the published docs cannot drift.
//
// The `//go:embed *.md` directive captures every Markdown file at the docs/
// directory root (not subdirectories like docs/schemas/). New topic files
// added here are automatically available to the explainer.
package docs

import "embed"

// FS is the embedded read-only filesystem rooted at docs/. Filenames are
// flat (e.g. "runtimeclass-101.md"); no directory prefix.
//
//go:embed *.md
var FS embed.FS
