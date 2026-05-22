// Package schema defines the versioned, externally-visible data shapes that
// agentmoat emits via `--output json` or `--output yaml`. These same types
// are also reused by the (future) MCP server as its tool result schemas,
// per plan.md sections 12.3 and 12.7 ("MCP tool schemas from Go types").
//
// Versioning rules
//
//   - Every top-level resource has APIVersion ("agentmoat.io/v1alpha1") and
//     Kind ("ScanReport", "MigrationPlan", ...). This mirrors the Kubernetes
//     object envelope so operators reading the output find it familiar.
//
//   - Field-tag style: each exported field carries `json:"..."` and
//     `yaml:"..."` tags. The two share names so JSON and YAML output stay
//     identical and can be diffed byte-for-byte after canonicalisation.
//
//   - Breaking changes require bumping the APIVersion (v1alpha1 -> v1alpha2)
//     and a deprecation note in docs/exit-codes.md.
//
// For Phase 1 we ship only ScanReport. Subsequent phases will add
// MigrationPlan, ApplyResult, VerifyReport in this same package.
package schema

// APIVersion is the only currently supported API version. Bump this string
// (and add a migration note in docs/) before changing any field above.
const APIVersion = "agentmoat.io/v1alpha1"

// Kinds emitted by agentmoat. Keep these short and PascalCase so they read
// naturally in JSON output.
const (
	KindScanReport     = "ScanReport"
	KindMigrationPlan  = "MigrationPlan"
	KindApplyResult    = "ApplyResult"
	KindRollbackResult = "RollbackResult"
)

// DefaultRuntimeClassName is the RuntimeClass name that the applier writes
// into pod templates. It must match the `metadata.name` of the RuntimeClass
// object the operator installs on the cluster (`deploy/runtimeclass.yaml`).
// Public so the MCP server can include it in tool documentation.
const DefaultRuntimeClassName = "gvisor"

// DefaultGVisorToleration is the toleration injected onto every migrated
// pod template, matching the `runtime=gvisor:NoSchedule` taint that the
// Packer-built nodes carry. See plan.md section 9.4.
const (
	GVisorTaintKey    = "runtime"
	GVisorTaintValue  = "gvisor"
	GVisorTaintEffect = "NoSchedule"
)

// PlanHashAnnotation is the namespace annotation key the applier writes to
// record which MigrationPlan was last applied to that namespace. Used for
// idempotency: re-applying an already-applied plan exits 0 without mutating.
const PlanHashAnnotation = "agentmoat.io/plan-hash"

// Compatibility is the per-workload verdict produced by the classifier.
// The string values are stable: scripts and dashboards downstream may
// switch on them.
type Compatibility string

const (
	// CompatibilityCompatible: no incompatible features detected; gVisor
	// migration is safe.
	CompatibilityCompatible Compatibility = "compatible"

	// CompatibilityReview: the workload uses features that may degrade or
	// require tuning under gVisor (network throughput, syscall-heavy, etc.)
	// but will not fail outright.
	CompatibilityReview Compatibility = "review"

	// CompatibilityIncompatible: the workload uses a feature that gVisor
	// does not support (eBPF, GPU passthrough outside nvproxy, host
	// network, raw sockets without --net-raw, etc.).
	CompatibilityIncompatible Compatibility = "incompatible"
)

// Severity tags individual rule findings. "error" is fatal for gVisor
// migration; "warn" needs operator review; "info" is purely informational
// (for instance, "this is a network-bound workload, expect ~30% overhead").
type Severity string

const (
	SeverityInfo  Severity = "info"
	SeverityWarn  Severity = "warn"
	SeverityError Severity = "error"
)

// ScanReport is the top-level envelope of `agentmoat scan` output. It is
// always a valid JSON document and a valid YAML document.
type ScanReport struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion"`
	Kind       string         `json:"kind"       yaml:"kind"`
	Metadata   ReportMetadata `json:"metadata"   yaml:"metadata"`
	Spec       ReportSpec     `json:"spec"       yaml:"spec"`
}

// NewScanReport returns an empty ScanReport with APIVersion and Kind set,
// ready for the orchestrator to fill in.
func NewScanReport() *ScanReport {
	return &ScanReport{
		APIVersion: APIVersion,
		Kind:       KindScanReport,
	}
}

