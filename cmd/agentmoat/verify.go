// agentmoat verify: audit a previously-applied MigrationPlan against the
// live cluster, or (with -f) the workloads of manifest files that ask for
// the RuntimeClass. Read-only.
//
// Exit codes (docs/exit-codes.md), from agentmoat.VerifyExitCode:
//   0  every step's live state matches the plan
//   1  generic error (kubeconfig, plan file missing, etc.). With -f, also
//      input that verify refuses, and a report with no failure in which a
//      workload could not be checked
//   4  at least one step reports mismatch or error
//
// With --wait, verify looks again until every step is ok or the time ends.
// This is the form for a pipeline, directly after `apply --dry-run=false`:
// one look at that moment finds a rollout in progress and reports a
// mismatch for a migration that is correct. With a wait, a step is ok only
// when its pods are also Ready and stay Ready, so exit 4 after a wait means
// "roll back", not "look again later".
//
// The in-pod probe (--in-pod-probe) opts into a per-step `kubectl exec`
// that greps for distinctive gVisor markers in dmesg, /proc/cmdline, and
// uname output. It needs the `pods/exec` permission; skip it in CI loops
// where the field-only check is enough.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// flag* identifiers stay in root.go for the global flags; subcommand-local
// flags live in the subcommand's own file (mirrors plan.go / apply.go).
var (
	flagVerifyPlanPath     string
	flagVerifyFiles        []string
	flagVerifyRuntimeClass string
	flagVerifyInPodProbe   bool
	flagVerifyWait         time.Duration
)

func newVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify (--plan <file> | -f <manifests>)",
		Short: "Check that workloads run in the gVisor RuntimeClass: the steps of a plan, or the workloads of manifest files",
		Long: `verify reads a MigrationPlan from disk and inspects the live
cluster to confirm every step actually landed:

  - For each PlanStep, the verifier resolves the target workload and
    reads each backing pod's .spec.runtimeClassName.
  - The result for each step is ok / mismatch / error. The bucketed
    summary drives the exit code (4 on any non-ok, per docs/exit-codes.md).

With --in-pod-probe, the verifier also execs a small script inside one
Running pod per step and asserts the stdout contains gVisor markers.
This requires the 'pods/exec' permission; leave it off in CI where the
field-only check is enough.

With --wait <duration> (for example --wait 3m), verify looks again every
few seconds until every step is ok or the time ends. Use it in a pipeline
directly after 'apply --dry-run=false', when the rollout is in progress:

  agentmoat apply --plan plan.json --dry-run=false
  agentmoat verify --plan plan.json --wait 3m || agentmoat rollback --plan plan.json --dry-run=false

With a wait, a step is ok only when its pods are also Ready and stay Ready
for 10 seconds with no restart. A pod that has the RuntimeClass and stops at
start (the program needs something that gVisor does not have) is a mismatch,
with the reason in the message. A workload that has no pods now (replicas: 0,
a CronJob between two runs) is checked on its pod template and does not use
the wait time. A Job that failed is the exception: it is a mismatch.
Progress goes to stderr; -q removes it.

After a change that came through a repository (-f <file or directory>):

There is no plan then: the manifest files say which workloads must run in
the sandbox. With -f, verify reads the files as 'scan -f' does, and checks
in the cluster each workload whose pod spec asks for the RuntimeClass
(--runtime-class, default gvisor). The other workloads are not looked at.

  agentmoat verify -f ./k8s -n shop --wait 5m
  kustomize build ./overlays/prod | agentmoat verify -f - --wait 5m

  - The input must be the objects as they were deployed. Input that is not
    rendered (a Kustomize base and its overlay patches) is refused: a name
    in it is not always the name in the cluster. Render it, and give - for
    standard input.
  - -n is the namespace of the objects that name none: the namespace that
    they were deployed to. verify refuses a workload with no namespace.
  - A kind that verify has no reader for (an Argo Rollout) is checked on
    the pods that the selector in its manifest selects. With no such pods,
    or with no selector, verify cannot tell, and says so: the workload is
    listed as not checked, and the exit code is then 1, not 0.
  - A Job or a Pod that is not in the cluster is listed as not checked too:
    it can be gone after a deploy that worked (a time to live, a hook that
    its tool deletes). For a Helm chart, 'helm template --no-hooks' keeps
    the hooks out of the input.
  - No workload that asks for the class is an error (exit code 1): the
    usual cause is the wrong input.

Exit codes (docs/exit-codes.md): 0 each workload is ok; 4 one or more is a
mismatch or an error (with --wait, the rollback signal); 1 an error, or a
workload that could not be checked.

verify is strictly read-only and never mutates the cluster.`,
		RunE: runVerify,
	}
	cmd.Flags().StringVar(&flagVerifyPlanPath, "plan", "",
		"path to the MigrationPlan file (JSON or YAML). This or -f is required.")
	cmd.Flags().StringArrayVarP(&flagVerifyFiles, "filename", "f", nil,
		"check the workloads of these manifests that ask for the RuntimeClass: a file, a directory, or - for stdin (repeatable)")
	cmd.Flags().StringVar(&flagVerifyRuntimeClass, "runtime-class", "gvisor",
		"with -f: the RuntimeClass name to look for")
	cmd.Flags().BoolVar(&flagVerifyInPodProbe, "in-pod-probe", false,
		"exec a gVisor-detection probe inside one Running pod per step (requires pods/exec)")
	cmd.Flags().DurationVar(&flagVerifyWait, "wait", 0,
		"look again until every step is ok and its pods stay Ready, for at most this long (for example 3m). 0 looks one time")
	return cmd
}

