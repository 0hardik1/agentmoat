// Tests for a verify from manifest files (`verify -f`): which workloads of
// the input are checked, what the report says about the others, and the
// refusals that come before any cluster read.
//
// The rule under test is in verify_manifests.go: exit code 4 is a rollback
// signal, so it comes only from evidence in the cluster. Input that verify
// cannot read as the cluster has it is refused (an error), or the workload
// is listed as not checked. The checks in the cluster are tested in
// pkg/verifier.
package agentmoat

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// The manifests of the tests. sandboxedWeb asks for the class; plainAPI and
// the DaemonSet do not.
const (
	sandboxedWeb = `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      runtimeClassName: gvisor
      containers: [{name: web, image: example.test/web:1}]
`
	plainAPI = `apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: shop}
spec:
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      containers: [{name: api, image: example.test/api:1}]
`
	cniDaemonSet = `apiVersion: apps/v1
kind: DaemonSet
metadata: {name: cni, namespace: kube-system}
spec:
  selector: {matchLabels: {app: cni}}
  template:
    metadata: {labels: {app: cni}}
    spec:
      hostNetwork: true
      containers: [{name: cni, image: example.test/cni:1}]
`
	// An Argo Rollout that asks for the class. verify has no reader for
	// the kind; the selector is how it finds the pods.
	sandboxedRollout = `apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata: {name: canary, namespace: shop}
spec:
  selector: {matchLabels: {app: canary}}
  template:
    metadata: {labels: {app: canary}}
    spec:
      runtimeClassName: gvisor
      containers: [{name: canary, image: example.test/canary:1}]
`
	// A Knative Service: a pod template and no selector.
	sandboxedKnative = `apiVersion: serving.knative.dev/v1
kind: Service
metadata: {name: fn, namespace: shop}
spec:
  template:
    spec:
      runtimeClassName: gvisor
      containers: [{name: fn, image: example.test/fn:1}]
`
	// A Job with generateName: the API server makes the name.
	generatedJob = `apiVersion: batch/v1
kind: Job
metadata: {generateName: migrate-, namespace: shop}
spec:
  template:
    spec:
      runtimeClassName: gvisor
      restartPolicy: Never
      containers: [{name: migrate, image: example.test/migrate:1}]
`
	// A Job with a name. After it ends, its tool or its time to live can
	// remove it.
	hookJob = `apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
  namespace: shop
  annotations: {"helm.sh/hook": pre-upgrade, "helm.sh/hook-delete-policy": hook-succeeded}
spec:
  ttlSecondsAfterFinished: 60
  template:
    spec:
      runtimeClassName: gvisor
      restartPolicy: Never
      containers: [{name: migrate, image: example.test/migrate:1}]
`
	// The same Deployment as sandboxedWeb, with no namespace in the file.
	sandboxedWebNoNamespace = `apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  selector: {matchLabels: {app: web}}
  template:
    metadata: {labels: {app: web}}
    spec:
      runtimeClassName: gvisor
      containers: [{name: web, image: example.test/web:1}]
`
)

// shopPod is one running pod in the namespace shop. class "" means that the
// pod has no runtimeClassName.
func shopPod(name, app, class string, ready bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: map[string]string{"app": app}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if class != "" {
		pod.Spec.RuntimeClassName = &class
	}
	if ready {
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	}
	return pod
}

// shopDeployment is the Deployment shop/<name> as the cluster has it.
func shopDeployment(name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}}},
	}
}

