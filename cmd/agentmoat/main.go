// Command agentmoat is the CLI front-end for the agentmoat toolkit.
//
// The CLI is intentionally a thin shell over the pkg/agentmoat library:
// the real logic (scanning, classification, future planning/applying)
// lives in pkg/, so a third party can `import` agentmoat and call it
// without spawning this binary.
//
// The build pipeline injects version metadata via -ldflags:
//
//	go build -ldflags "-X main.Version=$(VERSION) -X main.GitSHA=$(GIT_SHA)" ./cmd/agentmoat
//
// See the Makefile's `build` target for the canonical invocation.
package main

import (
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

// These two are overridden at build time via -ldflags. They are empty by
// default on purpose: an empty value tells ResolveBuildInfo to read the
// version and commit that the Go toolchain recorded in the binary, so a
// plain `go install ...@v0.2.0` reports 0.2.0 and not "dev".
var (
	Version = ""
	GitSHA  = ""
)

// init resolves the version and commit once (ldflags first, then the Go
// build record, then "dev"/"unknown") and stores the version in the library
// package, so scan reports carry the same string that `agentmoat version`
// prints. cmd/agentmoat-mcp makes the same call.
func init() {
	Version, GitSHA = agentmoat.ResolveBuildInfo(Version, GitSHA)
}

func main() {
	// Execute returns the chosen exit code. Documented in docs/exit-codes.md:
	//   0 success
	//   1 generic error
	//   2 compatibility issues found
	//   3 partial apply
	//   4 verify failed
	//   5 cluster not ready (preflight, probe, apply)
	code := Execute()
	if code != 0 {
		// Already printed a message via Cobra/our error handler; nothing
		// to add here.
		fmt.Fprintln(os.Stderr, "")
	}
	os.Exit(code)
}
