// Package seccompprobe runs `agentmoat probe seccomp`: a trial copy of each
// migrated workload in a gVisor sandbox, with the workload's own seccomp
// profile, and a report of the syscalls that profile refused.
//
// Why. From gVisor release 20261123.1, runsc loads the container's OCI
// seccomp filter inside the sandbox by default (runsc --oci-seccomp
// defaults to true; source: google/gvisor runsc/config/flags.go at tag
// release-20261123.1, false at release-20261123.0). Before that release a
// RuntimeDefault or Localhost profile under runsc filtered nothing: runsc
// logged "Seccomp spec is being ignored because oci-seccomp is disabled".
// So a workload that was migrated to gVisor and ran fine can start getting
// EPERM from calls its profile refuses (containerd's default profile
// refuses unshare, mount, keyctl, bpf, get_mempolicy and others) the day its
// nodes move to 20261123.1. The classifier cannot see that from a pod spec;
// only running the workload under the filter can.
//
// How. gVisor exports no metric for seccomp denials. The Sentry logs each
// one ("Syscall <n>: denied by seccomp") at debug level only. So the probe
// needs a runtime handler that runs runsc with the debug log on: the
// gvisor-seccomp-audit handler (kind/runsc-seccomp-audit.toml,
// packer/files/runsc-seccomp-audit.toml) and its RuntimeClass
// (deploy/runtimeclass-seccomp-audit.yaml). That handler also forces
// oci-seccomp on, so a node still on an older runsc applies the filter
// too, and logs only the boot (Sentry) process to
// /var/log/runsc/seccomp-audit/<sandbox id>.boot.txt. Run:
//
//  1. For each target, one trial pod: the workload's pod template with the
//     audit RuntimeClass and the changes buildTrial lists (no workload
//     labels, restartPolicy Never, claims replaced by emptyDir), in the
//     workload's own namespace. The seccomp profiles and capabilities are
//     not touched.
//  2. Wait until each trial has started (Options.Timeout), let them run
//     (Options.RunFor), then delete them.
//  3. On each node that ran a trial, one reader pod under runc with the log
//     directory mounted read-only (hostPath, type Directory). It prints
//     counts per sandbox for this run only, never log lines.
//  4. Parse, map syscall numbers to names for the sandbox's architecture,
//     and store the result in ClusterFacts.Seccomp; preflight.Evaluate
//     turns it into findings (pkg/preflight/seccomp.go).
//
// What it is not. A trial shows what the workload did during its window,
// with no traffic and empty volumes: a call made only under load, at
// shutdown, or on a code path the trial did not reach is not in the
// report. It runs the workload's real code with the workload's service
// account, so whatever the workload does on start (a migration, a message,
// a call out) the trial does too. That is why the probe follows the apply
// convention: dry-run by default, and a dry run reports every pod it
// would create and every change it would make.
//
// RBAC: create/get/delete on pods in each target namespace and in the
// reader namespace, get on pods/log in the reader namespace, get on
// node.k8s.io/runtimeclasses, get on the target workloads, plus the
// preflight's reads. The reader pod has a hostPath mount, which Pod
// Security Admission "baseline" forbids: use a reader namespace that
// allows privileged pods.
package seccompprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/classifier"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	"github.com/0hardik1/agentmoat/pkg/scanner"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Defaults, exported so the CLI flags and the MCP tool quote them.
const (
	DefaultAuditRuntimeClass = "gvisor-seccomp-audit"
	DefaultLogDir            = "/var/log/runsc/seccomp-audit"
	DefaultReaderNamespace   = "default"
	DefaultReaderImage       = "busybox:1.36.1"
	DefaultRunFor            = time.Minute
	DefaultTimeout           = 3 * time.Minute

	defaultPollInterval = 2 * time.Second
	deleteTimeout       = 60 * time.Second
	logReadLimit        = 256 << 10
)

// LogFetcher reads a pod's log; tests substitute canned output.
type LogFetcher func(ctx context.Context, client kubernetes.Interface, namespace, name string) (string, error)