// verifyFiles runs a verify from manifests against the fake cluster.
func verifyFiles(t *testing.T, opts VerifyOptions) (*schema.VerifyReport, string) {
	t.Helper()
	var stderr bytes.Buffer
	opts.Stderr = &stderr
	report, err := Verify(context.Background(), opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return report, stderr.String()
}

// TestVerifyManifests_ChecksTheWorkloadsThatAskForTheClass: the report has a
// result for each workload that asks for the class, and counts the others.
// It says where its list came from, and it has no plan hash.
func TestVerifyManifests_ChecksTheWorkloadsThatAskForTheClass(t *testing.T) {
	t.Parallel()
	dir := writeManifests(t, map[string]string{"web.yaml": sandboxedWeb, "api.yaml": plainAPI, "sys/cni.yaml": cniDaemonSet})
	tests := []struct {
		name     string
		podClass string
		want     schema.VerifySummary
		wantExit int
	}{
		{"the pod has the class", "gvisor", schema.VerifySummary{Total: 1, OK: 1, Excluded: 2}, VerifyExitOK},
		{"the pod does not have the class", "", schema.VerifySummary{Total: 1, Mismatch: 1, Excluded: 2}, VerifyExitFailed},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset(
			shopDeployment("web"), shopPod("web-1", "web", tc.podClass, true),
			// The workloads that do not ask for the class are in the
			// cluster with no class. verify must not look at them.
			shopDeployment("api"), shopPod("api-1", "api", "", true),
		)
		report, stderr := verifyFiles(t, VerifyOptions{ManifestPaths: []string{dir}, KubeClient: client})
		if got := report.Spec.Summary; got != tc.want {
			t.Errorf("%s: summary = %+v, want %+v", tc.name, got, tc.want)
		}
		if got := VerifyExitCode(report); got != tc.wantExit {
			t.Errorf("%s: exit code = %d, want %d", tc.name, got, tc.wantExit)
		}
		wantManifestReport(t, tc.name, report)
		if want := `1 workload(s) of the input ask for RuntimeClass "gvisor"; 2 do not and were not looked at`; !strings.Contains(stderr, want) {
			t.Errorf("%s: stderr lacks %q:\n%s", tc.name, want, stderr)
		}
		for _, a := range client.Actions() {
			if a.GetNamespace() == "kube-system" {
				t.Errorf("%s: verify read the namespace of a workload that does not ask for the class: %s %s",
					tc.name, a.GetVerb(), a.GetResource().Resource)
			}
		}
	}
}

// wantManifestReport checks what a report from the three files of the test
// above says about its source and about the one workload that it checked.
func wantManifestReport(t *testing.T, name string, report *schema.VerifyReport) {
	t.Helper()
	meta := report.Metadata
	if meta.Source == nil || meta.Source.Type != schema.SourceManifests || meta.Source.Inputs != 3 ||
		meta.RuntimeClassName != "gvisor" || meta.PlanHash != "" {
		t.Errorf("%s: metadata = %+v (source %+v)", name, meta, meta.Source)
	}
	r := report.Spec.Results[0]
	if r.Target != (schema.WorkloadRef{Kind: "Deployment", Namespace: "shop", Name: "web"}) ||
		r.Expected != "gvisor" || r.Origin == nil || !strings.HasSuffix(r.Origin.File, "web.yaml") {
		t.Errorf("%s: result = %+v", name, r)
	}
}

// TestVerifyManifests_NotChecked: a workload that asks for the class and
// that verify cannot find from the file has no result. It is in the
// not-checked list with the reason, the exit code is 1 (not 0 and not 4),
// and stderr says that this is not a pass.
func TestVerifyManifests_NotChecked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		files      map[string]string
		cluster    []runtime.Object
		want       schema.VerifySummary
		wantExit   int
		wantTarget string
		wantReason string
	}{
		{
			name:       "a kind with no reader and no selector",
			files:      map[string]string{"web.yaml": sandboxedWeb, "fn.yaml": sandboxedKnative},
			cluster:    []runtime.Object{shopDeployment("web"), shopPod("web-1", "web", "gvisor", true)},
			want:       schema.VerifySummary{Total: 1, OK: 1, NotChecked: 1},
			wantExit:   VerifyExitNotChecked,
			wantTarget: "fn",
			wantReason: "no reader for the kind Service.serving.knative.dev",
		},
		{
			name:       "an object with generateName",
			files:      map[string]string{"web.yaml": sandboxedWeb, "job.yaml": generatedJob},
			cluster:    []runtime.Object{shopDeployment("web"), shopPod("web-1", "web", "gvisor", true)},
			want:       schema.VerifySummary{Total: 1, OK: 1, NotChecked: 1},
			wantExit:   VerifyExitNotChecked,
			wantTarget: "migrate-",
			wantReason: "generateName",
		},
		{
			name:       "a selector that selects no pods",
			files:      map[string]string{"web.yaml": sandboxedWeb, "canary.yaml": sandboxedRollout},
			cluster:    []runtime.Object{shopDeployment("web"), shopPod("web-1", "web", "gvisor", true)},
			want:       schema.VerifySummary{Total: 1, OK: 1, NotChecked: 1},
			wantExit:   VerifyExitNotChecked,
			wantTarget: "canary",
			wantReason: "scaled to zero",
		},
		{
			// The deploy worked, the Job ran, and its time to live (or
			// the delete policy of the hook) removed it. "Not found" is
			// not a rollback signal here.
			name:       "a Job that is not in the cluster",
			files:      map[string]string{"web.yaml": sandboxedWeb, "job.yaml": hookJob},
			cluster:    []runtime.Object{shopDeployment("web"), shopPod("web-1", "web", "gvisor", true)},
			want:       schema.VerifySummary{Total: 1, OK: 1, NotChecked: 1},
			wantExit:   VerifyExitNotChecked,
			wantTarget: "migrate",
			wantReason: "Job shop/migrate is not in the cluster",
		},
		{
			// A failure in the cluster is the rollback signal, also when
			// another workload has no verdict.
			name:       "not checked, and a mismatch",
			files:      map[string]string{"web.yaml": sandboxedWeb, "fn.yaml": sandboxedKnative},
			cluster:    []runtime.Object{shopDeployment("web"), shopPod("web-1", "web", "", true)},
			want:       schema.VerifySummary{Total: 1, Mismatch: 1, NotChecked: 1},
			wantExit:   VerifyExitFailed,
			wantTarget: "fn",
			wantReason: "no reader for the kind",
		},
		{
			// Each workload that asks for the class has no verdict. The
			// report has no result, and it is not an error return: the
			// reasons are in the report.
			name:       "only a workload that is not checked",
			files:      map[string]string{"fn.yaml": sandboxedKnative, "api.yaml": plainAPI},
			want:       schema.VerifySummary{NotChecked: 1, Excluded: 1},
			wantExit:   VerifyExitNotChecked,
			wantTarget: "fn",
			wantReason: "no reader for the kind",
		},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset(tc.cluster...)
		report, stderr := verifyFiles(t, VerifyOptions{ManifestPaths: []string{writeManifests(t, tc.files)}, KubeClient: client})
		if got := report.Spec.Summary; got != tc.want {
			t.Errorf("%s: summary = %+v, want %+v", tc.name, got, tc.want)
		}
		if got := VerifyExitCode(report); got != tc.wantExit {
			t.Errorf("%s: exit code = %d, want %d", tc.name, got, tc.wantExit)
		}
		if len(report.Spec.NotChecked) != 1 {
			t.Fatalf("%s: not checked = %+v, want one entry", tc.name, report.Spec.NotChecked)
		}
		n := report.Spec.NotChecked[0]
		if n.Target.Name != tc.wantTarget || !strings.Contains(n.Reason, tc.wantReason) || n.Origin == nil {
			t.Errorf("%s: not checked = %+v", tc.name, n)
		}
		for _, r := range report.Spec.Results {
			if r.Target.Name == tc.wantTarget {
				t.Errorf("%s: %s has a result (%s); a workload with no verdict must have none", tc.name, r.Target.Name, r.Status)
			}
		}
		if want := "could not be checked. This is not a pass"; !strings.Contains(stderr, want) {
			t.Errorf("%s: stderr lacks %q:\n%s", tc.name, want, stderr)
		}
		// stderr has the count. The name and the reason are text from a
		// manifest, and they are in the report only.
		if strings.Contains(stderr, n.Reason) {
			t.Errorf("%s: stderr has the reason of a workload, which is text from a manifest:\n%s", tc.name, stderr)
		}
	}
}

