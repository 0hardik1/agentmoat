// agentmoat explain: print embedded gVisor/RuntimeClass documentation.
//
// The root command (this file) is purely offline (no Kubernetes client
// work) and never mutates anything. With a positional topic argument, it
// prints the embedded markdown for that topic. Without one, it lists the
// available topics so the operator can pick.
//
// Two online subcommands are attached here but live in their own files:
//   - `agentmoat explain namespace <ns>`     (explain_namespace.go)
//   - `agentmoat explain workload <ns>/<n>`  (explain_workload.go)
// Those make the same read-only API calls as `agentmoat scan` and emit
// the same ExplainDocument envelope with a populated Spec.Namespace.
//
// Honors the global --output flag:
//   - table (default): raw markdown for topic mode, plain topic list for
//     list mode, deep per-workload view for the online subcommands.
//     Pipes cleanly into pagers like `less`.
//   - json / yaml: a stable ExplainDocument envelope so the MCP server
//     and CI scripts can consume the same vocabulary structurally.
//
// Exit codes (docs/exit-codes.md):
//   0  topic printed (or list shown), or namespace/workload compatible
//   1  unknown topic, or generic kube error; stderr explains
//   2  online subcommand: at least one workload is Incompatible

package main

import (
	"context"
	"os"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

func newExplainCmd() *cobra.Command {
	cmd := &cobra.Command{
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
envelope suitable for MCP tool output.

Two subcommands extend explain into the live cluster:

    agentmoat explain namespace <ns>
    agentmoat explain workload <ns>/<name>

Both contact the cluster (read-only, like 'agentmoat scan') and produce a
per-workload deep explanation: which rule fired, what concrete evidence in
the PodSpec triggered it, and operator-facing prose on why each finding
matters.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runExplain,
	}

	// Deep-mode subcommands. Both inherit the global K8s flags
	// (--kubeconfig, --context, -l, --rules, --include-system) so the
	// kubectl muscle memory carries over. Their positional argument is
	// the source of truth for namespace selection; -n is ignored.
	cmd.AddCommand(newExplainNamespaceCmd())
	cmd.AddCommand(newExplainWorkloadCmd())
	return cmd
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

	// Topic mode is offline; ctx is unused by the orchestrator but kept
	// on the signature so the deep-mode subcommands can share it.
	doc, err := agentmoat.Explain(context.Background(), opts)
	if err != nil {
		// Return the error verbatim: it already lists every valid topic so
		// the user can self-correct. The root Execute() prints the message
		// to stderr and exits 1.
		return err
	}

	return output.Render(doc, format, cmd.OutOrStdout(), renderOpts)
}
