// agentmoat scan: classify workloads for gVisor compatibility.
//
// The workloads come from a cluster (the default) or, with -f, from manifest
// files and no cluster at all. The second form is the one a pipeline uses:
// a pull request has files, not a cluster.
//
// Exit codes (docs/exit-codes.md):
//   0  the gate passed (see --fail-on)
//   1  generic error (kubeconfig, network, a manifest that does not parse,
//      or --fail-on sandboxed on input that is not rendered)
//   2  the gate failed. With the default --fail-on incompatible: at least
//      one workload is Incompatible.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// Scan-specific flag values.
var (
	flagScanRuntimeClass   string
	flagScanNoClusterFacts bool
	flagScanFactsPath      string
	flagScanFiles          []string
	flagScanFailOn         string
)

func newScanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Classify workloads for gVisor compatibility (a cluster, or manifests with -f)",
		Long: `scan classifies each workload against the built-in compatibility rules and
prints a report. It reads the workloads from one of two places.

A cluster (the default). scan walks the cluster via client-go:

    agentmoat scan -n payments

Manifests, with -f. scan reads the files and no cluster. It builds no
Kubernetes client and reads no kubeconfig, so it runs in a pull-request job
or on a laptop with nothing installed:

    agentmoat scan -f ./k8s
    helm template ./chart | agentmoat scan -f -
    kustomize build ./overlays/prod | agentmoat scan -f -

-f takes a file, a directory (searched for *.yaml, *.yml, and *.json), or -
for standard input, and can be given more than once. It reads the six
workload kinds and any other kind with a pod template (an Argo Rollout).
Other objects (a Service, a ConfigMap) are counted and listed as not
classified. With -f, -n sets the namespace for objects that name none; it
does not filter.

A file scan reads each object as written. It does not render Helm
templates, it does not apply Kustomize overlays, it does not see what
admission adds in the cluster, and it does not check that a field name is
spelled right. Pipe rendered output in, as above, to scan what gets deployed.

The default output is a human-readable table. Use --output json for a
versioned, stable schema suitable for piping into other tools or AI agents:

    agentmoat scan --output json | jq '.spec.workloads[] | select(.compatibility=="incompatible")'

--fail-on sets when scan exits 2:

    incompatible  a workload is incompatible (the default)
    sandboxed     a workload that asks for the gVisor RuntimeClass is
                  incompatible. Use this in a pull-request check: it does
                  not fail on a CNI DaemonSet that will never move, and it
                  stops a change that breaks a sandboxed workload
    none          never; the report is the result

--fail-on sandboxed refuses (exit 1) input it can tell is not rendered, such
as a directory with a kustomization file: an overlay can add
runtimeClassName, and the answer would not describe what gets deployed.

For a cluster, the report also records cluster facts (metadata.clusterFacts):
whether the RuntimeClass exists, how many Ready nodes match its nodeSelector,
and whether those nodes are EKS Auto Mode or Bottlerocket instances that can
never run runsc. 'agentmoat plan --scan' turns them into warnings. Facts
need get/list on nodes and runtimeclasses; without that permission the scan
still succeeds and just omits them (--no-cluster-facts skips the reads).

Facts also refine one verdict: gpu-passthrough. GPU nodes are read from GPU
Feature Discovery labels (card model, driver, MIG); a workload on a card
gVisor nvproxy supports, with a driver the installed runsc lists, is
compatible, while an unsupported card or MIG is incompatible. The driver
list comes from 'agentmoat probe nvproxy --dry-run=false --output json';
pass that file with --facts and the scan reads it instead of the cluster.
--facts works with -f too.

scan is strictly read-only and never mutates cluster state.`,
		RunE: runScan,
	}
	cmd.Flags().StringArrayVarP(&flagScanFiles, "filename", "f", nil,
		"scan manifests and no cluster: a file, a directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&flagScanFailOn, "fail-on", string(agentmoat.FailOnIncompatible),
		"when to exit 2: incompatible | sandboxed | none")
	cmd.Flags().StringVar(&flagScanRuntimeClass, "runtime-class", "gvisor",
		"the sandbox RuntimeClass: inspected for metadata.clusterFacts, and the class --fail-on sandboxed looks for")
	cmd.Flags().BoolVar(&flagScanNoClusterFacts, "no-cluster-facts", false,
		"do not read nodes and the RuntimeClass; omit metadata.clusterFacts")
	cmd.Flags().StringVar(&flagScanFactsPath, "facts", "",
		"load cluster facts from a saved PreflightReport or ScanReport (e.g. the output of 'probe nvproxy') instead of reading the cluster")
	return cmd
}