// Options configures one Run.
type Options struct {
	// Client is the Kubernetes API client. Required.
	Client kubernetes.Interface

	// RuntimeClassName is the RuntimeClass the workloads migrate to; the
	// preflight facts describe it, and with no Targets the probe runs the
	// workloads that already request it. Empty means "gvisor".
	RuntimeClassName string

	// AuditRuntimeClassName is the RuntimeClass the trials request.
	AuditRuntimeClassName string

	// Targets are the workloads to run, normally the steps of a plan.
	// Empty means every workload in Namespaces whose pod template
	// requests RuntimeClassName (the ones already migrated).
	Targets    []schema.WorkloadRef
	Namespaces []string

	RunFor          time.Duration
	Timeout         time.Duration
	LogDir          string
	ReaderNamespace string
	ReaderImage     string

	// DryRun creates nothing. The CLI and the MCP tool default it to true.
	DryRun bool

	Cluster          string
	AgentmoatVersion string

	// Testing seams.
	Logs         LogFetcher
	PollInterval time.Duration
	RunID        string
}

// trial is one target as it goes through the run.
type trial struct {
	entry   schema.SeccompWorkload
	pod     *corev1.Pod
	created bool
	started bool
}

// Run performs the preflight, resolves the targets and, unless DryRun is
// set or nothing can run, runs the trials and reads their logs. Problems
// with one workload (it is gone, its trial would not start, admission
// refused it) become that workload's status and a finding; errors are
// reserved for reads the whole run depends on.
func Run(ctx context.Context, opts Options) (*schema.PreflightReport, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("seccompprobe: opts.Client is nil")
	}
	opts = withDefaults(opts)

	facts, err := preflight.Collect(ctx, opts.Client, opts.RuntimeClassName)
	if err != nil {
		return nil, err
	}
	trials, err := resolveTargets(ctx, opts)
	if err != nil {
		return nil, err
	}
	meta := &schema.SeccompProbeMetadata{
		DryRun: opts.DryRun, RuntimeClassName: opts.AuditRuntimeClassName, RunFor: opts.RunFor.String(),
		LogDir: opts.LogDir, ReaderNamespace: opts.ReaderNamespace, ReaderImage: opts.ReaderImage, RunID: opts.RunID,
	}

	var extra []schema.PreflightFinding
	switch skip := skipReason(ctx, opts, facts); {
	case len(trials) == 0:
		extra = append(extra, schema.PreflightFinding{
			ID: preflight.FindingSeccompProbeSkipped, Severity: schema.SeverityWarn,
			Message:     noTargetsMessage(opts),
			Remediation: "pass --plan with a plan that has steps, or --namespace where migrated workloads run",
		})
	case skip != "":
		for _, t := range trials {
			if t.entry.Status == "" {
				t.entry.Status, t.entry.Reason = schema.SeccompTrialSkipped, skip
			}
		}
	case opts.DryRun:
		for _, t := range trials {
			if t.entry.Status == "" {
				t.entry.Status = schema.SeccompTrialDryRun
			}
		}
		extra = append(extra, dryRunFinding(opts, trials))
	default:
		execute(ctx, opts, trials)
	}

	sf := &schema.SeccompFacts{ProbedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, t := range trials {
		sf.Workloads = append(sf.Workloads, t.entry)
	}
	facts.Seccomp = sf
	findings := append(preflight.Evaluate(facts), extra...)
	preflight.SortFindings(findings)

	report := schema.NewPreflightReport()
	report.Metadata = schema.PreflightMetadata{
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		Cluster:          opts.Cluster,
		AgentmoatVersion: opts.AgentmoatVersion,
		RuntimeClassName: opts.RuntimeClassName,
		SeccompProbe:     meta,
	}
	report.Spec = schema.PreflightSpec{Summary: preflight.Summarize(findings), Facts: *facts, Findings: findings}
	return report, nil
}

