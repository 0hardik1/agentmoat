// Package preflight answers one question before anything is mutated: can
// this cluster actually run pods that request the target RuntimeClass?
//
// The per-workload classifier (pkg/classifier) decides whether a workload
// is compatible with gVisor. Nothing there can notice that the cluster has
// no gVisor node, that the RuntimeClass steers pods nowhere, or that every
// candidate node is an EKS Auto Mode instance on which runsc can never be
// installed. Without this check `apply` succeeds (the API server accepts
// the patch) and the pods sit Pending or, worse, run on runc while the
// spec says gVisor. The preflight makes that failure loud and early.
//
// Three functions, deliberately separated:
//
//   - Collect reads the cluster (RuntimeClass + Nodes, get/list only) and
//     returns schema.ClusterFacts. It is the only function here that
//     touches the API.
//   - Evaluate is a pure function from ClusterFacts to findings. Being
//     pure lets `plan`, which never touches the cluster, reuse it on the
//     facts stored in a ScanReport, and lets tests cover every severity
//     branch without a client.
//   - Run is Collect + Evaluate + the PreflightReport envelope. `agentmoat
//     preflight`, the MCP tool, and the apply gate all call Run.
//
// What "ready" means: no error-severity finding. Warnings and info never
// block. The finding IDs are stable identifiers documented in
// docs/preflight.md.
//
// RBAC: get on node.k8s.io/runtimeclasses and list on core/nodes
// (deploy/clusterrole-readonly.yaml). Nothing is written.
package preflight

import (
	"k8s.io/client-go/kubernetes"
)

// Options configures one Run.
type Options struct {
	// Client is the Kubernetes API client. Required.
	Client kubernetes.Interface

	// RuntimeClassName is the RuntimeClass to inspect. Empty means
	// schema.DefaultRuntimeClassName ("gvisor").
	RuntimeClassName string

	// Cluster is the kubeconfig context name (best-effort), recorded in
	// PreflightReport.Metadata.Cluster.
	Cluster string

	// AgentmoatVersion is the binary version, recorded in the report.
	AgentmoatVersion string
}
