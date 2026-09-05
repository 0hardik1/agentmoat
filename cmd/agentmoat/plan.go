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
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

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

Each step patches only runtimeClassName. Placement is the RuntimeClass's
job: its scheduling.nodeSelector and tolerations are merged into every pod
at admission. Pass --add-toleration only when your RuntimeClass lacks
scheduling.tolerations and your gVisor nodes carry the
runtime=gvisor:NoSchedule taint; it changes the plan hash.

When the scan recorded cluster facts, the plan lists warnings (no matching
Ready node, EKS Auto Mode nodes, ...) under spec.warnings. They do not
change the steps or the hash; 'agentmoat apply' re-checks the live cluster.

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
	return cmd
}

func runPlan(cmd *cobra.Command, _ []string) error {
	format, err := output.Parse(flagOutput)
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
			Stderr:           stderr,
		},
		Planner: schema.PlannerOptions{
			IncludeReview:    flagPlanIncludeReview,
			RuntimeClassName: flagPlanRuntimeClass,
			AddToleration:    flagPlanAddToleration,
		},
		Stderr: stderr,
	}

	// Source resolution: if --scan was given, load the report from disk
	// and skip the inline scan.
	if flagPlanScanPath != "" {
		report, err := loadScanReport(flagPlanScanPath)
		if err != nil {
			return fmt.Errorf("loading --scan: %w", err)
		}
		opts.ScanReport = report
	}

	plan, err := agentmoat.Plan(context.Background(), opts)
	if err != nil {
		return err
	}

	return output.Render(plan, format, cmd.OutOrStdout(), renderOpts)
}

// loadScanReport reads and parses a ScanReport from disk. The file may be
// JSON or YAML; sigs.k8s.io/yaml normalises both internally.
func loadScanReport(path string) (*schema.ScanReport, error) {
	// #nosec G304 -- path is a user-supplied CLI flag for a scan file the
	// operator explicitly points us at; that is the entire purpose of
	// `agentmoat plan --scan`.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if envelope.Kind != schema.KindScanReport {
		return nil, fmt.Errorf("file %s has kind %q, want %q",
			path, envelope.Kind, schema.KindScanReport)
	}
	var r schema.ScanReport
	if err := yaml.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &r, nil
}
