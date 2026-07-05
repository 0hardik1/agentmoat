// Package schema defines the versioned, externally-visible data shapes that
// agentmoat emits via `--output json` or `--output yaml`. These same types
// are also reused by the MCP server as its tool result schemas, so the CLI
// and MCP surfaces emit byte-identical shapes.
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
	KindScanReport      = "ScanReport"
	KindMigrationPlan   = "MigrationPlan"
	KindApplyResult     = "ApplyResult"
	KindRollbackResult  = "RollbackResult"
	KindVerifyReport    = "VerifyReport"
	KindExplainDocument = "ExplainDocument"
)

// DefaultRuntimeClassName is the RuntimeClass name that the applier writes
// into pod templates. It must match the `metadata.name` of the RuntimeClass
// object the operator installs on the cluster (`deploy/runtimeclass.yaml`).
// Public so the MCP server can include it in tool documentation.
const DefaultRuntimeClassName = "gvisor"

// DefaultGVisorToleration is the toleration injected onto every migrated
// pod template, matching the `runtime=gvisor:NoSchedule` taint that the
// Packer-built nodes carry.
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

	// Overhead is the qualitative cost estimate ("CPU-bound: <5%",
	// "Network throughput: 20-40%", etc.). Populated by the classifier
	// when the workload class is detectable.
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
// options yields the same plan (determinism).
type PlannerOptions struct {
	// BatchSize is RESERVED for a future batched applier. The v1 applier
	// walks steps serially and ignores this field; it is carried on the
	// envelope (and folded into the planHash) only so the schema does not
	// need a version bump when batching lands. Zero means "unset".
	BatchSize int `json:"batchSize,omitempty" yaml:"batchSize,omitempty"`

	// MaxParallel is RESERVED, like BatchSize. The v1 applier is strictly
	// serial (see pkg/applier's package doc for why); this field is
	// echoed back but never honored. Zero means "unset".
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

	// WaitFor is an ADVISORY hint naming the pod condition that marks
	// this step as converged. The v1 applier does not wait on rollouts
	// (it patches and moves on); the hint tells the operator (or a
	// future waiting applier) what to watch. One of:
	//   "Ready"   the controller's pods are Ready (rolling deploys, etc.)
	//   "Running" the pod is Running but not necessarily Ready (Jobs, etc.)
	//   ""        nothing to wait on.
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
	// real apply. Dry-run is the default; callers opt in to mutate.
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

	// StepStatusSkipped: the step was deliberately not executed. Today
	// this happens when a rollback finds the namespace stamped with a
	// different plan's hash (a newer plan governs it); the
	// StepResult.Error string explains why and what to do instead.
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

	// Error is populated when Status == "failed" (why the patch was
	// rejected) or "skipped" (why the step was not attempted). Empty
	// otherwise. We use a string (not a Go error) so the envelope
	// survives JSON round-trips.
	Error string `json:"error,omitempty" yaml:"error,omitempty"`
}

// ---------------------------------------------------------------------------
// RollbackResult: output of `agentmoat rollback`.
// ---------------------------------------------------------------------------