// verifySource checks that the command has one source for the workloads to
// check (a plan or manifests), and fills the options of the manifest form.
func verifySource(cmd *cobra.Command, opts *agentmoat.VerifyOptions) error {
	switch {
	case flagVerifyPlanPath == "" && len(flagVerifyFiles) == 0:
		return fmt.Errorf("verify needs the workloads to check: --plan <plan file>, or -f <manifest file or directory>")
	case flagVerifyPlanPath != "" && len(flagVerifyFiles) > 0:
		return fmt.Errorf("verify takes --plan or -f, not the two: a plan names its own workloads")
	case flagVerifyPlanPath != "":
		if cmd.Flags().Changed("runtime-class") {
			return fmt.Errorf("--runtime-class is for -f; with --plan each step of the plan has its class")
		}
		opts.PlanPath = flagVerifyPlanPath
		return nil
	}

	if len(flagNamespaces) > 1 {
		return fmt.Errorf("verify -f takes one namespace (the namespace of the objects that name none), not %d: %s",
			len(flagNamespaces), strings.Join(flagNamespaces, ", "))
	}
	if len(flagNamespaces) == 1 {
		opts.Namespace = flagNamespaces[0]
	}
	opts.ManifestPaths = flagVerifyFiles
	opts.LabelSelector = flagLabelSelector
	opts.RuntimeClassName = flagVerifyRuntimeClass
	for _, path := range flagVerifyFiles {
		if path != scanner.StdinPath {
			continue
		}
		in := cmd.InOrStdin()
		if isTerminal(in) {
			// Without this check the command would sit and wait for typed
			// YAML, and look like it hangs.
			return fmt.Errorf("-f - reads manifests from standard input, and standard input is a terminal; " +
				"pipe the manifests in, for example: kustomize build ./overlays/prod | agentmoat verify -f -")
		}
		opts.Stdin = in
		break
	}
	return nil
}

func runVerify(cmd *cobra.Command, _ []string) error {
	format, err := output.ParseFor(flagOutput, schema.KindVerifyReport)
	if err != nil {
		return err
	}

	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}

	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	opts := agentmoat.VerifyOptions{
		KubeconfigPath: flagKubeconfig,
		Context:        flagContext,
		InPodProbe:     flagVerifyInPodProbe,
		Wait:           flagVerifyWait,
		Stderr:         stderr,
	}
	if err := verifySource(cmd, &opts); err != nil {
		return err
	}

	// No signal handler here, on purpose. An interrupt (Ctrl-C, or the
	// SIGTERM of a job that the CI system cancels) must end the process by
	// the signal. A shell goes on with the right side of
	//
	//	agentmoat verify --wait 3m || agentmoat rollback --dry-run=false
	//
	// when the left side handled the signal and then returned an exit
	// code, and a wait that was cut short always has a step that is not
	// ok (exit 4). Ctrl-C on a healthy rollout would then start a live
	// rollback. A process that dies by the signal makes the shell stop
	// the full list. The library does return the last complete look when
	// its context is canceled (metadata.wait.interrupted); that is for
	// callers that own their context, such as the MCP server.
	res, err := agentmoat.Verify(context.Background(), opts)
	if err != nil {
		return err
	}

	if err := output.Render(res, format, cmd.OutOrStdout(), renderOpts); err != nil {
		return err
	}

	// The exit code is in the library (docs/exit-codes.md): 4 for a result
	// that is not ok, so CI scripts and AI agents can branch on it, and 1
	// for a report with no failure and a workload that could not be checked.
	exitCode = agentmoat.VerifyExitCode(res)
	return nil
}