func runScan(cmd *cobra.Command, _ []string) error {
	// Parse --output and --fail-on first, so a typing error fails before
	// any file or cluster is read.
	format, err := output.Parse(flagOutput)
	if err != nil {
		return err
	}
	failOn, err := agentmoat.ParseFailOn(flagScanFailOn)
	if err != nil {
		return err
	}

	// Build the render options once. NoColor is the OR of the explicit
	// flag and the NO_COLOR env (the renderer also checks the env, but
	// we OR here so the env decision is visible at the CLI level too).
	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}

	// --quiet swaps stderr for io.Discard so the orchestrator's progress
	// lines vanish without any orchestrator-side branching. The
	// orchestrator option docs (pkg/agentmoat/options.go) explicitly
	// name io.Discard as the silencing path.
	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	opts, err := scanOptions(cmd, stderr)
	if err != nil {
		return err
	}

	report, err := agentmoat.Scan(context.Background(), opts)
	if err != nil {
		return err
	}

	// Render to stdout. Stderr already received progress messages.
	if err := output.Render(report, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// Exit-code shaping. The decision is the library's (EvaluateGate), so
	// every surface gates the same way; this command only turns it into
	// an exit code. A gate error (it could not answer) is returned and
	// becomes exit 1, after the report was printed: the report is still
	// correct about the files it read.
	gate, err := agentmoat.EvaluateGate(report, failOn)
	if err != nil {
		return err
	}
	for _, line := range gate.Messages {
		_, _ = fmt.Fprintln(stderr, line)
	}
	if gate.Failed {
		exitCode = 2
	}
	return nil
}

// scanOptions maps the flags to ScanOptions for the cluster form or, when
// -f was given, the manifest form.
func scanOptions(cmd *cobra.Command, stderr io.Writer) (agentmoat.ScanOptions, error) {
	opts := agentmoat.ScanOptions{
		Namespaces:       flagNamespaces,
		LabelSelector:    flagLabelSelector,
		RulesYAMLPath:    flagRulesYAML,
		RuntimeClassName: flagScanRuntimeClass,
		FactsPath:        flagScanFactsPath,
		Stderr:           stderr,
	}

	if len(flagScanFiles) == 0 {
		// The cluster form. Default behavior: if no namespace is
		// specified, scan all namespaces.
		opts.KubeconfigPath = flagKubeconfig
		opts.Context = flagContext
		opts.AllNamespaces = flagAllNamespaces || len(flagNamespaces) == 0
		opts.IncludeSystem = flagIncludeSystem
		opts.SkipClusterFacts = flagScanNoClusterFacts
		return opts, nil
	}

	// The manifest form. No kubeconfig field is set, on purpose: the
	// library ignores them in this form, and leaving them empty here means
	// a reader of this function can see that no cluster setting gets in.
	opts.ManifestPaths = flagScanFiles
	for _, path := range flagScanFiles {
		if path != scanner.StdinPath {
			continue
		}
		in := cmd.InOrStdin()
		if isTerminal(in) {
			// Without this check the command would sit and wait for
			// typed YAML, and look like it hangs.
			return opts, fmt.Errorf("-f - reads manifests from standard input, and standard input is a terminal; " +
				"pipe the manifests in, for example: helm template ./chart | agentmoat scan -f -")
		}
		opts.Stdin = in
		break
	}

	// Flags that only mean something for a cluster. Say that they were
	// not used; an error would be unkind to a wrapper script that always
	// passes --context.
	var unused []string
	for _, name := range []string{"kubeconfig", "context", "all-namespaces", "include-system", "no-cluster-facts"} {
		if cmd.Flags().Changed(name) {
			unused = append(unused, "--"+name)
		}
	}
	if len(unused) > 0 {
		_, _ = fmt.Fprintf(stderr, "note: -f scans manifests and no cluster; not used: %s\n", strings.Join(unused, ", "))
	}
	return opts, nil
}

// isTerminal reports whether r is an interactive terminal. A pipe, a file,
// /dev/null, and a test's bytes.Buffer are not.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}