// ReportMetadata captures the "when, where, who" of a scan run.
type ReportMetadata struct {
	// GeneratedAt is the RFC3339 timestamp when the scan completed.
	GeneratedAt string `json:"generatedAt" yaml:"generatedAt"`

	// Cluster is the kubeconfig context name (best-effort) or the
	// in-cluster service-account namespace if running inside the cluster.
	Cluster string `json:"cluster,omitempty" yaml:"cluster,omitempty"`

	// Namespaces is the list of namespaces actually scanned (post-filter).
	// Empty list means cluster-wide.
	Namespaces []string `json:"namespaces,omitempty" yaml:"namespaces,omitempty"`

	// AgentmoatVersion records which binary produced this report. Useful
	// for replaying scans and debugging classifier-rule drift over time.
	AgentmoatVersion string `json:"agentmoatVersion" yaml:"agentmoatVersion"`
}

// ReportSpec is the payload of a ScanReport: a summary plus the per-workload
// details.
type ReportSpec struct {
	Summary   Summary          `json:"summary"   yaml:"summary"`
	Workloads []WorkloadResult `json:"workloads" yaml:"workloads"`
}

// Summary is the bucketed count by Compatibility. Useful for dashboards and
// for the CLI's table view header.
type Summary struct {
	Total        int `json:"total"        yaml:"total"`
	Compatible   int `json:"compatible"   yaml:"compatible"`
	NeedsReview  int `json:"needsReview"  yaml:"needsReview"`
	Incompatible int `json:"incompatible" yaml:"incompatible"`
}

// WorkloadResult is one row of a ScanReport. The classifier produces these
// (via pkg/classifier) and the orchestrator joins them with the workload
// identity gathered by the scanner (pkg/scanner).
type WorkloadResult struct {
	// Identity. "Kind" is one of Pod/Deployment/StatefulSet/DaemonSet/Job/
	// CronJob; agentmoat reports on the controller, not its child pods,
	// when a controller exists, to avoid double-counting.
	Kind      string `json:"kind"      yaml:"kind"`
	Namespace string `json:"namespace" yaml:"namespace"`
	Name      string `json:"name"      yaml:"name"`

	// Compatibility is the verdict and the primary field operators look at.
	Compatibility Compatibility `json:"compatibility" yaml:"compatibility"`

	// Reasons lists every rule that fired, in deterministic order. A
	// "compatible" workload has Reasons == nil (or empty).
	Reasons []Reason `json:"reasons,omitempty" yaml:"reasons,omitempty"`

	// Recommendation is a short, human-readable sentence suggesting what to
	// do next: "set runtimeClassName: gvisor", "review network throughput
	// before opting in", "do not migrate". Generated by the classifier.
	Recommendation string `json:"recommendation,omitempty" yaml:"recommendation,omitempty"`

	// Overhead is the qualitative cost estimate sourced from plan.md
	// section 5.5 ("CPU-bound: <5%", "Network throughput: 20-40%", etc.).
	// Populated by the classifier when the workload class is detectable.
	Overhead string `json:"overhead,omitempty" yaml:"overhead,omitempty"`
}

// Reason is a single classifier finding. The combination of (RuleID,
// Severity, Description) tells an operator exactly which gotcha tripped and
// where to read more.
type Reason struct {
	// RuleID is the stable identifier of the classifier rule that fired
	// (e.g. "raw-socket", "ebpf", "host-network"). It is referenced from
	// internal/rules/gvisor.yaml so operators can override severities.
	RuleID string `json:"ruleId" yaml:"ruleId"`

	// Severity influences exit code: any "error" reason in any workload
	// causes `agentmoat scan` to exit 2 (per docs/exit-codes.md).
	Severity Severity `json:"severity" yaml:"severity"`

	// Description is the human-readable explanation of the finding. Kept
	// concise (one sentence) so it fits in the table view.
	Description string `json:"description" yaml:"description"`

	// RemediationURL is a deep-link to upstream gVisor docs or to a
	// agentmoat doc that explains the workaround.
	RemediationURL string `json:"remediationUrl,omitempty" yaml:"remediationUrl,omitempty"`
}

