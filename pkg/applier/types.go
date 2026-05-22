// Package applier patches running Kubernetes controllers so their pod
// templates land on a gVisor RuntimeClass, and rolls those patches back when
// the operator asks.
//
// Public surface (all in this package; no sub-packages):
//
//   - Apply(ctx, opts) -> *schema.ApplyResult, error
//   - Rollback(ctx, opts) -> *schema.RollbackResult, error
//
// Why a dedicated package
//
//   - The applier is the only mutating component in agentmoat. Keeping it in
//     its own package means the rest of the tree (scanner, classifier,
//     planner, output, schema) can be imported by read-only consumers
//     without dragging in a write-shaped dependency graph.
//
//   - Patch generation is small but kind-sensitive (Pod patches a different
//     JSON path than Deployment, CronJob nests two more levels). One file
//     (patch.go) owns the dispatch; the applier core in applier.go does not
//     need to know about controller layout.
//
//   - The idempotency check (namespace annotation + plan hash) and audit
//     logging are cross-cutting concerns. Putting them next to the
//     mutating code keeps the contract obvious in code review: every
//     mutation goes through Apply()/Rollback(), which always emit an audit
//     line and update the annotation.
//
// This file declares types only. Behavior is in applier.go, patch.go, and
// idempotency.go.
package applier

import (
	"io"

	"github.com/0hardik1/agentmoat/internal/schema"
	"k8s.io/client-go/kubernetes"
)

// Options configures an Apply (or Rollback) run. The caller is the
// orchestrator (pkg/agentmoat.Apply / Rollback), which builds the client and
// reads the plan from disk before invoking this package.
type Options struct {
	// Client is the Kubernetes API client to use. Required.
	Client kubernetes.Interface

	// Plan is the MigrationPlan to apply (or undo, for Rollback).
	// Required and must be non-nil.
	Plan *schema.MigrationPlan

	// DryRun, when true, makes Apply/Rollback compute the patches and
	// report what it WOULD do, without sending any write requests to the
	// API server. Default per plan.md section 12.2 is true; the
	// orchestrator opts mutating callers in explicitly.
	DryRun bool

	// EmitEvents, when true, emits a Kubernetes Event on the affected
	// namespace per mutation. Default true; tests set this to false so
	// they do not have to plumb a fake event sink.
	EmitEvents bool

	// AuditEnabled, when true, appends to the audit log via
	// internal/audit. Default true; tests set this to false to avoid
	// touching the developer's home dir even with the env override.
	AuditEnabled bool

	// Stderr is the writer for progress messages. Defaults to os.Stderr
	// in cmd/. Set to io.Discard to silence.
	Stderr io.Writer

	// Cluster is the kubeconfig context name, threaded through for the
	// ApplyResult / RollbackResult metadata. Optional.
	Cluster string

	// AgentmoatVersion is the binary version string, threaded through for
	// the ApplyResult / RollbackResult metadata. Optional.
	AgentmoatVersion string
}
