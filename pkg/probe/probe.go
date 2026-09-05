// Package probe runs the nvproxy probe: a one-shot pod that reads the runsc
// binary installed on a gVisor node and reports which NVIDIA host driver
// versions its nvproxy supports.
//
// Why a pod at all. Everything else agentmoat knows about the cluster comes
// from the API server. The nvproxy driver list does not: it is compiled into
// runsc, and the only way to read it is to run `runsc nvproxy
// list-supported-drivers` on a node that has runsc. So the probe schedules a
// tiny pod onto such a node, mounts the runsc binary read-only from the host
// (hostPath, type File), runs it as an unprivileged user, and reads the pod
// log. The pod does NOT request the gVisor RuntimeClass: it must run under
// runc so the binary it executes is the host's runsc, not a copy inside a
// sandbox.
//
// What the probe is not. It does not exercise a GPU, load CUDA, or touch
// /dev/nvidia*. It answers one narrow question ("does this runsc know this
// driver version?") that GFD labels plus the runsc binary answer exactly.
// The end-to-end validation stays where docs/explanations/gpu-passthrough.md
// puts it: a CUDA smoke test in a real gVisor pod on the target node.
//
// Safety. The probe creates one pod and deletes it, nothing else. It follows
// the apply convention: dry-run by default, in which case it reports the
// pod it would create (namespace, name, node, image, host path) and creates
// nothing. The pod carries a hostPath mount, which Pod Security Admission
// "baseline" forbids; run it in a namespace that allows privileged pods
// (deploy/nvproxy-probe.yaml ships one) or in a namespace without PSA
// enforcement. RBAC: create/get/delete on pods and get on pods/log in that
// namespace, plus the preflight's get/list on nodes and runtimeclasses.
//
// Output. Run returns a PreflightReport, the same document `agentmoat
// preflight` produces, with Metadata.Probe describing the pod and
// Spec.Facts.GPU.Nvproxy holding the runsc version and driver list. Feed the
// saved report to `scan --facts` (or `plan`, `explain`, `assess_workload`)
// and the classifier uses it to settle the gpu-passthrough verdict.
package probe

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Defaults. Exported so the CLI flags and the MCP tool schema quote the
// same values.
const (
	// DefaultNamespace is where the probe pod is created. "default" needs
	// no setup on kind or a fresh EKS cluster; clusters that enforce Pod
	// Security Admission need a namespace labeled privileged, see
	// deploy/nvproxy-probe.yaml.
	DefaultNamespace = "default"

	// DefaultImage only needs /bin/sh; runsc is a static binary read from
	// the host. busybox is small and on every mirror.
	DefaultImage = "busybox:1.36.1"

	// DefaultRunscPath is where the agentmoat AMI (packer/) and the kind
	// node image (kind/Dockerfile.gvisor-node) install runsc.
	DefaultRunscPath = "/usr/local/bin/runsc"

	// DefaultTimeout bounds the wait for the pod to complete; the image
	// pull dominates on a cold node.
	DefaultTimeout = 2 * time.Minute

	// PodName is fixed so a leftover from an interrupted run is easy to
	// find and so two concurrent probes cannot both succeed silently.
	PodName = "agentmoat-nvproxy-probe"

	defaultPollInterval = 2 * time.Second
	logReadLimit        = 64 << 10
)

// LogFetcher reads a pod's log. The default (podLogs) streams it through
// client-go; tests substitute a canned string because the fake clientset
// cannot serve logs.
type LogFetcher func(ctx context.Context, client kubernetes.Interface, namespace, name string) (string, error)

// Options configures one Run.
type Options struct {
	// Client is the Kubernetes API client. Required.
	Client kubernetes.Interface

	// RuntimeClassName selects the node pool: the pod is pinned to a Ready
	// node matching the RuntimeClass nodeSelector, preferring one with
	// GPUs. Empty means schema.DefaultRuntimeClassName.
	RuntimeClassName string

	// Namespace, Image, RunscPath, and Timeout default to the constants
	// above when zero.
	Namespace string
	Image     string
	RunscPath string
	Timeout   time.Duration

	// DryRun, when true, creates nothing and reports the pod that would
	// have been created. The CLI and the MCP tool default it to true.
	DryRun bool

	// Cluster and AgentmoatVersion are recorded in the report metadata.
	Cluster          string
	AgentmoatVersion string

	// Logs and PollInterval are testing seams; nil / zero select the
	// production behavior.
	Logs         LogFetcher
	PollInterval time.Duration
}