// ---------------------------------------------------------------------------
// Phase 2 types: MigrationPlan, ApplyResult, RollbackResult.
//
// These mirror the ScanReport envelope (APIVersion + Kind + Metadata + Spec)
// so the renderer (pkg/output) can switch on Kind and downstream tools can
// parse all four with the same loader.
// ---------------------------------------------------------------------------

// MigrationPlan is the output of `agentmoat plan`. It contains an ordered
// list of PlanSteps (lowest-risk first), a set of workloads the planner
// chose to exclude (typically Incompatible verdicts), and a deterministic
// PlanHash that the applier writes onto the namespace to record what was
// applied.
type MigrationPlan struct {
	APIVersion string       `json:"apiVersion" yaml:"apiVersion"`
	Kind       string       `json:"kind"       yaml:"kind"`
	Metadata   PlanMetadata `json:"metadata"   yaml:"metadata"`
	Spec       PlanSpec     `json:"spec"       yaml:"spec"`
}

// NewMigrationPlan returns an empty MigrationPlan with the envelope filled.
func NewMigrationPlan() *MigrationPlan {
	return &MigrationPlan{
		APIVersion: APIVersion,
		Kind:       KindMigrationPlan,
	}
}

// PlanMetadata holds the "when/where/from-what" provenance of a plan. Keeping
// these fields parallel to ReportMetadata makes the two envelopes easy to
// scan side-by-side.
type PlanMetadata struct {
	// GeneratedAt is the RFC3339 timestamp when the plan was produced.
	GeneratedAt string `json:"generatedAt" yaml:"generatedAt"`

	// Cluster is the kubeconfig context name (best-effort) that the source
	// scan came from. Empty when the plan was generated from a stored scan
	// without an attached context.
	Cluster string `json:"cluster,omitempty" yaml:"cluster,omitempty"`

	// AgentmoatVersion is the binary version that produced this plan.
	AgentmoatVersion string `json:"agentmoatVersion" yaml:"agentmoatVersion"`

	// SourceScanGeneratedAt is the timestamp of the ScanReport this plan
	// was derived from. Useful when applying a stored plan, because it lets
	// the operator see how stale the underlying view of the cluster is.
	SourceScanGeneratedAt string `json:"sourceScanGeneratedAt,omitempty" yaml:"sourceScanGeneratedAt,omitempty"`

	// PlanHash is the deterministic content hash of Spec.Steps. The applier
	// writes this onto the affected namespaces as the
	// `agentmoat.io/plan-hash` annotation. Re-applying an already-applied
	// plan compares this against the annotation to short-circuit.
	PlanHash string `json:"planHash" yaml:"planHash"`
}

// PlanSpec is the payload of a MigrationPlan: the ordered batches of steps,
// the workloads excluded from migration, and a summary count.
type PlanSpec struct {
	// Summary is the bucketed count: total steps, included, excluded.
	Summary PlanSummary `json:"summary" yaml:"summary"`

	// Options records the PlannerOptions the plan was generated with so a
	// reader can reproduce the same plan from the same scan.
	Options PlannerOptions `json:"options" yaml:"options"`

	// Steps is the ordered list of migration steps. Phase 2 emits them as
	// a flat list; the applier divides them into batches at apply time.
	Steps []PlanStep `json:"steps" yaml:"steps"`

	// Excluded lists every workload the planner declined to include, with
	// the reason. Always populated (may be empty) so consumers do not need
	// to nil-check.
	Excluded []ExcludedWorkload `json:"excluded" yaml:"excluded"`
}

// PlanSummary is the bucketed count of plan contents.
type PlanSummary struct {
	Total    int `json:"total"    yaml:"total"`
	Included int `json:"included" yaml:"included"`
	Excluded int `json:"excluded" yaml:"excluded"`
}

