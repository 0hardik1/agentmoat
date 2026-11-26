// Tests for the seccomp probe. The fake clientset stores whatever a create
// reactor puts in a pod's status and node, which is how each test scripts
// a trial's fate; the reader's output comes from the LogFetcher seam
// because the fake cannot serve pods/log.
package seccompprobe

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/preflight"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const runID = "abc123"

// realReaderOutput is readerScript's output over a Sentry boot log written
// by runsc release-20261123.1 (systrap, --oci-seccomp=true,
// --debug-command=boot) for a sandbox whose container ran with containerd's
// RuntimeDefault profile and called get_mempolicy and unshare once each.
// Only the pod name and namespace are substituted per test.
const realReaderOutput = `F 4f2a.boot.txt
V release-20261123.1 amd64
N %s
S %s
I 1
G 0
D 272 1
D 239 1
E
DONE 1
`

func gvisorRC(name, handler string) *nodev1.RuntimeClass {
	return &nodev1.RuntimeClass{
		ObjectMeta: metav1.ObjectMeta{Name: name}, Handler: handler,
		Scheduling: &nodev1.Scheduling{NodeSelector: map[string]string{"runtime": "gvisor"}},
	}
}

func gvisorNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{"runtime": "gvisor"}},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
}

func deployment(ns, name string, mutate func(*corev1.PodSpec)) *appsv1.Deployment {
	spec := corev1.PodSpec{
		SecurityContext: &corev1.PodSecurityContext{SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers:      []corev1.Container{{Name: "app", Image: "example/app:1"}},
	}
	if mutate != nil {
		mutate(&spec)
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
			Spec:       spec,
		}},
	}
}

func cluster(objs ...runtime.Object) *fake.Clientset {
	base := []runtime.Object{gvisorRC("gvisor", "gvisor"), gvisorRC(DefaultAuditRuntimeClass, DefaultAuditRuntimeClass), gvisorNode("gv-a")}
	return fake.NewSimpleClientset(append(base, objs...)...)
}

// scriptPods makes every created trial pod Running on gv-a and every
// reader pod Succeeded, and records the pods created.
func scriptPods(c *fake.Clientset) *[]*corev1.Pod {
	var created []*corev1.Pod
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Spec.NodeName = "gv-a"
		if pod.Labels[labelComponent] == componentReader {
			pod.Status.Phase = corev1.PodSucceeded
		} else {
			pod.Status.Phase = corev1.PodRunning
		}
		created = append(created, pod.DeepCopy())
		return false, nil, nil
	})
	return &created
}

func readerLogs(out string) LogFetcher {
	return func(context.Context, kubernetes.Interface, string, string) (string, error) { return out, nil }
}

func baseOpts(c kubernetes.Interface) Options {
	return Options{Client: c, RunID: runID, RunFor: time.Millisecond, Timeout: time.Second, PollInterval: time.Millisecond}
}

func findingByID(fs []schema.PreflightFinding, id string) *schema.PreflightFinding {
	for i := range fs {
		if fs[i].ID == id {
			return &fs[i]
		}
	}
	return nil
}

func TestParseReaderOutputRealLog(t *testing.T) {
	t.Parallel()
	logs, err := parseReaderOutput(fmt.Sprintf(realReaderOutput, "agentmoat-seccomp-1-abc123", "shop"))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Version != "release-20261123.1" || logs[0].Arch != "amd64" || logs[0].Installed != 1 {
		t.Fatalf("got %+v", logs)
	}
	w := schema.SeccompWorkload{}
	merge(&w, logs)
	got := []string{}
	for _, b := range w.Blocked {
		got = append(got, fmt.Sprintf("%s/%d/%s/%d", b.Name, b.Number, b.Action, b.Count))
	}
	if want := "get_mempolicy/239/denied/1,unshare/272/denied/1"; strings.Join(got, ",") != want {
		t.Fatalf("blocked = %v, want %s", got, want)
	}
}

func TestParseReaderOutputRejectsTruncation(t *testing.T) {
	t.Parallel()
	if _, err := parseReaderOutput("F a.boot.txt\nI 1\n"); err == nil {
		t.Fatal("want an error for output without DONE")
	}
	if _, err := parseReaderOutput("ERR cannot enter /x\n"); err == nil {
		t.Fatal("want an error for ERR")
	}
}