// Run performs the preflight, then, unless the preflight is blocking or
// DryRun is set, creates the probe pod, waits for it, parses its output,
// and deletes it. A probe that could not run to completion (image pull
// failure, timeout, unparseable output) is reported as an
// nvproxy-probe-failed warning, not as an error: the report still carries
// the preflight facts. Errors are reserved for the API refusing the
// request (RBAC, network, a leftover pod with the same name).
func Run(ctx context.Context, opts Options) (*schema.PreflightReport, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("probe: opts.Client is nil")
	}
	opts = withDefaults(opts)

	facts, err := preflight.Collect(ctx, opts.Client, opts.RuntimeClassName)
	if err != nil {
		return nil, err
	}
	findings := preflight.Evaluate(facts)
	meta := &schema.ProbeMetadata{
		DryRun: opts.DryRun, Namespace: opts.Namespace, PodName: PodName,
		Image: opts.Image, RunscPath: opts.RunscPath,
	}

	switch {
	case preflight.IsBlocking(findings):
		findings = append(findings, skippedFinding(facts))
	default:
		node, err := pickNode(ctx, opts.Client, facts)
		if err != nil {
			return nil, err
		}
		meta.Node = node
		if opts.DryRun {
			findings = append(findings, dryRunFinding(meta))
			break
		}
		nv, failure, err := execute(ctx, opts, node)
		if err != nil {
			return nil, err
		}
		if failure != "" {
			findings = append(findings, failedFinding(meta, failure))
			break
		}
		if facts.GPU == nil {
			facts.GPU = &schema.GPUFacts{}
		}
		facts.GPU.Nvproxy = nv
		preflight.RefreshGPUSupport(facts)
		findings = preflight.Evaluate(facts)
		meta.Succeeded = true
	}
	preflight.SortFindings(findings)

	report := schema.NewPreflightReport()
	report.Metadata = schema.PreflightMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          opts.Cluster,
		AgentmoatVersion: opts.AgentmoatVersion,
		RuntimeClassName: opts.RuntimeClassName,
		Probe:            meta,
	}
	report.Spec = schema.PreflightSpec{
		Summary:  preflight.Summarize(findings),
		Facts:    *facts,
		Findings: findings,
	}
	return report, nil
}

func withDefaults(o Options) Options {
	if o.RuntimeClassName == "" {
		o.RuntimeClassName = schema.DefaultRuntimeClassName
	}
	if o.Namespace == "" {
		o.Namespace = DefaultNamespace
	}
	if o.Image == "" {
		o.Image = DefaultImage
	}
	if o.RunscPath == "" {
		o.RunscPath = DefaultRunscPath
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Logs == nil {
		o.Logs = podLogs
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	return o
}

// pickNode lists the nodes and returns the name of the Ready node matching
// the RuntimeClass nodeSelector the pod should run on. GPU nodes come
// first (their runsc is the one that matters), then name order, so the
// choice is stable across runs.
func pickNode(ctx context.Context, client kubernetes.Interface, facts *schema.ClusterFacts) (string, error) {
	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("probe: listing nodes: %w", err)
	}
	var candidates []*corev1.Node
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if preflight.NodeIsReady(n) && preflight.MatchesRuntimeClass(facts, n) {
			candidates = append(candidates, n)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("probe: no Ready node matches RuntimeClass %q nodeSelector", facts.RuntimeClass.Name)
	}
	sort.Slice(candidates, func(i, j int) bool {
		gi, gj := preflight.HasGPU(candidates[i]), preflight.HasGPU(candidates[j])
		if gi != gj {
			return gi
		}
		return candidates[i].Name < candidates[j].Name
	})
	return candidates[0].Name, nil
}

// execute creates the pod, waits, reads, parses, and deletes. The three
// return values separate "the probe ran and here is the answer" (nv), "the
// probe ran and did not produce an answer" (failure, a warning in the
// report), and "the API would not let the probe run" (err).
func execute(ctx context.Context, opts Options, node string) (nv *schema.NvproxyFacts, failure string, err error) {
	pods := opts.Client.CoreV1().Pods(opts.Namespace)
	if _, err := pods.Create(ctx, buildPod(opts, node), metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil, "", fmt.Errorf("probe: pod %s/%s already exists (left over from an earlier run); delete it and re-run: %w",
				opts.Namespace, PodName, err)
		}
		return nil, "", fmt.Errorf("probe: creating pod %s/%s: %w", opts.Namespace, PodName, err)
	}
	// Delete with a fresh context: the caller's ctx may already be
	// canceled (that is one way to get here), and a leftover pod would
	// make the next run fail with AlreadyExists.
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = pods.Delete(dctx, PodName, metav1.DeleteOptions{})
	}()

	last, waitErr := waitForCompletion(ctx, opts)
	if waitErr != nil {
		return nil, describeWaitFailure(opts, last, waitErr), nil
	}
	logs, err := opts.Logs(ctx, opts.Client, opts.Namespace, PodName)
	if err != nil {
		return nil, fmt.Sprintf("reading the log of pod %s/%s: %v", opts.Namespace, PodName, err), nil
	}
	if last.Status.Phase == corev1.PodFailed {
		return nil, fmt.Sprintf("pod %s/%s on node %s failed (%s); last output: %s",
			opts.Namespace, PodName, node, terminationReason(last), tail(logs)), nil
	}
	version, drivers, err := parseOutput(logs)
	if err != nil {
		return nil, fmt.Sprintf("unexpected output from pod %s/%s on node %s: %v; last output: %s",
			opts.Namespace, PodName, node, err, tail(logs)), nil
	}
	return &schema.NvproxyFacts{
		RunscVersion:     version,
		SupportedDrivers: drivers,
		Node:             node,
		ProbedAt:         time.Now().UTC().Format(time.RFC3339),
	}, "", nil
}