// PlannerOptions records the inputs that shaped the plan. Carried on the
// plan envelope so re-running planner against the same scan with the same
// options yields the same plan (determinism, plan.md section 12.6).
type PlannerOptions struct {
	// BatchSize is the number of workloads the applier will mutate before
	// pausing to await readiness. Zero means "all in one batch".
	BatchSize int `json:"batchSize,omitempty" yaml:"batchSize,omitempty"`

	// MaxParallel is the maximum number of patches in flight at one time
	// within a batch. Zero means "serial".
	MaxParallel int `json:"maxParallel,omitempty" yaml:"maxParallel,omitempty"`

	// IncludeReview, when true, allows Review-class workloads into the
	// plan. Default false: only Compatible workloads ride the happy path.
	IncludeReview bool `json:"includeReview,omitempty" yaml:"includeReview,omitempty"`

	// RuntimeClassName overrides the default "gvisor" RuntimeClass name.
	// Empty means use schema.DefaultRuntimeClassName.
	RuntimeClassName string `json:"runtimeClassName,omitempty" yaml:"runtimeClassName,omitempty"`
}

// PlanStep is one migration action. The applier consumes these in order.
type PlanStep struct {
	// Order is the 1-based position in the plan. Carried so excerpting one
	// step into a comment, log, or audit row still reads naturally.
	Order int `json:"order" yaml:"order"`

	// Target identifies the workload to patch.
	Target WorkloadRef `json:"target" yaml:"target"`

	// Action is the kind of mutation the applier will perform. Only
	// "set-runtime-class" is defined in v1alpha1; future revs may add
	// "add-toleration-only", "label-node", etc.
	Action string `json:"action" yaml:"action"`

	// RuntimeClassName is the RuntimeClass to switch the workload to.
	// Always populated for action="set-runtime-class".
	RuntimeClassName string `json:"runtimeClassName,omitempty" yaml:"runtimeClassName,omitempty"`

	// AddToleration, when true, also adds the runtime=gvisor:NoSchedule
	// toleration so the pod can land on a tainted gVisor node. Plan.md
	// section 9.4 requires this be true by default.
	AddToleration bool `json:"addToleration" yaml:"addToleration"`

	// WaitFor names the pod condition the applier should wait on after
	// patching. One of:
	//   "Ready"   the controller's pods are Ready (rolling deploys, etc.)
	//   "Running" the pod is Running but not necessarily Ready (Jobs, etc.)
	//   ""        do not wait (default for one-shot Pods and CronJobs).
	WaitFor string `json:"waitFor,omitempty" yaml:"waitFor,omitempty"`

	// RiskScore is the integer score the planner used to order this step,
	// kept here for transparency. Lower = earlier (less risky first).
	RiskScore int `json:"riskScore" yaml:"riskScore"`

	// Notes is an optional, free-form, single-sentence rationale for the
	// step's position (e.g. "Deployment fronted by load balancer, can roll
	// safely; included in batch 1"). Populated when meaningful.
	Notes string `json:"notes,omitempty" yaml:"notes,omitempty"`
}

// WorkloadRef is the minimal identity of a Kubernetes workload, used in
// PlanStep, ExcludedWorkload, and the Apply/Rollback result types.
type WorkloadRef struct {
	Kind      string `json:"kind"      yaml:"kind"`
	Namespace string `json:"namespace" yaml:"namespace"`
	Name      string `json:"name"      yaml:"name"`
}

// ExcludedWorkload records a workload the planner chose to skip and why.
type ExcludedWorkload struct {
	Target WorkloadRef `json:"target" yaml:"target"`

	// Compatibility is the verdict from the source scan that drove the
	// exclusion ("incompatible" or "review" when IncludeReview=false).
	Compatibility Compatibility `json:"compatibility" yaml:"compatibility"`

	// Reason is a one-sentence rationale ("workload is incompatible:
	// host-network, raw-socket") suitable for the table view.
	Reason string `json:"reason" yaml:"reason"`
}

// ---------------------------------------------------------------------------
// ApplyResult: output of `agentmoat apply`.
// ---------------------------------------------------------------------------

// ApplyResult is the wire-shape of `agentmoat apply`. It records the source
// plan (by hash) and a per-step outcome.
type ApplyResult struct {
	APIVersion string        `json:"apiVersion" yaml:"apiVersion"`
	Kind       string        `json:"kind"       yaml:"kind"`
	Metadata   ApplyMetadata `json:"metadata"   yaml:"metadata"`
	Spec       ApplySpec     `json:"spec"       yaml:"spec"`
}

// NewApplyResult returns an empty ApplyResult with the envelope filled.
func NewApplyResult() *ApplyResult {
	return &ApplyResult{
		APIVersion: APIVersion,
		Kind:       KindApplyResult,
	}
}

