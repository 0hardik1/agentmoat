// Package verifier: the default in-pod probe ExecRunner.
//
// What this file is responsible for
//
//   - Implementing the ExecRunner interface (types.go) against a live API
//     server. The verifier core in verifier.go calls Exec() once per step
//     when --in-pod-probe is on. The probe is the difference between
//     "the controller says we're on gVisor" (a spec-level claim) and
//     "/proc inside the pod also says we're on gVisor" (the reality).
//
// How the probe reaches the container
//
//   - kubectl-exec under the hood: the K8s API server exposes a per-pod
//     subresource at POST /api/v1/namespaces/{ns}/pods/{pod}/exec. The body
//     is upgraded to a SPDY (or websocket) stream and the kubelet on the
//     node pipes stdin/stdout/stderr through to the container runtime.
//
//   - We use k8s.io/client-go/tools/remotecommand.NewSPDYExecutor for the
//     transport. SPDY has been the kubectl-exec default since K8s 1.0 and
//     works on every supported control plane; websocket exec (newer) is
//     not yet universally available. Picking SPDY keeps the verifier
//     functional on older clusters too.
//
// Why this lives in its own file
//
//   - The SPDY exec dependency pulls in a chunk of streaming machinery
//     (bytes buffers, executor, scheme codec). Keeping it segregated means
//     the verifier core (verifier.go) reads as plain K8s typed-API code
//     and the probe-specific concerns are isolated for review.
//
//   - A future "websocket exec" implementation can drop in as a second
//     ExecRunner without touching verifier.go.
package verifier

import (
	"bytes"
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// defaultExecRunner is the production implementation of ExecRunner. It
// dials the API server's exec subresource over SPDY. Constructed lazily
// in verifier.go when opts.InPodProbe is true and opts.Exec is nil.
type defaultExecRunner struct {
	// client is used only to obtain the REST URL for the exec subresource
	// (CoreV1().RESTClient().Post() with the right resource/name/subresource
	// path builders). The actual stream is opened by NewSPDYExecutor on the
	// *rest.Config below; the client itself is not used to stream bytes.
	client kubernetes.Interface

	// config is required by remotecommand.NewSPDYExecutor: it carries the
	// TLS material and the bearer token that the SPDY transport will
	// re-use for the upgraded stream. The typed Clientset does not expose
	// these, which is why Options.Config is a separate field.
	config *rest.Config
}

// newDefaultExecRunner returns the production ExecRunner. The verifier
// builds it lazily so test runs with a fake ExecRunner never touch
// remotecommand.NewSPDYExecutor.
func newDefaultExecRunner(client kubernetes.Interface, cfg *rest.Config) *defaultExecRunner {
	return &defaultExecRunner{client: client, config: cfg}
}

// Exec opens a SPDY stream to the named pod's exec subresource, runs cmd,
// and returns the captured stdout, stderr, and any error from the
// transport itself or a non-zero exit code in the container.
//
// The container argument may be "" to let the API server pick the pod's
// first container; that matches the behavior of `kubectl exec` without
// `-c`. Our migrated workloads tend to be single-container, so the
// default suffices for the verifier's probe.
func (r *defaultExecRunner) Exec(ctx context.Context, namespace, pod, container string, cmd []string) (stdout, stderr []byte, err error) {
	// Build the typed REST request: POST /api/v1/namespaces/<ns>/pods/<pod>/exec
	// with the PodExecOptions encoded into query parameters. scheme.ParameterCodec
	// is the codec that knows how to URL-encode a PodExecOptions value
	// (Container, Command, Stdin/Stdout/Stderr/TTY) into the query string the
	// API server expects. Doing it via the typed client (rather than
	// hand-assembling the URL) keeps the verifier honest if the API ever
	// gains a new query field.
	req := r.client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			// We do not need stdin: the probe runs a self-contained
			// shell pipeline (no interactive input). Stdout and stderr
			// are both requested so a script that writes its result to
			// stderr (uname does not, but dmesg sometimes does on
			// older kernels) still surfaces.
			Stdin:  false,
			Stdout: true,
			Stderr: true,
			TTY:    false,
		}, scheme.ParameterCodec)

	// NewSPDYExecutor performs the initial HTTP request and the SPDY
	// upgrade. Errors here are transport-level: bad URL, auth failure,
	// the server does not support SPDY, etc.
	executor, err := remotecommand.NewSPDYExecutor(r.config, "POST", req.URL())
	if err != nil {
		return nil, nil, err
	}

	// Stream returns when the remote command exits. A non-zero exit code
	// surfaces as an error of type *exec.CodeExitError; the caller can
	// type-assert if it wants the code, but most call sites just need to
	// know the probe failed.
	var outBuf, errBuf bytes.Buffer
	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &outBuf,
		Stderr: &errBuf,
		Tty:    false,
	})
	return outBuf.Bytes(), errBuf.Bytes(), err
}
