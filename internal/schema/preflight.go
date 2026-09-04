// Package schema: preflight and cluster-facts wire types.
//
// These shapes back `agentmoat preflight` (kind: PreflightReport) and the
// optional `metadata.clusterFacts` block that `agentmoat scan` attaches to
// a ScanReport. They exist because the migration has a precondition that
// none of the per-workload verdicts can express: the cluster must have at
// least one Ready node that can actually run runsc, and the RuntimeClass
// must steer pods onto it.
//
// How placement works in Kubernetes, in one paragraph
//
//	A RuntimeClass may carry `scheduling.nodeSelector` and
//	`scheduling.tolerations`. When a pod requests that RuntimeClass, the
//	RuntimeClass admission controller merges the nodeSelector into the
//	pod's own nodeSelector (intersection; a conflict rejects the pod) and
//	unions the tolerations into the pod's tolerations. agentmoat relies on
//	that merge for placement: apply patches only `runtimeClassName`, and
//	the preflight checks that the RuntimeClass and the nodes make the
//	merge land somewhere real.
//
// Every type here is additive to agentmoat.io/v1alpha1: existing documents
// stay valid, and every new field on an existing type is `omitempty`.
package schema

// Node labels and OS markers the preflight inspects. Exported so docs, the
// MCP server, and tests name the same strings.
const (
	// EKSAutoModeLabel / EKSAutoModeLabelValue identify EKS Auto Mode
	// managed instances. AWS owns their (Bottlerocket) image and container
	// runtime; software cannot be installed on them, so runsc can never be
	// present. Source: the EKS user guide, "Control if a workload is
	// deployed on EKS Auto Mode nodes".
	EKSAutoModeLabel      = "eks.amazonaws.com/compute-type"
	EKSAutoModeLabelValue = "auto"

	// KarpenterNodePoolLabel is set on every node Karpenter (and EKS Auto
	// Mode, which embeds Karpenter) provisions. Informational only.
	KarpenterNodePoolLabel = "karpenter.sh/nodepool"

	// BottlerocketOSImagePrefix is how Bottlerocket reports itself in
	// Node.Status.NodeInfo.OSImage. Bottlerocket ships no runsc and has an
	// immutable root filesystem.
	BottlerocketOSImagePrefix = "Bottlerocket OS"
)

// PreflightReport is the top-level envelope of `agentmoat preflight`.
type PreflightReport struct {
	APIVersion string            `json:"apiVersion" yaml:"apiVersion"`
	Kind       string            `json:"kind"       yaml:"kind"`
	Metadata   PreflightMetadata `json:"metadata"   yaml:"metadata"`
	Spec       PreflightSpec     `json:"spec"       yaml:"spec"`
}

// NewPreflightReport returns an empty PreflightReport with the envelope
// filled, ready for pkg/preflight to populate.
func NewPreflightReport() *PreflightReport {
	return &PreflightReport{
		APIVersion: APIVersion,
		Kind:       KindPreflightReport,
	}
}

// PreflightMetadata records when, where, and for which RuntimeClass the
// preflight ran.
type PreflightMetadata struct {
	GeneratedAt      string `json:"generatedAt" yaml:"generatedAt"`
	Cluster          string `json:"cluster,omitempty" yaml:"cluster,omitempty"`
	AgentmoatVersion string `json:"agentmoatVersion" yaml:"agentmoatVersion"`

	// RuntimeClassName is the RuntimeClass the facts and findings describe
	// ("gvisor" unless --runtime-class was passed).
	RuntimeClassName string `json:"runtimeClassName" yaml:"runtimeClassName"`
}

// PreflightSpec is the payload of a PreflightReport: what was observed
// (Facts), what it means (Findings), and the bucketed count (Summary).
type PreflightSpec struct {
	Summary PreflightSummary `json:"summary" yaml:"summary"`
	Facts   ClusterFacts     `json:"facts"   yaml:"facts"`

	// Findings is always non-nil (possibly empty), sorted error -> warn ->
	// info and then by ID, so the same cluster state renders identically.
	Findings []PreflightFinding `json:"findings" yaml:"findings"`
}

// PreflightSummary counts findings by severity. Ready is the load-bearing
// bit: false means at least one error-severity finding exists, apply
// refuses to run, and the CLI exits 5 (docs/exit-codes.md).
type PreflightSummary struct {
	Ready bool `json:"ready" yaml:"ready"`
	Total int  `json:"total" yaml:"total"`
	Error int  `json:"error" yaml:"error"`
	Warn  int  `json:"warn"  yaml:"warn"`
	Info  int  `json:"info"  yaml:"info"`
}

// PreflightFinding is one observation about the cluster. IDs are stable
// identifiers (see docs/preflight.md); scripts may switch on them.
type PreflightFinding struct {
	ID       string   `json:"id"       yaml:"id"`
	Severity Severity `json:"severity" yaml:"severity"`

	// Message says what was observed, with the numbers that justify the
	// severity ("0 of 12 nodes match nodeSelector runtime=gvisor").
	Message string `json:"message" yaml:"message"`

	// Remediation says what to do about it. Empty for info findings that
	// need no action.
	Remediation string `json:"remediation,omitempty" yaml:"remediation,omitempty"`
}