// ApplyMetadata records when, how, and against which plan the apply ran.
type ApplyMetadata struct {
	GeneratedAt      string `json:"generatedAt" yaml:"generatedAt"`
	Cluster          string `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	AgentmoatVersion string `json:"agentmoatVersion" yaml:"agentmoatVersion"`

	// PlanHash is the hash of the MigrationPlan this apply ran against.
	// The same value is written to each affected namespace as the
	// agentmoat.io/plan-hash annotation.
	PlanHash string `json:"planHash" yaml:"planHash"`

	// DryRun records whether the apply was a dry-run (no mutations) or a
	// real apply. Honored to match plan.md section 12.2's default.
	DryRun bool `json:"dryRun" yaml:"dryRun"`
}

// ApplySpec is the payload of an ApplyResult.
type ApplySpec struct {
	Summary ApplySummary `json:"summary" yaml:"summary"`

	// Steps is the per-step outcome, in the same order as the source
	// MigrationPlan.Spec.Steps.
	Steps []StepResult `json:"steps" yaml:"steps"`
}

// ApplySummary is the bucketed count of step outcomes.
type ApplySummary struct {
	Total          int `json:"total"          yaml:"total"`
	Applied        int `json:"applied"        yaml:"applied"`
	AlreadyApplied int `json:"alreadyApplied" yaml:"alreadyApplied"`
	Skipped        int `json:"skipped"        yaml:"skipped"`
	Failed         int `json:"failed"         yaml:"failed"`
}

// StepStatus is the outcome of one PlanStep at apply or rollback time.
type StepStatus string

const (
	// StepStatusApplied: the patch was sent to the API server and accepted
	// (or, in dry-run mode, would have been).
	StepStatusApplied StepStatus = "applied"

	// StepStatusAlreadyApplied: the namespace annotation says this plan
	// has already been applied; no mutation was needed.
	StepStatusAlreadyApplied StepStatus = "already-applied"

	// StepStatusSkipped: the operator declined this step (e.g. via a
	// future --workload selector), or it was excluded post-plan.
	StepStatusSkipped StepStatus = "skipped"

	// StepStatusFailed: the API server rejected the patch, or a precheck
	// errored. The StepResult.Error string explains why.
	StepStatusFailed StepStatus = "failed"
)

// StepResult is the per-step outcome. The combination (Target, Status,
// Error) tells an operator exactly which workload moved, which got stuck,
// and why.
type StepResult struct {
	Order  int         `json:"order"  yaml:"order"`
	Target WorkloadRef `json:"target" yaml:"target"`
	Status StepStatus  `json:"status" yaml:"status"`

	// Patch is the strategic-merge-patch JSON the applier sent (or would
	// send, in dry-run). Surfaced so operators can diff what is about to
	// change without running kubectl diff themselves.
	Patch string `json:"patch,omitempty" yaml:"patch,omitempty"`

	// Error is populated when Status == "failed". Empty otherwise. We use
	// a string (not a Go error) so the envelope survives JSON round-trips.
	Error string `json:"error,omitempty" yaml:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// RollbackResult: output of `agentmoat rollback`.
// ---------------------------------------------------------------------------

// RollbackResult mirrors ApplyResult: same envelope, same per-step structure,
// different Kind. Reusing StepResult keeps consumers from having to teach
// the renderer two near-identical shapes.
type RollbackResult struct {
	APIVersion string           `json:"apiVersion" yaml:"apiVersion"`
	Kind       string           `json:"kind"       yaml:"kind"`
	Metadata   ApplyMetadata    `json:"metadata"   yaml:"metadata"`
	Spec       RollbackSpec     `json:"spec"       yaml:"spec"`
}

// NewRollbackResult returns an empty RollbackResult with the envelope filled.
func NewRollbackResult() *RollbackResult {
	return &RollbackResult{
		APIVersion: APIVersion,
		Kind:       KindRollbackResult,
	}
}

// RollbackSpec mirrors ApplySpec.
type RollbackSpec struct {
	Summary ApplySummary `json:"summary" yaml:"summary"`
	Steps   []StepResult `json:"steps"   yaml:"steps"`
}
