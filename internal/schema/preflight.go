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

// NVIDIA markers the GPU facts read. GPU Feature Discovery (GFD, part of
// the NVIDIA GPU Operator and of the device plugin Helm chart) publishes
// the card model and the host driver version as node labels; the device
// plugin advertises the GPUs as an extended resource. gVisor's nvproxy
// supports a short list of cards and needs an exact host-driver match, so
// these labels are what decide whether a GPU workload can move.
const (
	// GFDProductLabel is the card model as GFD writes it, with spaces
	// replaced by dashes: "Tesla-T4", "NVIDIA-A10G", "NVIDIA-H100-80GB-HBM3".
	GFDProductLabel = "nvidia.com/gpu.product"

	// GFDCountLabel is the number of physical GPUs on the node.
	GFDCountLabel = "nvidia.com/gpu.count"

	// GFDDriverVersionLabel is the full host driver version ("535.183.06")
	// on GFD 0.15 and newer.
	GFDDriverVersionLabel = "nvidia.com/cuda.driver-version.full"

	// GFDDriverMajorLabel, GFDDriverMinorLabel, GFDDriverRevLabel are the
	// three-part form older GFD releases publish; they are joined with
	// dots when the full label is absent.
	GFDDriverMajorLabel = "nvidia.com/cuda.driver.major"
	GFDDriverMinorLabel = "nvidia.com/cuda.driver.minor"
	GFDDriverRevLabel   = "nvidia.com/cuda.driver.rev"

	// GFDMIGStrategyLabel is "none", "single", or "mixed". Anything other
	// than "none" means the node slices its GPUs with MIG, which nvproxy
	// does not support.
	GFDMIGStrategyLabel = "nvidia.com/mig.strategy"

	// NvidiaGPUResource is the extended resource the device plugin
	// advertises for whole GPUs. NvidiaResourcePrefix covers it together
	// with the shared (`nvidia.com/gpu.shared`) and MIG
	// (`nvidia.com/mig-1g.5gb`) variants; NvidiaMIGResourcePrefix is the
	// MIG subset.
	NvidiaGPUResource       = "nvidia.com/gpu"
	NvidiaResourcePrefix    = "nvidia.com/"
	NvidiaMIGResourcePrefix = "nvidia.com/mig-"
)

// SupportStatus is the three-way answer to "does gVisor nvproxy support
// this?". Unknown means the facts needed to decide were not available
// (no GFD label, or the driver list has not been probed yet).
type SupportStatus string

const (
	SupportSupported   SupportStatus = "supported"
	SupportUnsupported SupportStatus = "unsupported"
	SupportUnknown     SupportStatus = "unknown"
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

	// Probe is set when the report came from `agentmoat probe nvproxy`,
	// which extends the preflight with a one-shot pod that reads the runsc
	// binary on a gVisor node. Nil for a plain preflight.
	Probe *ProbeMetadata `json:"probe,omitempty" yaml:"probe,omitempty"`
}

