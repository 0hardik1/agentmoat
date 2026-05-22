// Package agentmoat is the public Go entrypoint for the agentmoat toolkit.
//
// Both the CLI (`cmd/agentmoat`) and the future MCP server
// (`cmd/agentmoat-mcp`) call into this package. Keeping the orchestration
// here, not in cmd/, is what makes agentmoat embeddable: a third party can
// `import "github.com/0hardik1/agentmoat/pkg/agentmoat"` and call Scan()
// without spawning a child process.
//
// This file declares the options structs. Behavior lives in agentmoat.go.
package agentmoat

import (
	"io"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// ScanOptions configures a Scan() call. Zero value is meaningful: it
// performs an all-namespaces, default-format scan against the in-cluster
// config (if running inside the cluster) or the default kubeconfig.
type ScanOptions struct {
	// KubeconfigPath is the path to a kubeconfig file. Empty means use the
	// standard search path (KUBECONFIG env, then ~/.kube/config), then
	// fall back to in-cluster config.
	KubeconfigPath string

	// Context overrides the active context in the loaded kubeconfig.
	// Empty means use whatever context the kubeconfig points at.
	Context string

	// Namespaces restricts the scan to the named namespaces. Empty means
	// cluster-wide.
	Namespaces []string

	// AllNamespaces is the explicit form of "cluster-wide". When true,
	// Namespaces is ignored. Default true (no namespace filter).
	AllNamespaces bool

	// IncludeSystem includes kube-system and other kube-* namespaces.
	// Default false (most operators don't want to migrate the control plane).
	IncludeSystem bool

	// LabelSelector is a Kubernetes-style label selector applied to every
	// list call. Empty means no filter.
	LabelSelector string

	// RulesYAMLPath, when non-empty, points at an override file (typically
	// internal/rules/gvisor.yaml bundled with the binary or a user-supplied
	// path). The classifier loads this on top of the built-in rule set.
	RulesYAMLPath string

	// Stderr is the writer used for human-readable progress messages
	// (counts, timings, "scanning namespace X..."). Defaults to os.Stderr
	// in cmd/. Set to io.Discard to silence.
	Stderr io.Writer
}

// PlanOptions configures pkg/agentmoat.Plan. Plan() either reads a
// ScanReport directly (when ScanReport is non-nil) or runs Scan() inline
// with the embedded ScanOptions.
type PlanOptions struct {
	// ScanOptions is the scan configuration to use when ScanReport is
	// nil. Ignored when ScanReport is supplied.
	ScanOptions ScanOptions

	// ScanReport, when non-nil, is used as the source for the plan.
	// This is how the CLI feeds in a stored scan from disk.
	ScanReport *schema.ScanReport

	// Planner controls ordering, inclusion, and the RuntimeClass name.
	Planner schema.PlannerOptions

	// Stderr is the writer for progress messages.
	Stderr io.Writer
}

// ApplyOptions configures pkg/agentmoat.Apply.
type ApplyOptions struct {
	// KubeconfigPath and Context are the same as ScanOptions.
	KubeconfigPath string
	Context        string

	// PlanPath is the path to a MigrationPlan YAML/JSON file on disk.
	// Required when PlanInline is nil.
	PlanPath string

	// DryRun defaults to true per plan.md section 12.2. Callers must set
	// this to false explicitly to mutate the cluster.
	DryRun bool

	// EmitEvents, when true, emits one Kubernetes Event per mutation on
	// the affected namespace. Default true.
	EmitEvents bool

	// AuditEnabled, when true, appends to ~/.agentmoat/audit.jsonl per
	// mutation. Default true.
	AuditEnabled bool

	// Stderr is the writer for progress messages.
	Stderr io.Writer
}

// RollbackOptions is the rollback-side counterpart of ApplyOptions. Fields
// kept identical so callers can copy-paste between commands.
type RollbackOptions = ApplyOptions

// VerifyOptions configures pkg/agentmoat.Verify. Verify is read-only: it
// loads a previously-applied MigrationPlan from disk, contacts the cluster,
// and reports per-step whether the live state matches the plan.
type VerifyOptions struct {
	// KubeconfigPath and Context are the same as ScanOptions.
	KubeconfigPath string
	Context        string

	// PlanPath is the path to a MigrationPlan YAML/JSON file on disk.
	// Required: verify needs to know which workloads to inspect.
	PlanPath string

	// InPodProbe, when true, opts into the gVisor in-pod probe: for each
	// step the verifier picks a Running pod and execs a small script that
	// inspects /proc/cmdline, dmesg, and uname for gVisor markers. Cheap
	// to skip in CI; opt-in by default so the standard run is purely
	// API-side.
	InPodProbe bool

	// Stderr is the writer for progress messages. Defaults to os.Stderr
	// in cmd/. Set to io.Discard to silence.
	Stderr io.Writer
}

// ExplainOptions configures pkg/agentmoat.Explain. Explain is offline: it
// reads from an embedded copy of docs/*.md and never contacts the cluster.
type ExplainOptions struct {
	// Topic is the topic name. Empty means "list available topics".
	// Lookups are case-insensitive (handled in pkg/explainer).
	Topic string
}
