// Package agentmoat: shared loaders for report files on disk.
//
// loadPlan is invoked by Apply, Rollback, and Verify. Keeping it here
// (instead of in apply.go) avoids a Verify -> Apply file dependency: Verify
// is read-only and should not be coupled to the mutating package.
//
// loadScanReport is invoked by Plan (PlanOptions.ScanReportPath). The CLI
// and the MCP server each used to carry their own copy with different
// error text; the library owns it now, so `plan --scan` and the
// propose_plan tool reject the same files with the same message.
//
// The on-disk format is whatever sigs.k8s.io/yaml accepts (YAML or JSON,
// since YAML is a JSON superset). The Kind sniff is the first defense
// against the operator pointing at the wrong kind of report by mistake.
package agentmoat

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/planner"
	"sigs.k8s.io/yaml"
)

// loadPlan reads a MigrationPlan from disk. The file may be either YAML or
// JSON; sigs.k8s.io/yaml round-trips through JSON internally so the same
// parser handles both.
//
// Returns a non-nil error for: missing path, file read failure, malformed
// document, wrong Kind (e.g. someone hands us a ScanReport), a failed
// integrity check, and a plan that was made from manifests (a preview).
func loadPlan(path string) (*schema.MigrationPlan, error) {
	if path == "" {
		return nil, fmt.Errorf("--plan is required")
	}
	// #nosec G304 -- path is a user-supplied CLI flag for a plan file the
	// operator explicitly points us at; that is the entire purpose of
	// `agentmoat apply --plan` and `agentmoat verify --plan`.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	// Quick header sniff so a stale ScanReport produces a friendly error
	// rather than a partially-populated MigrationPlan.
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if envelope.Kind != schema.KindMigrationPlan {
		return nil, fmt.Errorf("plan file %s has kind %q, want %q",
			path, envelope.Kind, schema.KindMigrationPlan)
	}

	var plan schema.MigrationPlan
	if err := yaml.Unmarshal(data, &plan); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	// json.Marshal acts as a final shape check; an explicit re-marshal makes
	// the test "the file is parseable as a real MigrationPlan" cheap.
	if _, err := json.Marshal(plan); err != nil {
		return nil, fmt.Errorf("validating plan shape: %w", err)
	}

	// A plan made from manifests is a preview and nothing that acts on a
	// cluster may load it. The reason is in the applier: a dry-run marks a
	// step "applied" without reading the workload (it only builds the
	// patch), so `apply` on this plan would print a clean dry-run for
	// workloads that may not exist in the cluster at all, in namespaces the
	// manifests may not even name. That output would look like a rehearsal
	// and be nothing of the kind. Every caller of loadPlan (apply,
	// rollback, verify) acts on a cluster, so the check lives here.
	if src := plan.Metadata.Source; src != nil && src.Type == schema.SourceManifests {
		return nil, fmt.Errorf("plan file %s cannot be used here: %s", path, manifestPlanMessage)
	}

	// Integrity check: when the file claims a planHash, recompute it from
	// the steps + options and reject a mismatch. A hand-edited (or
	// corrupted) plan whose hash no longer matches its content would
	// otherwise poison the namespace-annotation idempotency check: apply
	// would stamp (or trust) a hash that does not describe the steps it
	// ran. A plan with no planHash is allowed through: the applier never
	// short-circuits on an empty hash, so there is nothing to poison.
	if plan.Metadata.PlanHash != "" {
		recomputed, err := planner.ComputePlanHash(plan.Spec.Steps, plan.Spec.Options)
		if err != nil {
			return nil, fmt.Errorf("recomputing plan hash for %s: %w", path, err)
		}
		if recomputed != plan.Metadata.PlanHash {
			return nil, fmt.Errorf(
				"plan file %s failed its integrity check: metadata.planHash is %q but the steps/options hash to %q; "+
					"the file was edited after planning (re-run 'agentmoat plan' to regenerate it)",
				path, plan.Metadata.PlanHash, recomputed)
		}
	}
	return &plan, nil
}

// loadScanReport reads a ScanReport from disk (YAML or JSON).
//
// Returns a non-nil error for: file read failure, malformed document, wrong
// Kind (e.g. someone hands us a MigrationPlan). The Kind is sniffed before
// the full parse so a wrong file produces "has kind X, want ScanReport"
// rather than a ScanReport with every field empty, which would plan zero
// steps and look like a clean result.
func loadScanReport(path string) (*schema.ScanReport, error) {
	// #nosec G304 -- path is a user-supplied flag (or MCP tool argument)
	// for a scan file the operator explicitly points us at; that is the
	// entire purpose of `agentmoat plan --scan`.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := yaml.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if envelope.Kind != schema.KindScanReport {
		return nil, fmt.Errorf("scan file %s has kind %q, want %q",
			path, envelope.Kind, schema.KindScanReport)
	}
	var report schema.ScanReport
	if err := yaml.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &report, nil
}
