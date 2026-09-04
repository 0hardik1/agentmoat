// Package classifier: rule registry and override plumbing.
//
// Design notes
//
//   - Rule pattern. A Rule is a tiny struct: stable ID, severity, human
//     description, link to upstream remediation docs, and a Match closure
//     that runs over a scanner.Workload. The closure has no I/O and no
//     hidden state: the same workload always produces the same result.
//
//   - Registry vs flat list. Built-in rules are registered into an in-memory
//     Registry rather than declared as a package-level slice. The Registry
//     gives us three things that a flat list does not: (1) a single point of
//     control for ID uniqueness (Register panics on duplicate IDs, catching
//     copy-paste bugs at startup); (2) deterministic ordering (Rules()
//     always returns sorted by ID so callers see the same output for the
//     same input); (3) an override hook (Override and LoadYAML mutate an
//     existing rule's severity in-place, which lets operators tune the
//     classifier without forking the binary).
//
//   - YAML override. internal/rules/gvisor.yaml mirrors the built-in rule
//     IDs and lets operators dial severity per environment. For example,
//     a team that already wraps hostPath mounts with their own admission
//     policy can demote `host-path-mount` from warn to info. The override
//     file is loaded once at CLI startup; unknown IDs are reported via the
//     returned warnings slice but do not fail the call (forward-compat: an
//     older agentmoat binary should not blow up on a YAML file that names
//     rules introduced later).
package classifier

import (
	"errors"
	"fmt"
	"sort"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	"sigs.k8s.io/yaml"
)

// Rule is one classifier check. Match runs over the workload's PodSpec
// (plus a few hints from the Workload envelope, like the Kind) and returns
// true if the rule fires for this workload.
type Rule struct {
	// ID is the stable identifier referenced by output JSON and by the
	// YAML override file. Lowercase, dash-separated (e.g. "raw-socket").
	ID string

	// Severity controls how the verdict aggregates: any error makes the
	// workload Incompatible, any warn makes it Review, info is purely
	// informational.
	Severity Severity

	// Description is a single sentence shown to operators. Keep it short
	// enough to fit one row of the CLI table view.
	Description string

	// RemediationURL deep-links to the upstream gVisor docs (or an
	// agentmoat-internal doc) explaining the workaround.
	RemediationURL string

	// Match is the rule predicate. It is called with the workload value
	// (not a pointer) so rules cannot accidentally mutate scanner state.
	Match func(w scanner.Workload) bool

	// Refine, when set, lets a rule that matched adjust its verdict from
	// the workload and the cluster facts (schema.ClusterFacts: nodes,
	// RuntimeClass, GPUs, probed nvproxy drivers). It receives the rule's
	// configured severity (after any YAML override) as base and returns
	// ok=false to leave the verdict alone. facts may be nil: a rule may
	// still refine from the workload alone (a MIG resource request, say).
	// Only gpu-passthrough sets this today. Refine must stay pure and
	// deterministic like Match.
	Refine func(w scanner.Workload, facts *schema.ClusterFacts, base Severity) (Refinement, bool)
}

// Refinement is what Rule.Refine returns: the severity that replaces the
// rule's configured one for this workload, and a note appended to the
// Reason description saying which facts decided it.
type Refinement struct {
	Severity Severity
	Note     string
}

// Registry is the in-memory set of rules used by Classify(). Built-in rules
// come from RegisterBuiltins(); external YAML overrides go through
// LoadYAML().
//
// Registry is not safe for concurrent mutation. We register all rules at
// CLI startup and then treat the Registry as read-only for the rest of the
// process lifetime.
type Registry struct {
	// rules is keyed by ID for O(1) Override lookups. Rules() returns
	// them in deterministic (sorted) order.
	rules map[string]Rule
}

// ErrUnknownRuleID is returned by Override when no rule with the given ID
// exists in the registry. Callers (LoadYAML) collect these into a warnings
// slice rather than treating them as fatal.
var ErrUnknownRuleID = errors.New("classifier: unknown rule id")

// NewRegistry returns an empty registry. Callers normally then invoke
// RegisterBuiltins(reg) to populate the standard rule set.
func NewRegistry() *Registry {
	return &Registry{
		rules: make(map[string]Rule),
	}
}

// Register adds a rule. Panics if a rule with the same ID already exists,
// since duplicate IDs are a programmer error (built-in rules must be
// uniquely named).
func (r *Registry) Register(rule Rule) {
	if _, exists := r.rules[rule.ID]; exists {
		panic(fmt.Sprintf("classifier: duplicate rule ID %q", rule.ID))
	}
	r.rules[rule.ID] = rule
}

// Override updates an existing rule's Severity. Returns ErrUnknownRuleID if
// no rule with that ID is registered. This is the only mutation operation
// expected at runtime (other rule fields are compiled into the binary).
func (r *Registry) Override(id string, severity Severity) error {
	rule, exists := r.rules[id]
	if !exists {
		return fmt.Errorf("%w: %q", ErrUnknownRuleID, id)
	}
	rule.Severity = severity
	r.rules[id] = rule
	return nil
}

// Rules returns the current rule list sorted by ID. Sorting here (rather
// than at every call site) guarantees deterministic Verdict output across
// runs and across machines.
func (r *Registry) Rules() []Rule {
	out := make([]Rule, 0, len(r.rules))
	for _, rule := range r.rules {
		out = append(out, rule)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// yamlOverride is the wire shape of one entry in the override YAML. We
// accept extra fields (description, remediationUrl, source) but only the
// id and severity are honored at runtime: the description and link text
// live in the Go source so they ship with the binary.
type yamlOverride struct {
	ID             string   `json:"id"`
	Severity       Severity `json:"severity"`
	Description    string   `json:"description,omitempty"`
	RemediationURL string   `json:"remediationUrl,omitempty"`
	Source         string   `json:"source,omitempty"`
}

// yamlFile is the top-level shape of internal/rules/gvisor.yaml.
type yamlFile struct {
	Overrides []yamlOverride `json:"overrides"`
}

// LoadYAML reads override entries from a YAML byte stream. Each override
// is `{id: <string>, severity: <info|warn|error>}`. Unknown IDs are
// reported via the returned warnings slice but do not fail the call. A
// parse error (invalid YAML, missing required fields) is returned as err.
//
// sigs.k8s.io/yaml is used because it converts YAML to JSON internally and
// then unmarshals via encoding/json, which lets us share json tags with
// the rest of the project's output rendering.
func (r *Registry) LoadYAML(data []byte) (warnings []string, err error) {
	var f yamlFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("classifier: parse rule overrides: %w", err)
	}

	for _, o := range f.Overrides {
		if o.ID == "" {
			warnings = append(warnings, "override entry missing id, skipped")
			continue
		}
		// Validate severity value: only the three canonical strings are
		// accepted. Reject silently-bogus YAML (severity: foo) early.
		switch o.Severity {
		case SeverityInfo, SeverityWarn, SeverityError:
		default:
			warnings = append(warnings, fmt.Sprintf("rule %q: invalid severity %q, skipped", o.ID, o.Severity))
			continue
		}
		if err := r.Override(o.ID, o.Severity); err != nil {
			// Unknown ID: forward-compatible warning, not fatal.
			warnings = append(warnings, fmt.Sprintf("rule %q: not registered, override skipped", o.ID))
		}
	}
	return warnings, nil
}