// RollbackResult mirrors ApplyResult: same envelope, same per-step structure,
// different Kind. Reusing StepResult keeps consumers from having to teach
// the renderer two near-identical shapes.
type RollbackResult struct {
	APIVersion string        `json:"apiVersion" yaml:"apiVersion"`
	Kind       string        `json:"kind"       yaml:"kind"`
	Metadata   ApplyMetadata `json:"metadata"   yaml:"metadata"`
	Spec       RollbackSpec  `json:"spec"       yaml:"spec"`
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

// ---------------------------------------------------------------------------
// Phase 3 types: VerifyReport, ExplainDocument.
//
// VerifyReport is the output of `agentmoat verify`: one VerifyResult per
// PlanStep, plus a summary. The renderer dispatches on Kind exactly like the
// Phase 2 types.
//
// ExplainDocument is the output of `agentmoat explain`. For `--output table`
// (the default) the renderer prints Spec.Content verbatim so operators see
// the raw markdown; JSON and YAML keep the full envelope.
// ---------------------------------------------------------------------------

// VerifyStatus is the per-step verdict from the verifier. Downstream tools
// may switch on these string values; treat them as stable.
type VerifyStatus string

const (
	// VerifyStatusOK: the live cluster state matches what the plan asked
	// for. For controller steps every pod selected by the controller
	// reports the expected runtimeClassName; for Pod steps the pod itself
	// does. When --in-pod-probe is on, the probe also returned gVisor
	// markers.
	VerifyStatusOK VerifyStatus = "ok"

	// VerifyStatusMismatch: the target exists but its (or its pods')
	// runtimeClassName does not match Expected. Common after rollback or
	// when the operator points verify at the wrong plan.
	VerifyStatusMismatch VerifyStatus = "mismatch"

	// VerifyStatusError: the verifier could not reach a verdict. The
	// controller was missing, the selector returned zero pods, the API
	// server errored, or (when --in-pod-probe is on) the exec call
	// itself failed. The Message field explains which.
	VerifyStatusError VerifyStatus = "error"
)

// VerifyReport is the top-level envelope of `agentmoat verify` output.
type VerifyReport struct {
	APIVersion string         `json:"apiVersion" yaml:"apiVersion"`
	Kind       string         `json:"kind"       yaml:"kind"`
	Metadata   VerifyMetadata `json:"metadata"   yaml:"metadata"`
	Spec       VerifySpec     `json:"spec"       yaml:"spec"`
}

// NewVerifyReport returns an empty VerifyReport with APIVersion and Kind
// pre-filled, ready for the orchestrator to populate.
func NewVerifyReport() *VerifyReport {
	return &VerifyReport{
		APIVersion: APIVersion,
		Kind:       KindVerifyReport,
	}
}

// VerifyMetadata records when, where, and against what plan the verify ran.
// PlanHash is carried so the operator can correlate a verify report with the
// MigrationPlan that produced the apply.
type VerifyMetadata struct {
	GeneratedAt      string `json:"generatedAt" yaml:"generatedAt"`
	Cluster          string `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	AgentmoatVersion string `json:"agentmoatVersion" yaml:"agentmoatVersion"`

	// PlanHash is the hash of the MigrationPlan this verify ran against.
	// Same value the applier writes to the namespace annotation.
	PlanHash string `json:"planHash,omitempty" yaml:"planHash,omitempty"`

	// InPodProbe records whether the verifier ran the gVisor in-pod probe
	// (kubectl exec into a pod and grep /proc/cmdline et al). When false,
	// the verifier only checked the controller's runtimeClassName field.
	InPodProbe bool `json:"inPodProbe" yaml:"inPodProbe"`
}

// VerifySpec is the payload of a VerifyReport: a summary plus the per-step
// results in plan order.
type VerifySpec struct {
	Summary VerifySummary  `json:"summary" yaml:"summary"`
	Results []VerifyResult `json:"results" yaml:"results"`
}

// VerifySummary is the bucketed count of per-step VerifyStatus values. The
// CLI's exit code is shaped from this: any non-ok count triggers exit 4
// (see docs/exit-codes.md).
type VerifySummary struct {
	Total    int `json:"total"    yaml:"total"`
	OK       int `json:"ok"       yaml:"ok"`
	Mismatch int `json:"mismatch" yaml:"mismatch"`
	Error    int `json:"error"    yaml:"error"`
}

// VerifyResult is one row of a VerifyReport: what we expected, what we found,
// and (optionally) the result of the in-pod probe.
type VerifyResult struct {
	// Order is the 1-based position in the source plan. Useful when the
	// operator is staring at a screen of results and wants to cross-
	// reference with `agentmoat plan --output table`.
	Order int `json:"order" yaml:"order"`

	// Target identifies the workload this result is about.
	Target WorkloadRef `json:"target" yaml:"target"`

	// Status is the verdict (ok | mismatch | error).
	Status VerifyStatus `json:"status" yaml:"status"`

	// Expected is the runtimeClassName the plan asked for ("gvisor" by
	// default).
	Expected string `json:"expected" yaml:"expected"`

	// Actual is the runtimeClassName the verifier found on the live
	// workload. Empty when the workload was missing or no pods matched
	// the selector; in those cases Status is "error" and Message explains.
	Actual string `json:"actual,omitempty" yaml:"actual,omitempty"`

	// Message is a one-sentence human-readable explanation. Always
	// populated for non-ok results; may also be set for ok results to
	// note something useful (e.g. "probe confirmed gVisor markers").
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// Probe holds the in-pod probe result when --in-pod-probe is on.
	// Nil when the probe was skipped.
	Probe *ProbeResult `json:"probe,omitempty" yaml:"probe,omitempty"`
}

// ProbeResult is the output of one in-pod gVisor probe. The verifier picks a
// representative Running pod for the step's target and execs a small script
// that prints distinctive gVisor markers (dmesg, /proc/cmdline, uname).
type ProbeResult struct {
	// Pod is the name of the pod the verifier execed into.
	Pod string `json:"pod" yaml:"pod"`

	// Detected is true when the probe stdout contained at least one
	// gVisor marker (case-insensitive). False means the probe ran but did
	// not see a marker; that demotes the result to mismatch.
	Detected bool `json:"detected" yaml:"detected"`

	// Markers is a short, comma-separated list of the marker strings the
	// probe found (e.g. "gvisor"). Empty when Detected is false.
	Markers string `json:"markers,omitempty" yaml:"markers,omitempty"`

	// Error captures the exec error (or stderr summary) when the probe
	// failed outright. Populated only when the probe itself errored.
	Error string `json:"error,omitempty" yaml:"error,omitempty"`
}

// ExplainDocument is the output of `agentmoat explain`. For `--output table`
// the renderer ignores the envelope and prints Spec.Content (or the topic
// list, when Spec.Topic is empty). JSON and YAML output keeps the envelope so
// the MCP server and CI scripts can consume explain output structurally.
type ExplainDocument struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind"       yaml:"kind"`
	Metadata   ExplainMetadata `json:"metadata"   yaml:"metadata"`
	Spec       ExplainSpec     `json:"spec"       yaml:"spec"`
}

// NewExplainDocument returns an empty ExplainDocument with the envelope filled.
func NewExplainDocument() *ExplainDocument {
	return &ExplainDocument{
		APIVersion: APIVersion,
		Kind:       KindExplainDocument,
	}
}

// ExplainMetadata captures the timestamp and binary version of an explain
// invocation. There is no cluster field because explain is offline.
type ExplainMetadata struct {
	GeneratedAt      string `json:"generatedAt" yaml:"generatedAt"`
	AgentmoatVersion string `json:"agentmoatVersion" yaml:"agentmoatVersion"`
}

// ExplainSpec carries either the requested topic (when the user asked for
// one) or the available topic list (when they did not).
type ExplainSpec struct {
	// Topic is the topic name the user asked for, lowercased. Empty when
	// the user invoked `agentmoat explain` with no positional argument.
	Topic string `json:"topic,omitempty" yaml:"topic,omitempty"`

	// Content is the raw markdown content of the requested topic, sourced
	// from docs/<topic>.md via embed.FS. Empty when Topic is empty.
	Content string `json:"content,omitempty" yaml:"content,omitempty"`

	// Topics is the list of available topic names, sorted, always
	// populated. Lets `explain --output json` enumerate without two
	// invocations.
	Topics []string `json:"topics,omitempty" yaml:"topics,omitempty"`

	// Namespace is populated when the document was produced by
	// `explain namespace <ns>` or `explain workload <ns>/<name>`.
	// Nil for static-topic mode.
	Namespace *NamespaceExplanation `json:"namespace,omitempty" yaml:"namespace,omitempty"`
}

// NamespaceExplanation is the deep per-namespace explanation produced by
// `agentmoat explain namespace <ns>` or `agentmoat explain workload <ns>/<name>`.
// It is attached to ExplainSpec.Namespace when present; nil for static-topic mode.
type NamespaceExplanation struct {
	Name      string                `json:"name"      yaml:"name"`
	Summary   Summary               `json:"summary"   yaml:"summary"`
	Workloads []WorkloadExplanation `json:"workloads" yaml:"workloads"`
}

// WorkloadExplanation is the deep explanation for one workload: the
// verdict, the recommendation, the list of rules that fired (with
// structured evidence and prose), and (for compatible workloads) the
// list of rules that did not fire.
type WorkloadExplanation struct {
	Kind           string        `json:"kind"           yaml:"kind"`
	Namespace      string        `json:"namespace"      yaml:"namespace"`
	Name           string        `json:"name"           yaml:"name"`
	Compatibility  Compatibility `json:"compatibility"  yaml:"compatibility"`
	Recommendation string        `json:"recommendation,omitempty" yaml:"recommendation,omitempty"`
	Overhead       string        `json:"overhead,omitempty"       yaml:"overhead,omitempty"`
	Findings       []RuleFinding `json:"findings,omitempty" yaml:"findings,omitempty"`
	Checked        []RuleCheck   `json:"checked,omitempty"  yaml:"checked,omitempty"`
}

// RuleFinding is one rule that fired against a workload, with
// structured evidence and deep human prose.
type RuleFinding struct {
	RuleID         string   `json:"ruleId"               yaml:"ruleId"`
	Severity       Severity `json:"severity"             yaml:"severity"`
	Title          string   `json:"title"                yaml:"title"`
	WhyMarkdown    string   `json:"whyMarkdown,omitempty" yaml:"whyMarkdown,omitempty"`
	Evidence       Evidence `json:"evidence,omitempty"   yaml:"evidence,omitempty"`
	RemediationURL string   `json:"remediationUrl,omitempty" yaml:"remediationUrl,omitempty"`
}

// RuleCheck records a rule that was evaluated but did not fire. Used to
// show the operator what was checked in a compatible verdict.
type RuleCheck struct {
	RuleID  string `json:"ruleId"  yaml:"ruleId"`
	Outcome string `json:"outcome" yaml:"outcome"` // currently always "did-not-fire"
}

// Evidence is a discriminated record of the concrete facts that
// triggered (or could trigger) a rule. Only the fields relevant to the
// firing rule are populated; the rest stay zero so JSON output stays
// terse.
type Evidence struct {
	HostNamespaces       []string        `json:"hostNamespaces,omitempty"       yaml:"hostNamespaces,omitempty"`
	Capabilities         []CapabilityHit `json:"capabilities,omitempty"         yaml:"capabilities,omitempty"`
	HostPaths            []HostPathHit   `json:"hostPaths,omitempty"            yaml:"hostPaths,omitempty"`
	ImageMatches         []ImageMatch    `json:"imageMatches,omitempty"         yaml:"imageMatches,omitempty"`
	GPURequests          []GPURequest    `json:"gpuRequests,omitempty"          yaml:"gpuRequests,omitempty"`
	EnvVars              []EnvVarHit     `json:"envVars,omitempty"              yaml:"envVars,omitempty"`
	Annotations          []AnnotationHit `json:"annotations,omitempty"          yaml:"annotations,omitempty"`
	PrivilegedContainers []string        `json:"privilegedContainers,omitempty" yaml:"privilegedContainers,omitempty"`
	CSIDrivers           []CSIDriverHit  `json:"csiDrivers,omitempty"           yaml:"csiDrivers,omitempty"`
}

// CapabilityHit names a container/capability pair that triggered a rule.
type CapabilityHit struct {
	Container  string `json:"container"  yaml:"container"`
	Capability string `json:"capability" yaml:"capability"`
}

// HostPathHit names a hostPath volume, its host filesystem path, and the
// containers that mount it.
type HostPathHit struct {
	Volume     string   `json:"volume"     yaml:"volume"`
	Path       string   `json:"path"       yaml:"path"`
	Containers []string `json:"containers,omitempty" yaml:"containers,omitempty"`
}

// ImageMatch records an image-substring hint that fired a rule.
type ImageMatch struct {
	Container   string `json:"container"   yaml:"container"`
	Image       string `json:"image"       yaml:"image"`
	HintPattern string `json:"hintPattern" yaml:"hintPattern"`
}

// GPURequest records a GPU resource request on a container.
type GPURequest struct {
	Container string `json:"container" yaml:"container"`
	Resource  string `json:"resource"  yaml:"resource"`
	Quantity  string `json:"quantity"  yaml:"quantity"`
}

// EnvVarHit records an env var that signals a feature (e.g. FUSE opt-in).
type EnvVarHit struct {
	Container string `json:"container" yaml:"container"`
	Name      string `json:"name"      yaml:"name"`
	Value     string `json:"value"     yaml:"value"`
}

// AnnotationHit records a pod-template annotation that triggered a rule.
type AnnotationHit struct {
	Key   string `json:"key"   yaml:"key"`
	Value string `json:"value" yaml:"value"`
}

// CSIDriverHit records a CSI driver name (e.g. fuse) that triggered a rule.
type CSIDriverHit struct {
	Volume string `json:"volume" yaml:"volume"`
	Driver string `json:"driver" yaml:"driver"`
}
