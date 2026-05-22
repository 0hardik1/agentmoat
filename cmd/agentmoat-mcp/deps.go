// Deps is the dependency-injection bundle for the MCP server.
//
// The handlers don't reach into globals; everything they need is on Deps.
// In production main.go fills this with os.Stderr and the build-time
// Version; in tests, the unit suite overrides Stderr (io.Discard) and
// supplies the optional injection fields (KubeClient, RestConfig,
// ExecRunner) so handlers run against a fake clientset.
//
// Keeping the injection points here (rather than scattered as function
// parameters on every handler) means a new tool only needs to know about
// one bag, and a future "real K8s client factory" tweak (e.g. exec
// plugin support) is a single-file change.
package main

import (
	"io"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/0hardik1/agentmoat/pkg/verifier"
)

// Deps is the dependency bundle for tool/resource/prompt handlers.
type Deps struct {
	// Stderr is the writer used for human-readable progress messages
	// from the orchestrator. Defaults to os.Stderr in main.go; tests
	// set it to io.Discard. Never write to stdout from here.
	Stderr io.Writer

	// Version is the build-time version string, threaded into the MCP
	// server's InitializeResult so clients can see what they connected to.
	Version string

	// KubeClient, RestConfig, ExecRunner are optional injection points
	// used by the unit test suite. Production main.go leaves them nil so
	// each tool handler triggers the standard kube.NewClient path inside
	// pkg/agentmoat. When set, they short-circuit that loader and make
	// the handlers run entirely against an in-memory fake clientset.
	KubeClient kubernetes.Interface
	RestConfig *rest.Config
	ExecRunner verifier.ExecRunner
}
