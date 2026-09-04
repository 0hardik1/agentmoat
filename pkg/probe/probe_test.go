// Tests for the nvproxy probe. The fake clientset stores whatever a create
// reactor puts in the pod status, which is how each test scripts the pod's
// fate (Succeeded, Failed, stuck Pending). Logs come from the LogFetcher
// seam because the fake cannot serve pods/log.
package probe

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const sampleOutput = `runsc version release-20260817.0
spec: 1.2.1
---
550.54.15
535.129.03
535.183.06
`

func gvisorRC() *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "gvisor"}, Handler: "gvisor",
		Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"runtime": "gvisor"}},
	}
}

func readyNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

// gpuGvisorNode is a gVisor node with one T4 whose driver the sample
// output lists.
func gpuGvisorNode(name string) *corev1.Node {
	n := readyNode(name, map[string]string{
		"runtime": "gvisor", schema.GFDProductLabel: "Tesla-T4", schema.GFDDriverVersionLabel: "535.183.06",
	})
	n.Status.Capacity = corev1.ResourceList{corev1.ResourceName(schema.NvidiaGPUResource): resource.MustParse("1")}
	return n
}

// cluster is the standard fixture: a CPU gVisor node, a GPU gVisor node,
// and a runc node.
func cluster() *fake.Clientset {
	return fake.NewSimpleClientset(gvisorRC(),
		readyNode("gv-a", map[string]string{"runtime": "gvisor"}),
		gpuGvisorNode("gv-b"),
		readyNode("cpu-1", nil),
	)
}

// scriptPodStatus makes every created pod carry the given status, which is
// how a test decides the probe's fate without a kubelet.
func scriptPodStatus(c *fake.Clientset, status corev1.PodStatus) {
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status = status
		return false, nil, nil
	})
}

func cannedLogs(s string) LogFetcher {
	return func(context.Context, kubernetes.Interface, string, string) (string, error) { return s, nil }
}

func fastOptions(c *fake.Clientset, logs string) Options {
	return Options{Client: c, DryRun: false, Logs: cannedLogs(logs), PollInterval: time.Millisecond, Timeout: 2 * time.Second}
}

func verbs(c *fake.Clientset, verb string) int {
	n := 0
	for _, a := range c.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == "pods" {
			n++
		}
	}
	return n
}

func findingIDs(r *schema.PreflightReport) []string {
	ids := []string{}
	for _, f := range r.Spec.Findings {
		ids = append(ids, f.ID)
	}
	return ids
}

func findingByID(t *testing.T, r *schema.PreflightReport, id string) schema.PreflightFinding {
	t.Helper()
	for _, f := range r.Spec.Findings {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("finding %s not in %v", id, findingIDs(r))
	return schema.PreflightFinding{}
}

func assertPodGone(t *testing.T, c *fake.Clientset) {
	t.Helper()
	_, err := c.CoreV1().Pods(DefaultNamespace).Get(context.Background(), PodName, metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("probe pod should have been deleted, Get err = %v", err)
	}
}

func TestParseOutput(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantVersion string
		wantDrivers []string
		wantErr     string
	}{
		{name: "real output, sorted numerically", in: sampleOutput,
			wantVersion: "release-20260817.0", wantDrivers: []string{"535.129.03", "535.183.06", "550.54.15"}},
		{name: "extra lines and warnings ignored",
			in:          "runsc version release-20270101.0\nspec: 1.3.0\nbuild: abc\n---\nW0904 something\n570.86.15\n\n",
			wantVersion: "release-20270101.0", wantDrivers: []string{"570.86.15"}},
		{name: "no version line", in: "sh: /host-runsc: not found\n", wantErr: `no "runsc version" line`},
		{name: "no separator (old runsc)", in: "runsc version release-20230101.0\nspec: 1.0\n", wantErr: "separator"},
		{name: "no drivers", in: "runsc version x\n---\n", wantErr: "no driver versions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, d, err := parseOutput(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if v != tc.wantVersion || !reflect.DeepEqual(d, tc.wantDrivers) {
				t.Fatalf("got %q %v, want %q %v", v, d, tc.wantVersion, tc.wantDrivers)
			}
		})
	}
}