func TestSyscallNameByArch(t *testing.T) {
	t.Parallel()
	if SyscallName("amd64", 239) != "get_mempolicy" || SyscallName("arm64", 239) != "move_pages" || SyscallName("amd64", 99999) != "syscall-99999" {
		t.Fatal("syscall table lookup is wrong")
	}
}

func TestBuildTrialKeepsSeccompAndDropsLabels(t *testing.T) {
	t.Parallel()
	d := deployment("shop", "web", func(s *corev1.PodSpec) {
		s.Containers[0].SecurityContext = &corev1.SecurityContext{
			SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			Capabilities:   &corev1.Capabilities{Add: []corev1.Capability{"NET_ADMIN"}},
		}
		s.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/d"}, {Name: "from-vct", MountPath: "/v"}}
		s.Volumes = []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "web-data"}}}}
	})
	o := withDefaults(Options{RunID: runID})
	ref := schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "web"}
	pod, changes := buildTrial(d.Spec.Template.Spec, ref, o, runID)

	if *pod.Spec.RuntimeClassName != DefaultAuditRuntimeClass || pod.Namespace != "shop" || !strings.HasSuffix(pod.Name, "-"+runID) {
		t.Fatalf("pod = %s/%s rc %v", pod.Namespace, pod.Name, pod.Spec.RuntimeClassName)
	}
	if _, ok := pod.Labels["app"]; ok {
		t.Fatal("trial pod carries the workload's labels")
	}
	sc := pod.Spec.Containers[0].SecurityContext
	if sc.SeccompProfile.Type != corev1.SeccompProfileTypeUnconfined || sc.Capabilities.Add[0] != "NET_ADMIN" {
		t.Fatal("trial changed the container's seccomp profile or capabilities")
	}
	if pod.Spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("trial changed the pod's seccomp profile")
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatal("restartPolicy not Never")
	}
	for _, v := range pod.Spec.Volumes {
		if v.EmptyDir == nil {
			t.Fatalf("volume %s is not emptyDir", v.Name)
		}
	}
	if len(pod.Spec.Volumes) != 2 || len(changes) != 3 {
		t.Fatalf("volumes %d, changes %v", len(pod.Spec.Volumes), changes)
	}
	if d.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatal("buildTrial mutated the workload's spec")
	}
}

func TestRunDryRunCreatesNothing(t *testing.T) {
	t.Parallel()
	c := cluster(deployment("shop", "web", nil))
	o := baseOpts(c)
	o.DryRun = true
	o.Targets = []schema.WorkloadRef{{Kind: "Deployment", Namespace: "shop", Name: "web"}}
	report, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range c.Actions() {
		if a.GetVerb() == "create" || a.GetVerb() == "delete" {
			t.Fatalf("dry run made a %s call", a.GetVerb())
		}
	}
	f := findingByID(report.Spec.Findings, preflight.FindingSeccompProbeDryRun)
	if f == nil || !strings.Contains(f.Message, "Deployment shop/web as pod shop/agentmoat-seccomp-") {
		t.Fatalf("dry-run finding = %+v", f)
	}
	ws := report.Spec.Facts.Seccomp.Workloads
	if len(ws) != 1 || ws[0].Status != schema.SeccompTrialDryRun || ws[0].Profiles[0].Type != schema.SeccompProfileRuntimeDefault {
		t.Fatalf("workloads = %+v", ws)
	}
	if !report.Metadata.SeccompProbe.DryRun {
		t.Fatal("metadata not marked dry run")
	}
}

