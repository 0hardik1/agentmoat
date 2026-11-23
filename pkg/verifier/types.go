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
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	// Plan is the MigrationPlan to verify against. Its Spec.Steps drive
	// the walk; Metadata.PlanHash is carried through to the report so an
	// operator can correlate the verify output with the apply that produced
	// the state being checked. Exactly one of Plan and Targets is set.
	Plan *schema.MigrationPlan

	// Targets is the list of workloads to verify when there is no plan:
	// the workloads of manifest files that ask for the RuntimeClass
	// (`verify -f`). Not nil, and Plan is nil. The checks are the ones of
	// a plan step, in the order of the list.
	Targets []Target

	// InPodProbe, when true, enables the per-step kubectl-exec probe. Off
	// by default: the probe needs the `pods/exec` permission, which is a
	// privileged grant the operator must opt into. With InPodProbe off the
	// verifier only checks the live pods' .spec.runtimeClassName.
	InPodProbe bool

	// Wait, when it is more than zero, makes Verify look at the cluster
	// again until every step is ok or this much time passed. With a wait
	// time, a step is ok only when its pods are also Ready and stay Ready
	// (see wait.go). Zero means one look, with no readiness check.
	Wait time.Duration

	// PollInterval is the time between two looks. Zero means
	// DefaultPollInterval.
	PollInterval time.Duration

	// Settle is how long the pods of a step must stay Ready before the
	// step is ok. Zero means DefaultSettle. It is never more than Wait.
	Settle time.Duration

	// Clock is the test seam for the wait loop. Nil means the system
	// clock.
	Clock Clock

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

// Target is one workload to verify that comes from manifest files and not
// from a plan.
type Target struct {
	// Ref names the workload as it is in the cluster. For one of the six
	// kinds, Kind is the plain kind ("Deployment") and the verifier reads
	// the live object, as it does for a plan step.
	Ref schema.WorkloadRef

	// Expected is the runtimeClassName that the manifest asks for.
	Expected string

	// Selector is for a kind that the verifier has no reader for (an Argo
	// Rollout: Kind is then "Rollout.argoproj.io"). It is spec.selector of
	// the manifest, and it must have terms. The verifier lists the pods
	// that it selects and does not read the object, so it needs no
	// permission on a custom resource. Two things follow, and the result
	// says so (schema.VerifyBasisSelector): pods of another workload with
	// the same labels count too, and with no pods the verifier cannot
	// tell a workload that is scaled to zero from one that is not
	// deployed. Such a target then has no result: it goes to
	// VerifySpec.NotChecked.
	Selector *metav1.LabelSelector

	// Origin is where the manifest input defines the workload. It is
	// copied to the result.
	Origin *schema.Origin
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