// boolPtrIs reports whether a *bool is set and equals want.
func boolPtrIs(p *bool, want bool) bool { return p != nil && *p == want }

// invariant is one named yes/no check over the probe pod manifest.
type invariant struct {
	name string
	ok   bool
}

func assertInvariants(t *testing.T, checks []invariant) {
	t.Helper()
	for _, ck := range checks {
		if !ck.ok {
			t.Errorf("probe pod invariant violated: %s", ck.name)
		}
	}
}

func TestBuildPod_PlacementAndCommand(t *testing.T) {
	pod := buildPod(withDefaults(Options{Client: fake.NewSimpleClientset()}), "gv-b")
	c := pod.Spec.Containers[0]
	hp := pod.Spec.Volumes[0].HostPath
	if hp == nil {
		t.Fatal("no hostPath volume")
	}
	assertInvariants(t, []invariant{
		{"no runtimeClassName: the pod must run under runc to read the host runsc", pod.Spec.RuntimeClassName == nil},
		{"pinned to the node", pod.Spec.NodeName == "gv-b"},
		{"restartPolicy Never", pod.Spec.RestartPolicy == corev1.RestartPolicyNever},
		{"single Exists toleration", len(pod.Spec.Tolerations) == 1 && pod.Spec.Tolerations[0].Operator == corev1.TolerationOpExists},
		{"hostPath is the runsc binary", hp.Path == DefaultRunscPath},
		{"hostPath type File", hp.Type != nil && *hp.Type == corev1.HostPathFile},
		{"read-only mount at " + runscMountPath, c.VolumeMounts[0].ReadOnly && c.VolumeMounts[0].MountPath == runscMountPath},
		{"command runs --version", strings.Contains(c.Command[2], "--version")},
		{"command lists the drivers", strings.Contains(c.Command[2], "nvproxy list-supported-drivers")},
		{"activeDeadlineSeconds set", pod.Spec.ActiveDeadlineSeconds != nil && *pod.Spec.ActiveDeadlineSeconds > 0},
	})
}

func TestBuildPod_SecurityContext(t *testing.T) {
	pod := buildPod(withDefaults(Options{Client: fake.NewSimpleClientset()}), "gv-b")
	csc, psc := pod.Spec.Containers[0].SecurityContext, pod.Spec.SecurityContext
	if csc == nil || psc == nil {
		t.Fatalf("security contexts missing: container=%v pod=%v", csc != nil, psc != nil)
	}
	assertInvariants(t, []invariant{
		{"read-only root filesystem", boolPtrIs(csc.ReadOnlyRootFilesystem, true)},
		{"no privilege escalation", boolPtrIs(csc.AllowPrivilegeEscalation, false)},
		{"all capabilities dropped", csc.Capabilities != nil && len(csc.Capabilities.Drop) == 1 && csc.Capabilities.Drop[0] == "ALL"},
		{"runAsNonRoot", boolPtrIs(psc.RunAsNonRoot, true)},
		{"seccomp RuntimeDefault", psc.SeccompProfile != nil && psc.SeccompProfile.Type == corev1.SeccompProfileTypeRuntimeDefault},
		{"no service account token", boolPtrIs(pod.Spec.AutomountServiceAccountToken, false)},
	})
}

func TestRun_DryRunPrefersGPUNodeAndCreatesNothing(t *testing.T) {
	t.Parallel()
	c := cluster()
	r, err := Run(context.Background(), Options{Client: c, DryRun: true})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := &schema.ProbeMetadata{DryRun: true, Namespace: DefaultNamespace, PodName: PodName, Node: "gv-b",
		Image: DefaultImage, RunscPath: DefaultRunscPath}
	if !reflect.DeepEqual(r.Metadata.Probe, want) {
		t.Fatalf("Probe = %+v, want %+v", r.Metadata.Probe, want)
	}
	if verbs(c, "create") != 0 {
		t.Fatal("dry run created a pod")
	}
	f := findingByID(t, r, preflight.FindingNvproxyProbeDryRun)
	if f.Severity != schema.SeverityInfo || !strings.Contains(f.Message, "default/"+PodName) || !strings.Contains(f.Message, "gv-b") {
		t.Fatalf("dry-run finding = %+v", f)
	}
	if !r.Spec.Summary.Ready {
		t.Fatalf("dry run must not change readiness: %+v", r.Spec.Summary)
	}
	// Card known, driver not: the facts say so.
	if r.Spec.Facts.GPU == nil || r.Spec.Facts.GPU.Nvproxy != nil || r.Spec.Facts.GPU.Groups[0].DriverSupport != schema.SupportUnknown {
		t.Fatalf("GPU facts = %+v", r.Spec.Facts.GPU)
	}
	if r.Kind != schema.KindPreflightReport || r.Metadata.RuntimeClassName != "gvisor" {
		t.Fatalf("envelope = %s / %s", r.Kind, r.Metadata.RuntimeClassName)
	}
}