func TestRunReportsBlockedSyscallsAndCleansUp(t *testing.T) {
	t.Parallel()
	c := cluster(deployment("shop", "web", nil))
	created := scriptPods(c)
	o := baseOpts(c)
	o.Targets = []schema.WorkloadRef{{Kind: "Deployment", Namespace: "shop", Name: "web"}}
	trial := trialName(o.Targets[0], runID)
	o.Logs = readerLogs(fmt.Sprintf(realReaderOutput, trial, "shop"))

	report, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	w := report.Spec.Facts.Seccomp.Workloads[0]
	if w.Status != schema.SeccompTrialRan || w.Node != "gv-a" || w.RunscVersion != "release-20261123.1" || len(w.Blocked) != 2 {
		t.Fatalf("workload = %+v", w)
	}
	f := findingByID(report.Spec.Findings, preflight.FindingSeccompBlockedSyscalls)
	if f == nil || f.Severity != schema.SeverityWarn || !strings.Contains(f.Message, "get_mempolicy (denied, 1x), unshare (denied, 1x)") {
		t.Fatalf("blocked finding = %+v", f)
	}
	if len(*created) != 2 {
		t.Fatalf("created %d pods, want a trial and a reader", len(*created))
	}
	reader := (*created)[1]
	if reader.Spec.RuntimeClassName != nil || !reader.Spec.Containers[0].VolumeMounts[0].ReadOnly ||
		*reader.Spec.Volumes[0].HostPath.Type != corev1.HostPathDirectory {
		t.Fatal("reader pod must run under runc with a read-only hostPath of type Directory")
	}
	pods, _ := c.CoreV1().Pods("").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Fatalf("%d pods left behind", len(pods.Items))
	}
}

func TestRunFailsTrialWithoutLog(t *testing.T) {
	t.Parallel()
	c := cluster(deployment("shop", "web", nil))
	scriptPods(c)
	o := baseOpts(c)
	o.Targets = []schema.WorkloadRef{{Kind: "Deployment", Namespace: "shop", Name: "web"}}
	o.Logs = readerLogs("DONE 0\n")
	report, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	f := findingByID(report.Spec.Findings, preflight.FindingSeccompProbeFailed)
	if f == nil || !strings.Contains(f.Message, "no Sentry boot log") {
		t.Fatalf("failed finding = %+v", f)
	}
}

func TestRunTrialThatNeverStarts(t *testing.T) {
	t.Parallel()
	c := cluster(deployment("shop", "web", nil))
	c.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodPending
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Message: "0/1 nodes are available"}}
		return false, nil, nil
	})
	o := baseOpts(c)
	o.Timeout = 5 * time.Millisecond
	o.Targets = []schema.WorkloadRef{{Kind: "Deployment", Namespace: "shop", Name: "web"}}
	report, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	w := report.Spec.Facts.Seccomp.Workloads[0]
	if w.Status != schema.SeccompTrialFailed || !strings.Contains(w.Reason, "0/1 nodes are available") {
		t.Fatalf("workload = %+v", w)
	}
}

func TestRunSkipsWithoutAuditRuntimeClass(t *testing.T) {
	t.Parallel()
	c := fake.NewSimpleClientset(gvisorRC("gvisor", "gvisor"), gvisorNode("gv-a"), deployment("shop", "web", nil))
	o := baseOpts(c)
	o.Targets = []schema.WorkloadRef{{Kind: "Deployment", Namespace: "shop", Name: "web"}, {Kind: "Deployment", Namespace: "shop", Name: "gone"}}
	report, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, w := range report.Spec.Facts.Seccomp.Workloads {
		if w.Status != schema.SeccompTrialSkipped {
			t.Fatalf("workload %s status %s", w.Target.Name, w.Status)
		}
		reasons = append(reasons, w.Reason)
	}
	joined := strings.Join(reasons, "|")
	if !strings.Contains(joined, "could not read the workload") || !strings.Contains(joined, "deploy/runtimeclass-seccomp-audit.yaml") {
		t.Fatalf("reasons = %v", reasons)
	}
}

func TestRunDiscoversMigratedWorkloads(t *testing.T) {
	t.Parallel()
	gv := "gvisor"
	c := cluster(
		deployment("shop", "migrated", func(s *corev1.PodSpec) { s.RuntimeClassName = &gv }),
		deployment("shop", "on-runc", nil),
	)
	o := baseOpts(c)
	o.DryRun = true
	report, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	ws := report.Spec.Facts.Seccomp.Workloads
	if len(ws) != 1 || ws[0].Target.Name != "migrated" {
		t.Fatalf("workloads = %+v", ws)
	}
}

func TestRunNoTargets(t *testing.T) {
	t.Parallel()
	report, err := Run(context.Background(), baseOpts(cluster()))
	if err != nil {
		t.Fatal(err)
	}
	if findingByID(report.Spec.Findings, preflight.FindingSeccompProbeSkipped) == nil {
		t.Fatal("want a skipped finding when nothing is migrated")
	}
}
