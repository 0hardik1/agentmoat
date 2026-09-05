// Package agentmoat is the public Go entrypoint for the agentmoat toolkit.
//
// Both the CLI (`cmd/agentmoat`) and the MCP server
// (`cmd/agentmoat-mcp`) call into this package. Keeping the orchestration
// here, not in cmd/, is what makes agentmoat embeddable: a third party can
// `import "github.com/0hardik1/agentmoat/pkg/agentmoat"` and call Scan()
// without spawning a child process.
//
// This file declares the options structs. Behavior lives in agentmoat.go.
package agentmoat

import (
	"io"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/verifier"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
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

	// RuntimeClassName names the RuntimeClass whose scheduling block the
	// scan inspects for ReportMetadata.ClusterFacts (nodes matching its
	// nodeSelector, taints, EKS Auto Mode markers). Empty means
	// schema.DefaultRuntimeClassName. It does not affect classification.
	RuntimeClassName string

	// SkipClusterFacts turns off the node + RuntimeClass inventory. The
	// scan already degrades to "no facts" (with a stderr warning) when
	// the identity lacks get/list on nodes and runtimeclasses, so this
	// switch is for operators who do not want a node list issued at all.
	// Facts also feed the classifier (gpu-passthrough refinement), so
	// skipping them also turns that refinement off.
	SkipClusterFacts bool

	// FactsPath, when non-empty, loads ClusterFacts from a saved
	// PreflightReport (spec.facts) or ScanReport (metadata.clusterFacts)
	// instead of reading the cluster. This is how the driver list from
	// `agentmoat probe nvproxy --dry-run=false --output json` reaches the
	// classifier: the probe creates a pod, the scan must not, so the scan
	// reads the probe's report. A file that cannot be read or has no
	// facts is an error (the operator asked for it). Takes precedence
	// over SkipClusterFacts.
	FactsPath string

	// Stderr is the writer used for human-readable progress messages
	// (counts, timings, "scanning namespace X..."). Defaults to os.Stderr
	// in cmd/. Set to io.Discard to silence.
	Stderr io.Writer

	// KubeClient is an optional pre-built Kubernetes client. When non-nil,
	// Scan() skips its internal kube.NewClient call and uses this client
	// directly. Used by the MCP server's unit tests to inject a fake
	// clientset (pkg/applier/applier_test.go pattern). Production callers
	// (CLI) leave this nil so the standard kubeconfig loader runs.
	KubeClient kubernetes.Interface
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
	// Required.
	PlanPath string

	// DryRun defaults to true: dry-run is the safe default, so callers
	// must set this to false explicitly to mutate the cluster.
	DryRun bool

	// EmitEvents, when true, emits one Kubernetes Event per mutation on
	// the affected namespace. Default true.
	EmitEvents bool

	// AuditEnabled, when true, appends to ~/.agentmoat/audit.jsonl per
	// mutation. Default true.
	AuditEnabled bool

	// SkipPreflight bypasses the cluster preflight Apply runs before it
	// touches any step (pkg/preflight: the RuntimeClass exists, steers
	// pods to at least one Ready runsc node, and no EKS Auto Mode or
	// Bottlerocket dead end). Default false. A blocked preflight returns
	// an ApplyResult with every step skipped, Metadata.Preflight.Ready
	// false, and no mutation; the CLI exits 5. The gate runs in dry-run
	// too, so a preview cannot look greener than the real thing.
	// Rollback ignores this field: moving pods back to runc needs no
	// gVisor node, so it never runs the preflight.
	SkipPreflight bool

	// Stderr is the writer for progress messages.
	Stderr io.Writer

	// KubeClient is an optional pre-built Kubernetes client. See the
	// equivalent field on ScanOptions. Used only by the MCP server's
	// unit tests; CLI callers leave it nil.
	KubeClient kubernetes.Interface
}

// RollbackOptions is the rollback-side counterpart of ApplyOptions. Fields
// kept identical so callers can copy-paste between commands.
type RollbackOptions = ApplyOptions

// PreflightOptions configures pkg/agentmoat.Preflight, the read-only
// "can this cluster host the migration at all?" check. It reads one
// RuntimeClass and the node list; it never touches workloads.
type PreflightOptions struct {
	// KubeconfigPath and Context are the same as ScanOptions.
	KubeconfigPath string
	Context        string

	// RuntimeClassName is the RuntimeClass to inspect. Empty means
	// schema.DefaultRuntimeClassName ("gvisor").
	RuntimeClassName string

	// Stderr is the writer for progress messages.
	Stderr io.Writer

	// KubeClient is an optional pre-built Kubernetes client. See ScanOptions.
	KubeClient kubernetes.Interface
}

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

	// KubeClient is an optional pre-built Kubernetes client. See ScanOptions.
	KubeClient kubernetes.Interface

	// RestConfig is the rest.Config that backs the in-pod probe (the SPDY
	// exec transport needs TLS material that the typed Clientset does not
	// expose). Production callers leave this nil; the orchestrator builds
	// both client and config together via kube.NewClient. Tests that inject
	// a fake KubeClient and a fake ExecRunner can leave this nil too, since
	// the fake ExecRunner short-circuits before SPDY is dialed.
	RestConfig *rest.Config

	// ExecRunner is the testing seam for the in-pod probe. When non-nil
	// AND InPodProbe is true, the verifier uses this ExecRunner instead
	// of building the default SPDY one. CLI callers leave this nil so
	// the production probe runs.
	ExecRunner verifier.ExecRunner
}