func TestRun_SucceedsAndSettlesDriverSupport(t *testing.T) {
	t.Parallel()
	c := cluster()
	scriptPodStatus(c, corev1.PodStatus{Phase: corev1.PodSucceeded})
	r, err := Run(context.Background(), fastOptions(c, sampleOutput))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !r.Metadata.Probe.Succeeded || r.Metadata.Probe.DryRun || r.Metadata.Probe.Node != "gv-b" {
		t.Fatalf("Probe = %+v", r.Metadata.Probe)
	}
	assertSampleNvproxyFacts(t, r)
	findingByID(t, r, preflight.FindingGPUNvproxyReady)
	for _, id := range findingIDs(r) {
		if strings.HasPrefix(id, "nvproxy-probe-") {
			t.Fatalf("successful probe must not add a probe finding, got %v", findingIDs(r))
		}
	}
	if verbs(c, "create") != 1 || verbs(c, "delete") != 1 {
		t.Fatalf("create/delete = %d/%d, want 1/1", verbs(c, "create"), verbs(c, "delete"))
	}
	assertPodGone(t, c)
}

// assertSampleNvproxyFacts checks the facts a successful probe on the
// standard fixture must record: sampleOutput parsed and sorted, the T4
// group settled to supported/supported.
func assertSampleNvproxyFacts(t *testing.T, r *schema.PreflightReport) {
	t.Helper()
	nv := r.Spec.Facts.GPU.Nvproxy
	if nv == nil || nv.Node != "gv-b" || nv.ProbedAt == "" {
		t.Fatalf("Nvproxy = %+v", nv)
	}
	want := schema.NvproxyFacts{RunscVersion: "release-20260817.0", SupportedDrivers: []string{"535.129.03", "535.183.06", "550.54.15"}, Node: "gv-b", ProbedAt: nv.ProbedAt}
	if !reflect.DeepEqual(*nv, want) {
		t.Fatalf("Nvproxy = %+v, want %+v", *nv, want)
	}
	if g := r.Spec.Facts.GPU.Groups[0]; g.ProductSupport != schema.SupportSupported || g.DriverSupport != schema.SupportSupported {
		t.Fatalf("group = %+v, want supported/supported", g)
	}
}

func TestRun_PodFailedIsAWarning(t *testing.T) {
	t.Parallel()
	c := cluster()
	scriptPodStatus(c, corev1.PodStatus{Phase: corev1.PodFailed, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "probe", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 127, Reason: "Error"}},
	}}})
	r, err := Run(context.Background(), fastOptions(c, "sh: /host-runsc: not found\n"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	f := findingByID(t, r, preflight.FindingNvproxyProbeFailed)
	for _, want := range []string{"failed (exit 127, Error)", "gv-b", "not found"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message %q missing %q", f.Message, want)
		}
	}
	if f.Severity != schema.SeverityWarn || r.Metadata.Probe.Succeeded || !r.Spec.Summary.Ready {
		t.Fatalf("finding/probe/summary = %s / %+v / %+v", f.Severity, r.Metadata.Probe, r.Spec.Summary)
	}
	if r.Spec.Facts.GPU.Nvproxy != nil {
		t.Fatal("failed probe must not record nvproxy facts")
	}
	assertPodGone(t, c)
}