// errTimeout marks a wait that ran out of Options.Timeout.
var errTimeout = fmt.Errorf("timed out")

// waitForCompletion polls the pod until it reaches Succeeded or Failed.
// Returns the last pod object seen (nil when the first Get failed) so the
// caller can say what the pod was doing when the wait ended.
func waitForCompletion(ctx context.Context, opts Options) (*corev1.Pod, error) {
	deadline := time.Now().Add(opts.Timeout)
	var last *corev1.Pod
	for {
		pod, err := opts.Client.CoreV1().Pods(opts.Namespace).Get(ctx, PodName, metav1.GetOptions{})
		if err != nil {
			return last, err
		}
		last = pod
		switch pod.Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			return pod, nil
		}
		if time.Now().After(deadline) {
			return pod, errTimeout
		}
		select {
		case <-ctx.Done():
			return pod, ctx.Err()
		case <-time.After(opts.PollInterval):
		}
	}
}

// describeWaitFailure explains a wait that ended without a terminal phase,
// quoting the container's waiting reason (ImagePullBackOff, CreateContainer
// ConfigError, ...) because that is what the operator has to fix.
func describeWaitFailure(opts Options, pod *corev1.Pod, err error) string {
	where := fmt.Sprintf("pod %s/%s", opts.Namespace, PodName)
	if err == errTimeout {
		state := "phase unknown"
		if pod != nil {
			state = "phase " + string(pod.Status.Phase)
			if reason := waitingReason(pod); reason != "" {
				state += ", container " + reason
			}
		}
		return fmt.Sprintf("timed out after %s waiting for %s to complete (%s)", opts.Timeout, where, state)
	}
	return fmt.Sprintf("waiting for %s: %v", where, err)
}

// waitingReason returns the first container's Waiting reason, if any.
func waitingReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting != nil && cs.State.Waiting.Reason != "" {
			return cs.State.Waiting.Reason
		}
	}
	return ""
}

// terminationReason returns "exit N, <reason>" for a failed pod.
func terminationReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if t := cs.State.Terminated; t != nil {
			s := fmt.Sprintf("exit %d", t.ExitCode)
			if t.Reason != "" {
				s += ", " + t.Reason
			}
			return s
		}
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	return "no container status"
}

// tail returns the last few non-empty log lines on one line, for messages.
func tail(logs string) string {
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	out := strings.TrimSpace(strings.Join(lines, " | "))
	if out == "" {
		return "(empty)"
	}
	return out
}

// podLogs is the production LogFetcher.
func podLogs(ctx context.Context, client kubernetes.Interface, namespace, name string) (string, error) {
	rc, err := client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, logReadLimit))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// skippedFinding: the preflight is blocking, so there is no node to run on.
func skippedFinding(facts *schema.ClusterFacts) schema.PreflightFinding {
	return schema.PreflightFinding{
		ID: preflight.FindingNvproxyProbeSkipped, Severity: schema.SeverityWarn,
		Message: fmt.Sprintf("nvproxy probe not run: the preflight reports an error, so no Ready node can host the probe pod for RuntimeClass %q",
			facts.RuntimeClass.Name),
		Remediation: "fix the error findings above, then re-run 'agentmoat probe nvproxy'",
	}
}

// dryRunFinding describes the pod that would have been created.
func dryRunFinding(m *schema.ProbeMetadata) schema.PreflightFinding {
	return schema.PreflightFinding{
		ID: preflight.FindingNvproxyProbeDryRun, Severity: schema.SeverityInfo,
		Message: fmt.Sprintf("dry run: would create pod %s/%s (image %s) on node %s to run %s --version and %s nvproxy list-supported-drivers, then delete it",
			m.Namespace, m.PodName, m.Image, m.Node, m.RunscPath, m.RunscPath),
		Remediation: "re-run with --dry-run=false to create the pod; the driver verdicts stay unknown until then",
	}
}

// failedFinding: the pod ran (or tried to) but produced no answer.
func failedFinding(m *schema.ProbeMetadata, failure string) schema.PreflightFinding {
	return schema.PreflightFinding{
		ID: preflight.FindingNvproxyProbeFailed, Severity: schema.SeverityWarn,
		Message:     "nvproxy probe failed: " + failure,
		Remediation: fmt.Sprintf("check that node %s has runsc at %s and can pull %s; the pod was deleted, so re-run after fixing", m.Node, m.RunscPath, m.Image),
	}
}