// AssessWorkloadOptions configures pkg/agentmoat.AssessWorkload, the
// single-workload counterpart to Scan. The MCP server's assess_workload
// tool calls this so an operator (or an LLM acting on behalf of one) can
// classify one named workload without paying the cost of a cluster-wide
// scan.
type AssessWorkloadOptions struct {
	// KubeconfigPath and Context are the same as ScanOptions.
	KubeconfigPath string
	Context        string

	// Kind is the Kubernetes kind: Pod | Deployment | StatefulSet |
	// DaemonSet | Job | CronJob. Required.
	Kind string

	// Namespace and Name uniquely identify the target workload. Required.
	Namespace string
	Name      string

	// RulesYAMLPath, when non-empty, layers a YAML override file on top
	// of the built-in classifier rules. Same semantics as ScanOptions.
	RulesYAMLPath string

	// RuntimeClassName, SkipClusterFacts, and FactsPath control the
	// cluster facts the classifier refines against. Same semantics as
	// the ScanOptions fields of the same names: facts are best-effort
	// from the cluster unless FactsPath names a saved report.
	RuntimeClassName string
	SkipClusterFacts bool
	FactsPath        string

	// Stderr is the writer for progress messages.
	Stderr io.Writer

	// KubeClient is an optional pre-built Kubernetes client. See ScanOptions.
	KubeClient kubernetes.Interface
}

// ProbeOptions configures pkg/agentmoat.Probe, the nvproxy probe: a
// one-shot pod that reads the runsc binary on a gVisor node and reports
// which NVIDIA host driver versions its nvproxy supports. It is the only
// verb besides apply and rollback that creates anything, so it follows the
// same convention: DryRun defaults to true in cmd/ and the MCP tool, and
// a caller must set it to false explicitly to create the pod.
type ProbeOptions struct {
	// KubeconfigPath and Context are the same as ScanOptions.
	KubeconfigPath string
	Context        string

	// RuntimeClassName selects the node pool the pod is pinned to. Empty
	// means schema.DefaultRuntimeClassName.
	RuntimeClassName string

	// Namespace, Image, RunscPath, and Timeout default to the pkg/probe
	// constants (default, busybox:1.36.1, /usr/local/bin/runsc, 2m) when
	// zero.
	Namespace string
	Image     string
	RunscPath string
	Timeout   time.Duration

	// DryRun, when true, creates nothing and reports the pod that would
	// have been created.
	DryRun bool

	// Stderr is the writer for progress messages.
	Stderr io.Writer

	// KubeClient is an optional pre-built Kubernetes client. See ScanOptions.
	KubeClient kubernetes.Interface
}

// ExplainOptions configures pkg/agentmoat.Explain. The orchestrator
// supports three modes; the fields below describe which one the caller
// has selected.
//
// Mode 1: static-topic (Topic != "", Namespace == ""). Offline; reads the
// embedded docs/*.md content and returns a topic envelope. Backwards
// compatible with the previous Explain shape.
//
// Mode 2: list (Topic == "", Namespace == ""). Offline; returns just the
// list of available topics (Spec.Topics).
//
// Mode 3: deep (Namespace != ""). Connects to the cluster like Scan does,
// classifies the workloads in the named namespace, and attaches a
// NamespaceExplanation to the document. When Workload is non-nil the
// document is further narrowed to the single matching workload, but the
// envelope shape is identical (a NamespaceExplanation with one entry).
type ExplainOptions struct {
	// Topic is the static-topic name. Empty means "list" mode (or
	// "deep" mode when Namespace is set). Lookups are case-insensitive
	// (handled in pkg/explainer).
	Topic string

	// Namespace selects deep mode. When non-empty the orchestrator
	// performs a real scan + classify against this namespace and the
	// Workload filter, ignoring Topic.
	Namespace string

	// Workload, when non-nil, narrows a deep-mode document to a single
	// workload in the named namespace. Requires Namespace to be set.
	// Nil means "every workload in the namespace".
	Workload *WorkloadFilter

	// Scan carries the kube/classifier configuration used by deep mode
	// (kubeconfig path, context, label selector, rules override, etc.).
	// Ignored in static-topic and list modes.
	Scan ScanOptions
}

// WorkloadFilter narrows a deep-mode explanation to a single workload by
// name (and optionally by kind, to disambiguate when two controllers in
// the same namespace happen to share a name).
type WorkloadFilter struct {
	// Kind is the optional disambiguator: "Deployment", "StatefulSet",
	// "DaemonSet", "Job", "CronJob", or "Pod". Empty means "match any
	// kind"; the caller takes responsibility if multiple workloads then
	// share the Name.
	Kind string

	// Name is the workload name. Required when WorkloadFilter is set.
	Name string
}
