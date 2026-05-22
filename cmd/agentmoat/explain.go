// agentmoat explain: print embedded gVisor/RuntimeClass documentation.
//
// The command is purely offline (no Kubernetes client work) and never
// mutates anything. With a positional topic argument, it prints the
// embedded markdown for that topic. Without one, it lists the available
// topics so the operator can pick.
//
// Honors the global --output flag:
//   - table (default): raw markdown for topic mode, plain topic list for
//     list mode. Pipes cleanly into pagers like `less`.
//   - json / yaml: a stable ExplainDocument envelope so the MCP server
//     and CI scripts can consume the same vocabulary structurally.
//
// Exit codes (docs/exit-codes.md):
//   0  topic printed (or list shown)
//   1  unknown topic; stderr names every valid topic

package main

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

func newExplainCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "explain [topic]",
		Short: "Print embedded gVisor / RuntimeClass documentation",
		Long: `explain prints agentmoat's embedded educational content.

Run without an argument to see the list of available topics:

    agentmoat explain

Run with a topic name to print that topic's content:

    agentmoat explain runtimeclass
    agentmoat explain gvisor
    agentmoat explain threat-model
    agentmoat explain performance
    agentmoat explain compatibility

Topic lookup is case-insensitive. The text is sourced from docs/*.md in
the agentmoat repo, embedded into the binary at build time so the CLI and
the docs site never drift. Use --output json/yaml to get a structured
envelope suitable for MCP tool output.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runExplain,
	}
}

func runExplain(cmd *cobra.Command, args []string) error {
	format, err := output.Parse(flagOutput)
	if err != nil {
		return err
	}

	// renderOpts plumbing mirrors the other runX functions. --quiet is
	// effectively a no-op for explain (the orchestrator is offline and
	// silent), but we honor the flag at this layer so the global semantics
	// stay consistent: setting --quiet never makes anything noisier.
	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}

	opts := agentmoat.ExplainOptions{}
	if len(args) == 1 {
		opts.Topic = args[0]
	}

	doc, err := agentmoat.Explain(opts)
	if err != nil {
		// Return the error verbatim: it already lists every valid topic so
		// the user can self-correct. The root Execute() prints the message
		// to stderr and exits 1.
		return err
	}

	return output.Render(doc, format, cmd.OutOrStdout(), renderOpts)
}
