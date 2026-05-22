// Package scanner enumerates Kubernetes workloads from a live cluster and
// reduces each one to a canonical "Workload" value that the classifier can
// consume without knowing anything about the K8s API.
//
// Why a dedicated package
//
//   - The classifier should be a pure function over PodSpec, not over the
//     full Kubernetes object zoo (Pod has a PodSpec directly, Deployment
//     has one under .spec.template.spec, CronJob nests two layers down).
//     Scanner does that unwrapping.
//
//   - Image-label lookups (used in later phases to detect known agent
//     workloads) are I/O. Keeping them on this side of the boundary lets
//     us cache them on disk and replay them deterministically in tests
//     (plan.md section 12.6).
//
// This file declares the data shapes only. The actual cluster enumeration
// lives in scanner.go.
package scanner

import (
	corev1 "k8s.io/api/core/v1"
)

// Workload is the canonical, classifier-ready representation of one K8s
// workload, regardless of the controller it was sourced from.
//
// One Workload corresponds to one *workload owner*: a standalone Pod, a
// Deployment, a StatefulSet, a DaemonSet, a Job, or a CronJob. The Scanner
// avoids double-counting by surfacing the highest-level controller (so a
// Deployment is reported once, not also once per ReplicaSet, not also once
// per Pod).
type Workload struct {
	// Kind is the K8s kind of the owning controller (or "Pod" for
	// standalone pods). One of:
	//   Pod | Deployment | StatefulSet | DaemonSet | Job | CronJob
	Kind string

	// Namespace and Name uniquely identify the workload within Kind.
	Namespace string
	Name      string

	// PodSpec is the pod template spec extracted from the controller, or
	// the spec of the standalone Pod. The classifier reads only this.
	PodSpec corev1.PodSpec

	// Labels and Annotations are taken from the controller's metadata,
	// not the pod template's. They are useful for downstream filtering
	// (e.g. "skip workloads with agentmoat.io/skip=true").
	Labels      map[string]string
	Annotations map[string]string

	// ImageRefs is the flat list of container image references inside the
	// pod spec (init + main containers). Cached here so the classifier and
	// the planner do not each re-derive them.
	ImageRefs []string
}

// EnumerateOptions filters and shapes the cluster scan. All fields are
// optional. Zero value means "all namespaces, no label filter".
type EnumerateOptions struct {
	// Namespaces is the list of namespaces to scan. Empty means all
	// namespaces the kubeconfig has visibility into.
	Namespaces []string

	// LabelSelector is a Kubernetes-style label selector applied to the
	// list calls. Empty means no filter.
	LabelSelector string

	// IncludeSystem, when true, includes kube-system, kube-public,
	// kube-node-lease and other reserved namespaces. Default is false,
	// because operators usually don't want to migrate control-plane addons.
	IncludeSystem bool

	// PageSize is the chunk size passed to the K8s list API. Default 500.
	PageSize int64
}