// ProbeMetadata records what the nvproxy probe did (or, in dry-run, what
// it would have done). The pod is created and deleted by the probe; these
// fields are how an operator audits it afterwards.
type ProbeMetadata struct {
	// DryRun is true when no pod was created. The other fields then
	// describe the pod the probe would create.
	DryRun bool `json:"dryRun" yaml:"dryRun"`

	// Namespace and PodName identify the probe pod.
	Namespace string `json:"namespace" yaml:"namespace"`
	PodName   string `json:"podName"   yaml:"podName"`

	// Node is the node the pod was pinned to: a Ready node matching the
	// RuntimeClass nodeSelector, preferring one that has GPUs.
	Node string `json:"node,omitempty" yaml:"node,omitempty"`

	// Image is the container image; RunscPath is the host path of the
	// runsc binary the pod mounts read-only.
	Image     string `json:"image"     yaml:"image"`
	RunscPath string `json:"runscPath" yaml:"runscPath"`

	// Succeeded is true when the pod ran to completion and its output
	// parsed. False in dry-run and when the probe failed (see the
	// nvproxy-probe-failed finding for why).
	Succeeded bool `json:"succeeded" yaml:"succeeded"`
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

	// GPU describes the cluster's NVIDIA GPU nodes and, after `agentmoat
	// probe nvproxy` has run, the driver versions the installed runsc
	// supports. Nil when no node advertises a GPU and no probe ran. The
	// classifier reads this to refine the gpu-passthrough verdict.
	GPU *GPUFacts `json:"gpu,omitempty" yaml:"gpu,omitempty"`
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

// GPUFacts is the GPU inventory. A node counts as a GPU node when it
// advertises the nvidia.com/gpu extended resource or carries the GFD
// product label. Nodes are grouped by (product, driver, MIG) because that
// triple is exactly what decides nvproxy support.
type GPUFacts struct {
	// Nodes is the number of GPU nodes; MatchingNodes how many of them
	// match the RuntimeClass nodeSelector (only those can host a gVisor
	// GPU pod).
	Nodes         int `json:"nodes"         yaml:"nodes"`
	MatchingNodes int `json:"matchingNodes" yaml:"matchingNodes"`

	// MIGNodes counts GPU nodes whose GFD MIG strategy is not "none".
	MIGNodes int `json:"migNodes" yaml:"migNodes"`

	// Groups lists the distinct (product, driver, MIG) combinations,
	// sorted by product then driver, each with its node counts and the
	// support verdicts. Empty only when Nodes is 0.
	Groups []GPUNodeGroup `json:"groups,omitempty" yaml:"groups,omitempty"`

	// Nvproxy is what `agentmoat probe nvproxy` read from the runsc
	// binary on a gVisor node. Nil until the probe has run; the driver
	// verdict in every group is then "unknown".
	Nvproxy *NvproxyFacts `json:"nvproxy,omitempty" yaml:"nvproxy,omitempty"`
}

// GPUNodeGroup is one (product, driver, MIG) combination and the nodes
// that share it.
type GPUNodeGroup struct {
	// Product is the GFD gpu.product label value. Empty when the nodes
	// advertise nvidia.com/gpu but carry no GFD labels.
	Product string `json:"product,omitempty" yaml:"product,omitempty"`

	// Driver is the host driver version from the GFD labels, "" when
	// unknown.
	Driver string `json:"driver,omitempty" yaml:"driver,omitempty"`

	// MIG is true when the nodes slice their GPUs with MIG.
	MIG bool `json:"mig" yaml:"mig"`

	// Nodes and MatchingNodes are the group's node counts, the latter
	// restricted to nodes matching the RuntimeClass nodeSelector.
	Nodes         int `json:"nodes"         yaml:"nodes"`
	MatchingNodes int `json:"matchingNodes" yaml:"matchingNodes"`

	// ProductSupport says whether nvproxy supports the card (decided from
	// the compiled-in list of supported models); DriverSupport whether
	// the installed runsc lists the driver (decided from Nvproxy, so
	// "unknown" until the probe has run).
	ProductSupport SupportStatus `json:"productSupport" yaml:"productSupport"`
	DriverSupport  SupportStatus `json:"driverSupport"  yaml:"driverSupport"`
}

// NvproxyFacts is what the probe pod read from the runsc binary.
type NvproxyFacts struct {
	// RunscVersion is the `runsc --version` release string
	// ("release-20260817.0").
	RunscVersion string `json:"runscVersion" yaml:"runscVersion"`

	// SupportedDrivers is the output of `runsc nvproxy
	// list-supported-drivers`, one host driver version per entry, sorted.
	SupportedDrivers []string `json:"supportedDrivers" yaml:"supportedDrivers"`

	// Node is where the probe ran; ProbedAt when (RFC3339).
	Node     string `json:"node"     yaml:"node"`
	ProbedAt string `json:"probedAt" yaml:"probedAt"`
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