func TestRun_TimeoutQuotesWaitingReason(t *testing.T) {
	t.Parallel()
	c := cluster()
	scriptPodStatus(c, corev1.PodStatus{Phase: corev1.PodPending, ContainerStatuses: []corev1.ContainerStatus{{
		Name: "probe", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
	}}})
	opts := fastOptions(c, "")
	opts.Timeout = 20 * time.Millisecond
	r, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	f := findingByID(t, r, preflight.FindingNvproxyProbeFailed)
	for _, want := range []string{"timed out after 20ms", "phase Pending", "ImagePullBackOff"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("message %q missing %q", f.Message, want)
		}
	}
	assertPodGone(t, c)
}

func TestRun_UnparseableOutputIsAWarning(t *testing.T) {
	t.Parallel()
	c := cluster()
	scriptPodStatus(c, corev1.PodStatus{Phase: corev1.PodSucceeded})
	r, err := Run(context.Background(), fastOptions(c, "hello\n"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	f := findingByID(t, r, preflight.FindingNvproxyProbeFailed)
	if !strings.Contains(f.Message, "unexpected output") || !strings.Contains(f.Message, `no "runsc version" line`) {
		t.Fatalf("message = %q", f.Message)
	}
	assertPodGone(t, c)
}

func TestRun_SkippedWhenPreflightBlocks(t *testing.T) {
	t.Parallel()
	c := fake.NewSimpleClientset(readyNode("runc-1", nil)) // no RuntimeClass at all
	r, err := Run(context.Background(), fastOptions(c, sampleOutput))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Spec.Summary.Ready {
		t.Fatal("missing RuntimeClass must keep the report not ready")
	}
	want := []string{preflight.FindingRuntimeClassMissing, preflight.FindingNvproxyProbeSkipped}
	if got := findingIDs(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	if verbs(c, "create") != 0 || r.Metadata.Probe.Node != "" {
		t.Fatalf("blocked preflight must not pick a node or create a pod: %+v", r.Metadata.Probe)
	}
}

func TestRun_LeftoverPodIsAnError(t *testing.T) {
	t.Parallel()
	c := cluster()
	_, _ = c.CoreV1().Pods(DefaultNamespace).Create(context.Background(),
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: PodName, Namespace: DefaultNamespace}}, metav1.CreateOptions{})
	_, err := Run(context.Background(), fastOptions(c, sampleOutput))
	if err == nil || !strings.Contains(err.Error(), "already exists") || !apierrors.IsAlreadyExists(err) {
		t.Fatalf("err = %v, want wrapped AlreadyExists", err)
	}
}

func TestRun_CreateForbiddenIsAnError(t *testing.T) {
	t.Parallel()
	c := cluster()
	c.PrependReactor("create", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("pods"), PodName, errors.New("denied"))
	})
	_, err := Run(context.Background(), fastOptions(c, sampleOutput))
	if !apierrors.IsForbidden(err) {
		t.Fatalf("err = %v, want Forbidden", err)
	}
}

func TestRun_CustomNamespaceImageAndPath(t *testing.T) {
	t.Parallel()
	c := cluster()
	var created *corev1.Pod
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		created = action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		created.Status.Phase = corev1.PodSucceeded
		return false, nil, nil
	})
	opts := fastOptions(c, sampleOutput)
	opts.Namespace, opts.Image, opts.RunscPath = "agentmoat-probe", "docker.io/library/busybox:1.37", "/opt/gvisor/runsc"
	r, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if created.Namespace != "agentmoat-probe" || created.Spec.Containers[0].Image != opts.Image || created.Spec.Volumes[0].HostPath.Path != opts.RunscPath {
		t.Fatalf("pod did not honor options: ns=%s image=%s path=%s", created.Namespace, created.Spec.Containers[0].Image, created.Spec.Volumes[0].HostPath.Path)
	}
	if r.Metadata.Probe.Namespace != "agentmoat-probe" || r.Metadata.Probe.RunscPath != "/opt/gvisor/runsc" {
		t.Fatalf("Probe metadata = %+v", r.Metadata.Probe)
	}
}

func TestRun_NilClient(t *testing.T) {
	if _, err := Run(context.Background(), Options{}); err == nil {
		t.Fatal("expected error for nil client")
	}
}
