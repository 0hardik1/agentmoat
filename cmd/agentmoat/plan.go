// agentmoat plan: produce a MigrationPlan from a scan.
//
// The plan command is read-only: it never mutates the cluster. It produces a
// deterministic, ordered MigrationPlan that the operator can review,
// archive, then feed to `agentmoat apply` (possibly from a different host).
//
// Source resolution
//
//   - Default (no --scan): the planner runs `agentmoat scan` inline.
//   - --scan <file>: the planner reads a stored ScanReport (JSON or YAML).
//     This is useful when the cluster has thousands of workloads and
//     you've already saved a scan to disk; planning is cheap, scanning is
//     not.
//
// Exit codes (docs/exit-codes.md):
//   0  plan produced (possibly empty)
//   1  generic error (kubeconfig, malformed scan file, etc.)

package main

import (
	"context"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/agentmoat"
	"github.com/0hardik1/agentmoat/pkg/output"
)

// Plan-specific flag values. Kept package-level for symmetry with
// flagOutput, flagKubeconfig, etc. set in root.go.
var (
	flagPlanScanPath      string
	flagPlanIncludeReview bool
	flagPlanRuntimeClass  string
	flagPlanAddToleration bool
	flagPlanFactsPath     string
)

func newPlanCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Produce a MigrationPlan from a scan (read-only)",
		Long: `plan produces a deterministic MigrationPlan: an ordered list of
workloads to migrate to gVisor, lowest-risk first. The plan is itself a
versioned schema document (kind: MigrationPlan) you can archive, review,
and feed to 'agentmoat apply'.

The plan source is either an inline scan (default) or a stored ScanReport
on disk (--scan). Either way, the planner is a pure function: the same
input always produces the same plan and the same plan hash.

The plan has a step only for a workload that has no RuntimeClass now. A
workload that already has one is listed under "excluded" with the reason:
it has the class of the plan (nothing to change), or it has another class
(the plan does not replace it, because a rollback could not put it back).
So apply, 'verify --plan', and rollback with one plan cover the workloads
that this plan moves, and not the ones that an earlier plan moved.

Each step patches only runtimeClassName. Placement is the RuntimeClass's
job: its scheduling.nodeSelector and tolerations are merged into every pod
at admission. Pass --add-toleration only when your RuntimeClass lacks
scheduling.tolerations and your gVisor nodes carry the
runtime=gvisor:NoSchedule taint; it changes the plan hash.

When the scan recorded cluster facts, the plan lists warnings (no matching
Ready node, EKS Auto Mode nodes, unsupported GPU cards, ...) under
spec.warnings. They do not change the steps or the hash; 'agentmoat apply'
re-checks the live cluster. --facts loads the facts from a saved report
(the inline scan then classifies against them too; see 'scan --help').

plan is strictly read-only.`,
		RunE: runPlan,
	}
	cmd.Flags().StringVar(&flagPlanScanPath, "scan", "",
		"path to a stored ScanReport (JSON/YAML). Default: scan inline")
	cmd.Flags().BoolVar(&flagPlanIncludeReview, "include-review", false,
		"include workloads classified 'review' in the plan (default: only 'compatible')")
	cmd.Flags().StringVar(&flagPlanRuntimeClass, "runtime-class", "gvisor",
		"RuntimeClass name to patch onto migrated workloads")
	cmd.Flags().BoolVar(&flagPlanAddToleration, "add-toleration", false,
		"also patch the runtime=gvisor:NoSchedule toleration into each pod template (changes the plan hash)")
	cmd.Flags().StringVar(&flagPlanFactsPath, "facts", "",
		"load cluster facts from a saved PreflightReport or ScanReport (e.g. 'probe nvproxy' output); with --scan, replaces the stored facts for the warnings")
	return cmd
}

func runPlan(cmd *cobra.Command, _ []string) error {
	format, err := output.ParseFor(flagOutput, schema.KindMigrationPlan)
	if err != nil {
		return err
	}

	renderOpts := output.RenderOptions{
		NoColor: flagNoColor || os.Getenv("NO_COLOR") != "",
	}

	// --quiet -> orchestrator stderr is io.Discard. See the same idiom
	// in scan.go (kept duplicated here for clarity at the call site).
	stderr := cmd.ErrOrStderr()
	if flagQuiet {
		stderr = io.Discard
	}

	opts := agentmoat.PlanOptions{
		ScanOptions: agentmoat.ScanOptions{
			KubeconfigPath:   flagKubeconfig,
			Context:          flagContext,
			Namespaces:       flagNamespaces,
			AllNamespaces:    flagAllNamespaces || len(flagNamespaces) == 0,
			IncludeSystem:    flagIncludeSystem,
			LabelSelector:    flagLabelSelector,
			RulesYAMLPath:    flagRulesYAML,
			RuntimeClassName: flagPlanRuntimeClass,
			FactsPath:        flagPlanFactsPath,
			Stderr:           stderr,
		},
		Planner: schema.PlannerOptions{
			IncludeReview:    flagPlanIncludeReview,
			RuntimeClassName: flagPlanRuntimeClass,
			AddToleration:    flagPlanAddToleration,
		},
		Stderr: stderr,
	}

	// Source resolution: --scan names a saved ScanReport. The library
	// loads it (and rejects a file of the wrong kind), so this command and
	// the MCP propose_plan tool behave the same. Empty means scan inline.
	opts.ScanReportPath = flagPlanScanPath

	plan, err := agentmoat.Plan(context.Background(), opts)
	if err != nil {
		return err
	}

	return output.Render(plan, format, cmd.OutOrStdout(), renderOpts)
}
