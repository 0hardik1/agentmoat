// agentmoat version: prints the binary version and build provenance.
//
// Version + GitSHA are injected at build time via -ldflags, with a fallback
// to the Go build record for `go install` and plain `go build`. See main.go
// and pkg/agentmoat/version.go.

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print agentmoat version and build info",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "agentmoat %s (%s)\n", Version, GitSHA)
		},
	}
}