// ClusterFacts is the read-only inventory the preflight collected. It is
// also attached to ScanReport.Metadata so `plan`, which is a pure function
// of the ScanReport and never touches the cluster, can warn about a cluster
// that cannot host the plan.
type ClusterFacts struct {
	RuntimeClass RuntimeClassFacts `json:"runtimeClass" yaml:"runtimeClass"`
	Nodes        NodeFacts         `json:"nodes"        yaml:"nodes"`
	Platform     PlatformFacts     `json:"platform"     yaml:"platform"`
}

// RuntimeClassFacts describes the RuntimeClass object the migration
// targets, as found on the cluster.
type RuntimeClassFacts struct {
	// Name is the RuntimeClass name that was looked up.
	Name string `json:"name" yaml:"name"`

	// Found is false when no RuntimeClass with that name exists. Every
	// other field is then zero.
	Found bool `json:"found" yaml:"found"`

	// Handler is the containerd runtime handler name ("gvisor" in the
	// shipped manifests). Kubernetes cannot tell us whether the nodes'
	// containerd config registers it; `verify --in-pod-probe` is the
	// runtime check for that.
	Handler string `json:"handler,omitempty" yaml:"handler,omitempty"`

	// NodeSelector is scheduling.nodeSelector. Empty means the
	// RuntimeClass does not steer pods anywhere, which the preflight
	// reports as an error.
	NodeSelector map[string]string `json:"nodeSelector,omitempty" yaml:"nodeSelector,omitempty"`

	// Tolerations is the number of scheduling.tolerations entries.
	Tolerations int `json:"tolerations" yaml:"tolerations"`

	// Overhead is overhead.podFixed rendered as resource -> quantity
	// ("cpu": "250m", "memory": "140Mi").
	Overhead map[string]string `json:"overhead,omitempty" yaml:"overhead,omitempty"`
}

// NodeFacts counts nodes against the RuntimeClass nodeSelector. "Matching"
// means the node's labels satisfy scheduling.nodeSelector; when the
// RuntimeClass has no selector every Matching* count is zero.
type NodeFacts struct {
	Total            int `json:"total"            yaml:"total"`
	Ready            int `json:"ready"            yaml:"ready"`
	MatchingSelector int `json:"matchingSelector" yaml:"matchingSelector"`
	MatchingAndReady int `json:"matchingAndReady" yaml:"matchingAndReady"`

	// MatchingTaintedWithoutToleration counts matching nodes that carry a
	// NoSchedule or NoExecute taint that none of the RuntimeClass's
	// scheduling.tolerations tolerate. Pods admitted with this
	// RuntimeClass cannot land on those nodes.
	MatchingTaintedWithoutToleration int `json:"matchingTaintedWithoutToleration" yaml:"matchingTaintedWithoutToleration"`

	// MatchingNames lists the matching nodes, sorted, so an operator can
	// cross-check with `kubectl get nodes -l <selector>`.
	MatchingNames []string `json:"matchingNames,omitempty" yaml:"matchingNames,omitempty"`
}

// PlatformFacts counts nodes whose platform rules out runsc. The
// *MatchingNodes variants count only nodes that match the RuntimeClass
// nodeSelector; the plain variants count the whole cluster.
type PlatformFacts struct {
	EKSAutoModeNodes         int `json:"eksAutoModeNodes"         yaml:"eksAutoModeNodes"`
	EKSAutoModeMatchingNodes int `json:"eksAutoModeMatchingNodes" yaml:"eksAutoModeMatchingNodes"`

	// Bottlerocket counts exclude EKS Auto Mode nodes (those are counted
	// above even though they also run Bottlerocket).
	BottlerocketNodes         int `json:"bottlerocketNodes"         yaml:"bottlerocketNodes"`
	BottlerocketMatchingNodes int `json:"bottlerocketMatchingNodes" yaml:"bottlerocketMatchingNodes"`

	// KarpenterNodes is informational: Karpenter-provisioned nodes are
	// fine as long as their NodeClass uses an AMI that ships runsc.
	KarpenterNodes int `json:"karpenterNodes" yaml:"karpenterNodes"`
}

// PlanWarning is a plan-level caution derived from the ScanReport's
// ClusterFacts. It shares IDs with PreflightFinding. Warnings are NOT part
// of the plan hash: the same workloads produce the same hash whether or not
// the cluster was ready when the scan ran.
type PlanWarning struct {
	ID      string `json:"id"      yaml:"id"`
	Message string `json:"message" yaml:"message"`
}

// NodePlacement is the verifier's answer to "did the pods land on nodes the
// RuntimeClass selects?". A pod can carry runtimeClassName=gvisor and still
// run on a runc node when the RuntimeClass has no nodeSelector; this block
// is how verify catches that.
type NodePlacement struct {
	// Checked is false when the verifier could not evaluate placement:
	// the RuntimeClass was missing or unreadable, or it has no
	// scheduling.nodeSelector. Message explains which.
	Checked bool `json:"checked" yaml:"checked"`

	// Selector is the RuntimeClass scheduling.nodeSelector the nodes were
	// checked against.
	Selector map[string]string `json:"selector,omitempty" yaml:"selector,omitempty"`

	// Nodes lists the distinct nodes the step's pods run on, sorted.
	Nodes []string `json:"nodes,omitempty" yaml:"nodes,omitempty"`

	// Mismatched lists the nodes in Nodes that do not satisfy Selector,
	// sorted. Non-empty demotes the step to mismatch.
	Mismatched []string `json:"mismatched,omitempty" yaml:"mismatched,omitempty"`

	// Message is a one-sentence explanation, always set when Checked is
	// false and when Mismatched is non-empty.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
}