func withDefaults(o Options) Options {
	if o.RuntimeClassName == "" {
		o.RuntimeClassName = schema.DefaultRuntimeClassName
	}
	if o.AuditRuntimeClassName == "" {
		o.AuditRuntimeClassName = DefaultAuditRuntimeClass
	}
	if o.RunFor <= 0 {
		o.RunFor = DefaultRunFor
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.LogDir == "" {
		o.LogDir = DefaultLogDir
	}
	if o.ReaderNamespace == "" {
		o.ReaderNamespace = DefaultReaderNamespace
	}
	if o.ReaderImage == "" {
		o.ReaderImage = DefaultReaderImage
	}
	if o.Logs == nil {
		o.Logs = podLogs
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.RunID == "" {
		o.RunID = newRunID()
	}
	return o
}

// newRunID returns 6 lowercase hex characters.
func newRunID() string {
	b := make([]byte, 3)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%06x", time.Now().UnixNano()&0xffffff)
	}
	return hex.EncodeToString(b)
}

// resolveTargets reads each target workload, or finds the migrated ones,
// and prepares its trial. The result is sorted by kind, namespace, name.
func resolveTargets(ctx context.Context, o Options) ([]*trial, error) {
	var workloads []scanner.Workload
	var out []*trial
	if len(o.Targets) == 0 {
		all, err := scanner.Enumerate(ctx, o.Client, scanner.EnumerateOptions{Namespaces: o.Namespaces})
		if err != nil {
			return nil, fmt.Errorf("seccompprobe: listing workloads: %w", err)
		}
		for _, w := range all {
			if w.PodSpec.RuntimeClassName != nil && *w.PodSpec.RuntimeClassName == o.RuntimeClassName {
				workloads = append(workloads, w)
			}
		}
	} else {
		for _, ref := range o.Targets {
			w, err := scanner.GetByRef(ctx, o.Client, ref.Kind, ref.Namespace, ref.Name)
			if err != nil {
				out = append(out, &trial{entry: schema.SeccompWorkload{
					Target: ref, Status: schema.SeccompTrialSkipped, Reason: "could not read the workload: " + err.Error(),
				}})
				continue
			}
			workloads = append(workloads, w)
		}
	}
	for _, w := range workloads {
		ref := schema.WorkloadRef{Kind: w.Kind, Namespace: w.Namespace, Name: w.Name}
		pod, changes := buildTrial(w.PodSpec, ref, o, o.RunID)
		out = append(out, &trial{pod: pod, entry: schema.SeccompWorkload{
			Target: ref, Profiles: classifier.SeccompProfiles(w.PodSpec), TrialPod: pod.Namespace + "/" + pod.Name, Adjustments: changes,
		}})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].entry.Target, out[j].entry.Target
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	return out, nil
}

// skipReason returns why no trial can run, or "".
func skipReason(ctx context.Context, o Options, facts *schema.ClusterFacts) string {
	if preflight.IsBlocking(preflight.Evaluate(facts)) {
		return fmt.Sprintf("the preflight for RuntimeClass %q reports an error, so no gVisor node can run a trial; fix it first ('agentmoat preflight')", o.RuntimeClassName)
	}
	_, err := o.Client.NodeV1().RuntimeClasses().Get(ctx, o.AuditRuntimeClassName, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		return fmt.Sprintf("RuntimeClass %q does not exist; apply deploy/runtimeclass-seccomp-audit.yaml, and give the gVisor nodes its handler (kind/cluster.yaml and packer/files/gvisor-runtime.toml show it)", o.AuditRuntimeClassName)
	case err != nil:
		return fmt.Sprintf("could not read RuntimeClass %q: %v", o.AuditRuntimeClassName, err)
	}
	return ""
}

func noTargetsMessage(o Options) string {
	if len(o.Targets) > 0 {
		return "seccomp probe not run: none of the targets could be read"
	}
	where := "any namespace"
	if len(o.Namespaces) > 0 {
		where = "namespace(s) " + strings.Join(o.Namespaces, ", ")
	}
	return fmt.Sprintf("seccomp probe not run: no workload in %s requests RuntimeClass %q, and no plan was given", where, o.RuntimeClassName)
}

// execute creates the trials, lets them run, deletes them, and reads the
// logs on each node that ran one.
func execute(ctx context.Context, o Options, trials []*trial) {
	defer deleteTrials(o, trials)
	for _, t := range trials {
		if t.entry.Status != "" {
			continue
		}
		if _, err := o.Client.CoreV1().Pods(t.pod.Namespace).Create(ctx, t.pod, metav1.CreateOptions{}); err != nil {
			t.entry.Status, t.entry.Reason = schema.SeccompTrialFailed, "creating trial pod "+t.entry.TrialPod+": "+err.Error()
			continue
		}
		t.created = true
	}
	waitStarted(ctx, o, trials)
	if anyStarted(trials) {
		select {
		case <-ctx.Done():
		case <-time.After(o.RunFor):
		}
	}
	noteExits(ctx, o, trials)
	deleteTrials(o, trials)
	readLogs(ctx, o, trials)
}

// waitStarted polls the created trials until each has started (or ended),
// or until Options.Timeout. A trial that did not start is failed with the
// reason the pod shows.
func waitStarted(ctx context.Context, o Options, trials []*trial) {
	deadline := time.Now().Add(o.Timeout)
	for {
		pending := 0
		for _, t := range trials {
			if !t.created || t.started || t.entry.Status != "" {
				continue
			}
			pod, err := o.Client.CoreV1().Pods(t.pod.Namespace).Get(ctx, t.pod.Name, metav1.GetOptions{})
			if err != nil {
				t.entry.Status, t.entry.Reason = schema.SeccompTrialFailed, "reading trial pod: "+err.Error()
				continue
			}
			t.entry.Node = pod.Spec.NodeName
			if hasStarted(pod) {
				t.started = true
				continue
			}
			if time.Now().After(deadline) || ctx.Err() != nil {
				t.entry.Status, t.entry.Reason = schema.SeccompTrialFailed,
					fmt.Sprintf("trial pod %s did not start within %s (%s)", t.entry.TrialPod, o.Timeout, describePending(pod))
				continue
			}
			pending++
		}
		if pending == 0 {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(o.PollInterval):
		}
	}
}

// hasStarted: the pod ran at least one container, init or main.
func hasStarted(p *corev1.Pod) bool {
	switch p.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	for _, cs := range p.Status.InitContainerStatuses {
		if cs.State.Running != nil || cs.State.Terminated != nil {
			return true
		}
	}
	return false
}

func anyStarted(trials []*trial) bool {
	for _, t := range trials {
		if t.started {
			return true
		}
	}
	return false
}

// describePending says what a pod that has not started is waiting for.
func describePending(p *corev1.Pod) string {
	parts := []string{"phase " + orDefault(string(p.Status.Phase), "unknown")}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Message != "" {
			parts = append(parts, "not scheduled: "+c.Message)
		}
	}
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if w := cs.State.Waiting; w != nil && w.Reason != "" {
			parts = append(parts, "container "+cs.Name+" "+w.Reason)
		}
	}
	return strings.Join(parts, ", ")
}

