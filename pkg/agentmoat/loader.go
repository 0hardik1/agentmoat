// Package agentmoat: shared loaders for MigrationPlan files on disk.
//
// loadPlan is invoked by Apply, Rollback, and Verify. Keeping it here
// (instead of in apply.go) avoids a Verify -> Apply file dependency: Verify
// is read-only and should not be coupled to the mutating package.
//
// The on-disk format is whatever sigs.k8s.io/yaml accepts (YAML or JSON,
// since YAML is a JSON superset). The Kind sniff is the first defence
// against the operator pointing at a ScanReport by mistake.
package agentmoat

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/0hardik1/agentmoat/internal/schema"
	"sigs.k8s.io/yaml"
)

// loadPlan reads a MigrationPlan from disk. The file may be either YAML or
// JSON; sigs.k8s.io/yaml round-trips through JSON internally so the same
// parser handles both.
//
// Returns a non-nil error for: missing path, file read failure, malformed
// document, wrong Kind (e.g. someone hands us a ScanReport).
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
	return &plan, nil
}