// TestVerifyManifests_OtherKindBySelector: an Argo Rollout whose pods are in
// the cluster is checked on those pods, and the result says how.
func TestVerifyManifests_OtherKindBySelector(t *testing.T) {
	t.Parallel()
	dir := writeManifests(t, map[string]string{"canary.yaml": sandboxedRollout})
	client := fake.NewSimpleClientset(shopPod("canary-1", "canary", "gvisor", true))
	report, _ := verifyFiles(t, VerifyOptions{ManifestPaths: []string{dir}, KubeClient: client})
	if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1}); got != want {
		t.Fatalf("summary = %+v, want %+v (not checked: %+v)", got, want, report.Spec.NotChecked)
	}
	r := report.Spec.Results[0]
	if r.Target.Kind != "Rollout.argoproj.io" || r.Basis != schema.VerifyBasisSelector {
		t.Errorf("result = %+v", r)
	}
}

// TestVerifyManifests_Options: the namespace is a default for objects that
// name none, the class name selects other workloads, a label selector
// limits them, and standard input is a source.
func TestVerifyManifests_Options(t *testing.T) {
	t.Parallel()
	cluster := func() *fake.Clientset {
		return fake.NewSimpleClientset(shopDeployment("web"), shopPod("web-1", "web", "gvisor", true))
	}

	t.Run("a default namespace", func(t *testing.T) {
		t.Parallel()
		dir := writeManifests(t, map[string]string{"web.yaml": sandboxedWebNoNamespace})
		report, _ := verifyFiles(t, VerifyOptions{ManifestPaths: []string{dir}, Namespace: "shop", KubeClient: cluster()})
		if report.Spec.Summary.OK != 1 || report.Spec.Results[0].Target.Namespace != "shop" {
			t.Errorf("report = %+v", report.Spec)
		}
	})

	t.Run("the namespace is not a filter", func(t *testing.T) {
		t.Parallel()
		// The file names the namespace shop. -n other does not move the
		// object and does not remove it from the list.
		dir := writeManifests(t, map[string]string{"web.yaml": sandboxedWeb})
		report, _ := verifyFiles(t, VerifyOptions{ManifestPaths: []string{dir}, Namespace: "other", KubeClient: cluster()})
		if report.Spec.Summary.OK != 1 || report.Spec.Results[0].Target.Namespace != "shop" {
			t.Errorf("report = %+v", report.Spec)
		}
	})

	t.Run("another class name", func(t *testing.T) {
		t.Parallel()
		kata := strings.ReplaceAll(sandboxedWeb, "runtimeClassName: gvisor", "runtimeClassName: kata")
		kata = strings.ReplaceAll(kata, "name: web, namespace: shop", "name: vm, namespace: shop")
		dir := writeManifests(t, map[string]string{"web.yaml": sandboxedWeb, "vm.yaml": kata})
		// The cluster has the Deployment vm with one pod of the class kata.
		// The Deployment web (class gvisor) is not looked at in this run.
		client := fake.NewSimpleClientset(shopDeployment("vm"), shopPod("vm-1", "vm", "kata", true))
		report, _ := verifyFiles(t, VerifyOptions{ManifestPaths: []string{dir}, RuntimeClassName: "kata", KubeClient: client})
		if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1, Excluded: 1}); got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
		if r := report.Spec.Results[0]; r.Target.Name != "vm" || r.Expected != "kata" || report.Metadata.RuntimeClassName != "kata" {
			t.Errorf("result = %+v", r)
		}
	})

	t.Run("a label selector", func(t *testing.T) {
		t.Parallel()
		labeled := strings.Replace(sandboxedRollout, "metadata: {name: canary, namespace: shop}",
			"metadata: {name: canary, namespace: shop, labels: {tier: edge}}", 1)
		dir := writeManifests(t, map[string]string{"web.yaml": sandboxedWeb, "canary.yaml": labeled})
		client := fake.NewSimpleClientset(shopPod("canary-1", "canary", "gvisor", true))
		report, _ := verifyFiles(t, VerifyOptions{ManifestPaths: []string{dir}, LabelSelector: "tier=edge", KubeClient: client})
		if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1}); got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
	})

	t.Run("standard input", func(t *testing.T) {
		t.Parallel()
		report, _ := verifyFiles(t, VerifyOptions{
			ManifestPaths: []string{"-"}, Stdin: strings.NewReader(sandboxedWeb + "---\n" + plainAPI), KubeClient: cluster(),
		})
		if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1, Excluded: 1}); got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
	})

	t.Run("one identity two times", func(t *testing.T) {
		t.Parallel()
		// The input defines shop/web in two files. It is one workload in
		// the cluster, so it gets one result.
		dir := writeManifests(t, map[string]string{"a/web.yaml": sandboxedWeb, "b/web.yaml": sandboxedWeb})
		report, _ := verifyFiles(t, VerifyOptions{ManifestPaths: []string{dir}, KubeClient: cluster()})
		if got, want := report.Spec.Summary, (schema.VerifySummary{Total: 1, OK: 1}); got != want {
			t.Errorf("summary = %+v, want %+v", got, want)
		}
	})
}