// noteExits records, for a trial that ran, containers that exited non-zero
// during the window: a denied syscall is a common cause, and the reader
// cannot tell which container made the call.
func noteExits(ctx context.Context, o Options, trials []*trial) {
	for _, t := range trials {
		if !t.started {
			continue
		}
		pod, err := o.Client.CoreV1().Pods(t.pod.Namespace).Get(ctx, t.pod.Name, metav1.GetOptions{})
		if err != nil {
			continue
		}
		var exits []string
		for _, cs := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			if term := cs.State.Terminated; term != nil && term.ExitCode != 0 {
				exits = append(exits, fmt.Sprintf("container %s exited %d", cs.Name, term.ExitCode))
			}
		}
		if len(exits) > 0 {
			t.entry.Reason = "during the trial: " + strings.Join(exits, ", ")
		}
	}
}

// deleteTrials deletes every created trial and waits for them to go, so
// their sandboxes have stopped and flushed the Sentry log. It uses a fresh
// context: the caller's may be canceled, and a trial must not outlive the
// probe. Safe to call twice.
func deleteTrials(o Options, trials []*trial) {
	dctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
	defer cancel()
	for _, t := range trials {
		if t.created {
			_ = o.Client.CoreV1().Pods(t.pod.Namespace).Delete(dctx, t.pod.Name, metav1.DeleteOptions{})
		}
	}
	for _, t := range trials {
		if !t.created {
			continue
		}
		for {
			_, err := o.Client.CoreV1().Pods(t.pod.Namespace).Get(dctx, t.pod.Name, metav1.GetOptions{})
			if err != nil || dctx.Err() != nil {
				break
			}
			select {
			case <-dctx.Done():
			case <-time.After(o.PollInterval):
			}
		}
		t.created = false
	}
}

