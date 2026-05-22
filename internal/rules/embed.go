// Package rules embeds the classifier rule YAML files so binaries can
// surface them without depending on the filesystem layout at runtime.
//
// Today only one file is embedded: gvisor.yaml, which the MCP server
// returns to clients that read the agentmoat://compatibility-rules
// resource. The classifier loads its rule overrides via file path
// (not from this embed.FS); embedding here is purely for the MCP-side
// read-only resource surface.
package rules

import _ "embed"

//go:embed gvisor.yaml
var gvisorYAML []byte

// GVisorYAML returns the bytes of internal/rules/gvisor.yaml as bundled
// at build time. Callers must not mutate the returned slice.
func GVisorYAML() []byte {
	return gvisorYAML
}
