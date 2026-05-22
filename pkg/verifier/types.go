// Package verifier performs a read-only audit of a previously-applied
// MigrationPlan against the live state of a Kubernetes cluster. It is the
// "did the apply actually take?" half of agentmoat's apply/verify loop.
//
// What the verifier does (and does not do)
//
//   - It takes a MigrationPlan and walks its steps in order. For every step
//     it resolves the workload (a Pod or a controller such as a Deployment,
//     StatefulSet, DaemonSet, Job, or CronJob), then inspects the live pods
//     that controller selects. Each pod's `.spec.runtimeClassName` is
//     compared against what the plan asked for (typically "gvisor").
//
//   - It is strictly read-only. No patches, no annotations, no Events. The
//     output is a *schema.VerifyReport with one VerifyResult per plan step,
//     plus a summary. Per-step errors do not make Verify return a Go error;
//     only precondition failures (nil plan/client) do. This mirrors the
//     applier's contract so the orchestrator can shape exit codes uniformly
//     (see docs/exit-codes.md).
//
//   - When InPodProbe is on the verifier also executes a small shell snippet
//     inside one Running pod per step (via the kubelet's exec subresource).
//     The probe greps for distinctive gVisor markers in dmesg, /proc/cmdline,
//     and uname output. A pod whose runtimeClassName says "gvisor" but whose
//     in-pod probe finds no gVisor markers is downgraded to "mismatch": the
//     spec advertises gVisor but the kernel surface inside the pod does not.
//
// Why a separate package
//
//   - Like the applier, the verifier needs the K8s API client, the schema
//     types, and (optionally) the SPDY exec machinery. Keeping it in its own
//     package means consumers that only want to scan or plan do not pull in
//     remotecommand.NewSPDYExecutor and the larger surface that comes with
//     it.
//
//   - The verifier's logic is testable purely against a fake K8s clientset.
//     The exec runner is a single-method interface (ExecRunner) so tests can
//     supply a canned implementation without standing up a real cluster.
//     Production code wires in the default SPDY-based runner from probe.go.
//
// This file declares types only. Behavior is in verifier.go and probe.go.
package verifier

import (
	"context"
	"io"

	"github.com/0hardik1/agentmoat/internal/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Options configures one Verify run. Filled by the orchestrator
// (pkg/agentmoat.Verify), which builds the K8s client and loads the plan
// from disk before calling Verify.
type Options struct {
	// Client is the Kubernetes API client to use. Required.
	Client kubernetes.Interface

	// Config is the underlying REST config. Required only when InPodProbe
	// is true AND Exec is nil: in that case Verify builds the default
	// ExecRunner (probe.go), which dials the API server's exec subresource
	// over SPDY and needs the bearer token / TLS material that lives on
	// the *rest.Config (the typed Clientset does not expose it).
	Config *rest.Config

	// Plan is the MigrationPlan to verify against. Required and must be
	// non-nil. Its Spec.Steps drive the walk; Metadata.PlanHash is carried
	// through to the report so an operator can correlate the verify output
	// with the apply that produced the state being checked.
	Plan *schema.MigrationPlan

	// InPodProbe, when true, enables the per-step kubectl-exec probe. Off
	// by default: the probe needs the `pods/exec` permission, which is a
	// privileged grant the operator must opt into. With InPodProbe off the
	// verifier only checks the live pods' .spec.runtimeClassName.
	InPodProbe bool

	// Exec is the test seam for the in-pod probe. Tests supply a fake
	// implementation that returns canned stdout/stderr/exit. When nil and
	// InPodProbe is true, Verify constructs the default SPDY-based runner
	// using opts.Client and opts.Config.
	Exec ExecRunner

	// Stderr is the writer for progress messages. Defaults to os.Stderr.
	// Set to io.Discard to silence.
	Stderr io.Writer

	// Cluster is the kubeconfig context name (best-effort), threaded
	// through for VerifyReport.Metadata.Cluster.
	Cluster string

	// AgentmoatVersion is the binary version string, threaded through
	// for VerifyReport.Metadata.AgentmoatVersion.
	AgentmoatVersion string
}

// ExecRunner is the seam for the kubectl-exec-based gVisor probe. The
// production implementation lives in probe.go and uses
// k8s.io/client-go/tools/remotecommand's SPDY executor to POST to the
// /api/v1/namespaces/{ns}/pods/{pod}/exec subresource. Tests pass a fake
// that returns canned stdout/stderr/exit so they do not have to stand up
// the SPDY transport.
//
// Method contract:
//
//   - cmd is an exec.Cmd-style argv: cmd[0] is the executable, cmd[1:]
//     are arguments. The K8s API server passes the slice through to the
//     kubelet, which invokes it inside the container without a shell, so
//     callers that need shell evaluation must pass `["sh", "-c", "..."]`
//     themselves. The default probe does exactly that.
//
//   - container may be "". A blank container name makes the API server
//     pick the pod's first container, which is the right default for the
//     single-container workloads we typically migrate.
//
//   - stdout/stderr are the raw byte streams the command produced. err is
//     non-nil if the exec call itself failed (connection error, container
//     missing, non-zero exit code, etc.). A non-zero exit returns err so
//     the caller can branch without parsing the stream content.
type ExecRunner interface {
	Exec(ctx context.Context, namespace, pod, container string, cmd []string) (stdout, stderr []byte, err error)
}