// readLogs runs one reader per node, in parallel, and folds each trial's
// logs into its entry.
func readLogs(ctx context.Context, o Options, trials []*trial) {
	byNode := map[string][]*trial{}
	for _, t := range trials {
		if t.started && t.entry.Status == "" {
			byNode[t.entry.Node] = append(byNode[t.entry.Node], t)
		}
	}
	var wg sync.WaitGroup
	for node, ts := range byNode {
		wg.Add(1)
		go func(node string, ts []*trial) {
			defer wg.Done()
			logs, failure := readNode(ctx, o, node)
			for _, t := range ts {
				if failure != "" {
					t.entry.Status, t.entry.Reason = schema.SeccompTrialFailed, failure
					continue
				}
				var mine []sandboxLog
				for _, l := range logs {
					if l.Pod == t.pod.Name && l.Namespace == t.pod.Namespace {
						mine = append(mine, l)
					}
				}
				if len(mine) == 0 {
					t.entry.Status, t.entry.Reason = schema.SeccompTrialFailed, fmt.Sprintf(
						"no Sentry boot log for trial pod %s in %s on node %s; check that the %s handler on that node is configured with runsc-seccomp-audit.toml (debug-log under %s)",
						t.entry.TrialPod, o.LogDir, node, o.AuditRuntimeClassName, o.LogDir)
					continue
				}
				merge(&t.entry, mine)
				t.entry.Status = schema.SeccompTrialRan
			}
		}(node, ts)
	}
	wg.Wait()
}

// readNode runs the reader pod on one node and returns the parsed logs, or
// a failure reason.
func readNode(ctx context.Context, o Options, node string) ([]sandboxLog, string) {
	pod := buildReader(o, node, o.RunID)
	pods := o.Client.CoreV1().Pods(o.ReaderNamespace)
	where := o.ReaderNamespace + "/" + pod.Name
	if _, err := pods.Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Sprintf("creating reader pod %s on node %s: %v", where, node, err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
		defer cancel()
		_ = pods.Delete(dctx, pod.Name, metav1.DeleteOptions{})
	}()
	deadline := time.Now().Add(o.Timeout)
	for {
		p, err := pods.Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Sprintf("reading reader pod %s: %v", where, err)
		}
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Sprintf("reader pod %s on node %s did not finish within %s (%s); a node without %s keeps it in ContainerCreating",
				where, node, o.Timeout, describePending(p), o.LogDir)
		}
		select {
		case <-ctx.Done():
		case <-time.After(o.PollInterval):
		}
	}
	out, err := o.Logs(ctx, o.Client, o.ReaderNamespace, pod.Name)
	if err != nil {
		return nil, fmt.Sprintf("reading the log of reader pod %s: %v", where, err)
	}
	logs, err := parseReaderOutput(out)
	if err != nil {
		return nil, fmt.Sprintf("unexpected output from reader pod %s on node %s: %v", where, node, err)
	}
	return logs, ""
}

func podLogs(ctx context.Context, client kubernetes.Interface, namespace, name string) (string, error) {
	rc, err := client.CoreV1().Pods(namespace).GetLogs(name, &corev1.PodLogOptions{}).Stream(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, logReadLimit))
	return string(b), err
}

// dryRunFinding lists what a real run would create and change.
func dryRunFinding(o Options, trials []*trial) schema.PreflightFinding {
	var lines []string
	for _, t := range trials {
		if t.entry.Status != schema.SeccompTrialDryRun {
			continue
		}
		s := fmt.Sprintf("%s %s/%s as pod %s", t.entry.Target.Kind, t.entry.Target.Namespace, t.entry.Target.Name, t.entry.TrialPod)
		if len(t.entry.Adjustments) > 0 {
			s += " (" + strings.Join(t.entry.Adjustments, "; ") + ")"
		}
		lines = append(lines, s)
	}
	return schema.PreflightFinding{
		ID: preflight.FindingSeccompProbeDryRun, Severity: schema.SeverityInfo,
		Message: fmt.Sprintf("dry run: would run %d trial(s) under RuntimeClass %s for %s each, then delete them and read %s on each node that ran one with a reader pod (image %s) in namespace %s: %s. Each trial runs the workload's real containers with its service account and its seccomp profiles unchanged, but without its labels, so it gets no Service traffic",
			len(lines), o.AuditRuntimeClassName, o.RunFor, o.LogDir, o.ReaderImage, o.ReaderNamespace, strings.Join(lines, "; ")),
		Remediation: "re-run with --dry-run=false to run the trials",
	}
}