// TestVerifyManifests_Wait: the wait time goes to the verifier, and a pod
// that is never Ready is a mismatch after the wait (the rollback signal).
func TestVerifyManifests_Wait(t *testing.T) {
	t.Parallel()
	dir := writeManifests(t, map[string]string{"web.yaml": sandboxedWeb})
	tests := []struct {
		name     string
		ready    bool
		wantExit int
		timedOut bool
	}{
		{"pods that are Ready", true, VerifyExitOK, false},
		{"a pod that is never Ready", false, VerifyExitFailed, true},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset(shopDeployment("web"), shopPod("web-1", "web", "gvisor", tc.ready))
		report, _ := verifyFiles(t, VerifyOptions{
			ManifestPaths: []string{dir}, KubeClient: client,
			Wait: 30 * time.Second, Clock: &jumpClock{now: time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)},
		})
		if got := VerifyExitCode(report); got != tc.wantExit {
			t.Errorf("%s: exit code = %d, want %d (%+v)", tc.name, got, tc.wantExit, report.Spec.Summary)
		}
		if w := report.Metadata.Wait; w == nil || w.TimedOut != tc.timedOut {
			t.Errorf("%s: wait = %+v", tc.name, w)
		}
	}
}

// TestVerifyManifests_RefusedBeforeTheCluster: input that verify cannot read
// as the cluster has it is an error, and the error comes before a client is
// built or used. Not one of these is exit code 4: a refusal says nothing
// about the cluster.
//   - KUBECONFIG names a server that does not exist. A run that got as far
//     as the client would fail with a connection error, and not with the
//     text that the test wants.
//   - opts.KubeClient fails the test on any call.
func TestVerifyManifests_RefusedBeforeTheCluster(t *testing.T) {
	kubeconfig := writeFile(t, "kubeconfig", `apiVersion: v1
kind: Config
current-context: canary-must-not-appear
clusters: [{name: canary, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: canary-must-not-appear, context: {cluster: canary, user: canary}}]
users: [{name: canary, user: {}}]
`)
	t.Setenv("KUBECONFIG", kubeconfig) // no t.Parallel: Setenv forbids it

	plain := writeManifests(t, map[string]string{"api.yaml": plainAPI, "cni.yaml": cniDaemonSet})
	noNamespace := writeManifests(t, map[string]string{"web.yaml": sandboxedWebNoNamespace, "api.yaml": plainAPI})
	sandboxed := writeManifests(t, map[string]string{"web.yaml": sandboxedWeb})
	// A manifest with a flow mapping that is not closed. (A YAML file with
	// no apiVersion is not a manifest for the directory walk, and is not an
	// error; this one has an apiVersion.)
	broken := writeManifests(t, map[string]string{
		"web.yaml": sandboxedWeb,
		"bad.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api\nspec: {}\n",
	})

	tests := []struct {
		name   string
		opts   VerifyOptions
		want   string
		wantIs error
	}{
		{
			name:   "no workload asks for the class",
			opts:   VerifyOptions{ManifestPaths: []string{plain}},
			want:   `no workload in ` + plain + ` asks for RuntimeClass "gvisor" (2 workload(s) read)`,
			wantIs: ErrNothingToVerify,
		},
		{
			name:   "no workload asks for the class name that was given",
			opts:   VerifyOptions{ManifestPaths: []string{sandboxed}, RuntimeClassName: "kata"},
			want:   `asks for RuntimeClass "kata"`,
			wantIs: ErrNothingToVerify,
		},
		{
			name:   "input that is not rendered",
			opts:   VerifyOptions{ManifestPaths: []string{kustomizeTree(t)}},
			want:   "kustomize build <dir> | agentmoat verify -f -",
			wantIs: ErrUnrenderedInput,
		},
		{
			name: "a workload with no namespace",
			opts: VerifyOptions{ManifestPaths: []string{noNamespace}},
			want: "name no namespace (Deployment web), so verify does not know where they are in the cluster. Pass -n <namespace>",
		},
		{
			name: "a document that does not parse",
			opts: VerifyOptions{ManifestPaths: []string{broken}},
			want: "bad.yaml",
		},
		{
			name: "a plan and manifests",
			opts: VerifyOptions{ManifestPaths: []string{sandboxed}, PlanPath: "plan.json"},
			want: "a verify has one source",
		},
		{
			name: "a class name that is not a name",
			opts: VerifyOptions{ManifestPaths: []string{sandboxed}, RuntimeClassName: "a\nb"},
			want: "not a valid RuntimeClass name",
		},
		{
			name: "standard input with no reader",
			opts: VerifyOptions{ManifestPaths: []string{"-"}},
			want: "standard input",
		},
		{
			name: "a path that is not there",
			opts: VerifyOptions{ManifestPaths: []string{"/no/such/manifests"}},
			want: "/no/such/manifests",
		},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset()
		client.PrependReactor("*", "*", func(action clienttesting.Action) (bool, runtime.Object, error) {
			t.Errorf("%s: a refused verify called the Kubernetes API: %s %s", tc.name, action.GetVerb(), action.GetResource().Resource)
			return true, nil, errors.New("no API calls for input that is refused")
		})
		var stderr bytes.Buffer
		for _, withClient := range []bool{true, false} {
			opts := tc.opts
			opts.Stderr = &stderr
			if withClient {
				opts.KubeClient = client
			}
			report, err := Verify(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s (client given: %v): err = %v, want it to have %q", tc.name, withClient, err, tc.want)
				continue
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("%s: err = %v, want errors.Is %v", tc.name, err, tc.wantIs)
			}
			if report != nil {
				t.Errorf("%s: a refused verify returned a report", tc.name)
			}
			if strings.Contains(err.Error(), "canary-must-not-appear") || strings.Contains(err.Error(), "127.0.0.1") {
				t.Errorf("%s: the error is from the cluster connection, so the input was not refused first: %v", tc.name, err)
			}
		}
		if n := len(client.Actions()); n != 0 {
			t.Errorf("%s: %d API actions recorded, want 0", tc.name, n)
		}
	}
}
