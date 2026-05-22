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

// These two are overridden at build time via -ldflags. Defaults give a
// useful answer in `go run` scenarios.
var (
	Version = "dev"
	GitSHA  = "unknown"
)

// init copies our ldflag-injected Version into the library package so
// scan reports carry the same string that `agentmoat version` prints.
// We could `-X` both vars from the Makefile, but a single source of truth
// at the binary boundary keeps the Makefile simpler.
func init() {
	agentmoat.Version = Version
}

func main() {
	// Execute returns the chosen exit code. Documented in docs/exit-codes.md:
	//   0 success
	//   1 generic error
	//   2 compatibility issues found
	//   3 partial apply
	//   4 verify failed
	code := Execute()
	if code != 0 {
		// Already printed a message via Cobra/our error handler; nothing
		// to add here.
		fmt.Fprintln(os.Stderr, "")
	}
	os.Exit(code)
}
