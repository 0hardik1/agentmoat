// Unit tests for the verify_migration tool. The probe path (in_pod_probe)
// is covered in detail by pkg/verifier/verifier_test.go; here we verify
// the MCP wiring plus one probe-on round-trip using a fake ExecRunner so
// the SPDY-exec path is bypassed.
package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/0hardik1/agentmoat/internal/schema"
	"github.com/0hardik1/agentmoat/pkg/planner"
)

// strPtr is a single-line helper because *string parameters appear in
// pod specs (RuntimeClassName) and Go has no shorthand for that.
func strPtr(s string) *string { return &s }

func runningPodWithRCN(ns, name string, labels map[string]string, rcn *string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.PodSpec{
			RuntimeClassName: rcn,
			Containers:       []corev1.Container{{Name: "main", Image: "nginx"}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func deploymentWithSelector(ns, name string, sel map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: sel},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: sel},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "nginx"}},
				},
			},
		},
	}
}

// writeVerifyPlan writes a one-step plan targeting the named Deployment.
func writeVerifyPlan(t *testing.T, ns, name string) string {
	t.Helper()
	plan := schema.NewMigrationPlan()
	plan.Spec = schema.PlanSpec{
		Summary: schema.PlanSummary{Total: 1, Included: 1},
		Options: schema.PlannerOptions{RuntimeClassName: "gvisor"},
		Steps: []schema.PlanStep{{
			Order:            1,
			Target:           schema.WorkloadRef{Kind: "Deployment", Namespace: ns, Name: name},
			Action:           "set-runtime-class",
			RuntimeClassName: "gvisor",
		}},
		Excluded: []schema.ExcludedWorkload{},
	}
	// The loader verifies metadata.planHash against the steps/options.
	hash, err := planner.ComputePlanHash(plan.Spec.Steps, plan.Spec.Options)
	if err != nil {
		t.Fatalf("compute plan hash: %v", err)
	}
	plan.Metadata = schema.PlanMetadata{GeneratedAt: "2026-05-22T12:00:00Z", PlanHash: hash}
	data, _ := yaml.Marshal(plan)
	path := filepath.Join(t.TempDir(), "plan.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	return path
}

func TestVerifyMigrationHandler_HappyPath(t *testing.T) {
	t.Parallel()
	ns, name := "agentmoat-test", "web"
	labels := map[string]string{"app": "web"}
	client := fake.NewSimpleClientset(
		deploymentWithSelector(ns, name, labels),
		runningPodWithRCN(ns, "web-0", labels, strPtr("gvisor")),
	)
	srv := newTestServer(client)
	planPath := writeVerifyPlan(t, ns, name)

	result, err := callTool(t, srv, "verify_migration", map[string]any{
		"plan_path": planPath,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.VerifyReport
	readTool(t, result, &report)
	if report.Spec.Summary.OK != 1 {
		t.Errorf("Summary.OK: got %d, want 1; report=%+v", report.Spec.Summary.OK, report)
	}
}

// fakeProbe is a one-shot ExecRunner that returns the configured bytes.
// Allows the in_pod_probe path to be exercised without dialing SPDY.
type fakeProbe struct {
	stdout, stderr []byte
	err            error
}

func (f *fakeProbe) Exec(_ context.Context, _, _, _ string, _ []string) ([]byte, []byte, error) {
	return f.stdout, f.stderr, f.err
}

func TestVerifyMigrationHandler_InPodProbe(t *testing.T) {
	t.Parallel()
	ns, name := "agentmoat-test", "web"
	labels := map[string]string{"app": "web"}
	client := fake.NewSimpleClientset(
		deploymentWithSelector(ns, name, labels),
		runningPodWithRCN(ns, "web-0", labels, strPtr("gvisor")),
	)

	// Inject a fake probe that returns a gvisor-marker line so the
	// verifier's probe assertion stays OK.
	srv := newServer(Deps{
		Stderr:     io.Discard,
		KubeClient: client,
		ExecRunner: &fakeProbe{stdout: []byte("Linux x.y.z gVisor\n")},
	})

	result, err := callTool(t, srv, "verify_migration", map[string]any{
		"plan_path":    writeVerifyPlan(t, ns, name),
		"in_pod_probe": true,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.VerifyReport
	readTool(t, result, &report)
	if !report.Metadata.InPodProbe {
		t.Errorf("Metadata.InPodProbe: got false, want true")
	}
	if report.Spec.Summary.OK != 1 {
		t.Errorf("Summary.OK: got %d, want 1", report.Spec.Summary.OK)
	}
}

// jumpClock is a verifier.Clock that moves only when the wait loop sleeps,
// so a wait of minutes runs in no time.
type jumpClock struct{ now time.Time }

func (c *jumpClock) Now() time.Time { return c.now }

func (c *jumpClock) Sleep(_ context.Context, d time.Duration) { c.now = c.now.Add(d) }

// TestVerifyMigrationHandler_Wait: the wait input is the --wait flag of the
// CLI. The report says how the wait ended, and a pod that has the class and
// is not Ready is a mismatch with the reason.
func TestVerifyMigrationHandler_Wait(t *testing.T) {
	t.Parallel()
	ns, name := "agentmoat-test", "web"
	labels := map[string]string{"app": "web"}
	notReady := runningPodWithRCN(ns, "web-0", labels, strPtr("gvisor"))
	notReady.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: "app", RestartCount: 6,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
	}}
	srv := newServer(Deps{
		Stderr:     io.Discard,
		Version:    "test",
		KubeClient: fake.NewSimpleClientset(deploymentWithSelector(ns, name, labels), notReady),
		Clock:      &jumpClock{now: time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)},
	})

	result, err := callTool(t, srv, "verify_migration", map[string]any{
		"plan_path": writeVerifyPlan(t, ns, name),
		"wait":      "20s",
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var report schema.VerifyReport
	readTool(t, result, &report)
	want := schema.VerifyWait{RequestedSeconds: 20, WaitedSeconds: 20, Rounds: 5, TimedOut: true}
	if report.Metadata.Wait == nil || *report.Metadata.Wait != want {
		t.Errorf("metadata.wait = %+v, want %+v", report.Metadata.Wait, want)
	}
	r := report.Spec.Results[0]
	if r.Status != schema.VerifyStatusMismatch || r.Readiness == nil || r.Readiness.Ready != 0 {
		t.Fatalf("result = %+v, want a mismatch with readiness", r)
	}
	if wantMsg := "pod web-0 is not Ready: container app is waiting: CrashLoopBackOff, 6 restart(s)"; r.Message != wantMsg {
		t.Errorf("message = %q, want %q", r.Message, wantMsg)
	}
}

// TestVerifyMigrationHandler_NoWait: with no wait input the tool looks one
// time, as before, and the report has no wait block.
func TestVerifyMigrationHandler_NoWait(t *testing.T) {
	t.Parallel()
	ns, name := "agentmoat-test", "web"
	labels := map[string]string{"app": "web"}
	srv := newTestServer(fake.NewSimpleClientset(
		deploymentWithSelector(ns, name, labels),
		runningPodWithRCN(ns, "web-0", labels, strPtr("gvisor")),
	))
	for _, wait := range []any{nil, "", "0", "0s"} {
		args := map[string]any{"plan_path": writeVerifyPlan(t, ns, name)}
		if wait != nil {
			args["wait"] = wait
		}
		result, err := callTool(t, srv, "verify_migration", args)
		if err != nil {
			t.Fatalf("wait=%v: call: %v", wait, err)
		}
		var report schema.VerifyReport
		readTool(t, result, &report)
		if report.Metadata.Wait != nil || report.Spec.Summary.OK != 1 {
			t.Errorf("wait=%v: wait block = %+v, summary = %+v; want no wait block and 1 ok", wait, report.Metadata.Wait, report.Spec.Summary)
		}
	}
}

// TestVerifyMigrationHandler_BadWait: a wait that is not a duration, and a
// negative one, are tool errors. Neither reads the cluster.
func TestVerifyMigrationHandler_BadWait(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	srv := newTestServer(client)
	for wait, want := range map[string]string{
		"soon": `"soon" is not a duration`,
		"3":    `"3" is not a duration`,
		"-5s":  "the wait time is -5s",
	} {
		result, err := callTool(t, srv, "verify_migration", map[string]any{
			"plan_path": writeVerifyPlan(t, "ns", "web"),
			"wait":      wait,
		})
		if err != nil {
			t.Fatalf("wait=%q: call: %v", wait, err)
		}
		expectToolError(t, result, want)
	}
	if n := len(client.Actions()); n != 0 {
		t.Errorf("the cluster was read %d time(s) for a bad wait", n)
	}
}

// ---------------------------------------------------------------------------
// manifest_paths: the workloads to check come from manifest files.
// ---------------------------------------------------------------------------

// A Deployment that asks for the class, a Deployment that does not, and a
// Knative Service that asks for it (a kind with no reader and no selector).
const (
	verifySandboxedWeb = `apiVersion: apps/v1
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
	verifyPlainAPI = `apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: shop}
spec:
  selector: {matchLabels: {app: api}}
  template:
    metadata: {labels: {app: api}}
    spec:
      containers: [{name: api, image: example.test/api:1}]
`
	verifyKnative = `apiVersion: serving.knative.dev/v1
kind: Service
metadata: {name: fn, namespace: shop}
spec:
  template:
    spec:
      runtimeClassName: gvisor
      containers: [{name: fn, image: example.test/fn:1}]
`
)

// writeVerifyManifests writes the files (name -> text) into a new directory.
func writeVerifyManifests(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestVerifyMigrationHandler_Manifests: with manifest_paths the tool checks
// the workloads of the files that ask for the class, and the report says
// what it did not look at and what it could not check. A tool result has no
// exit code, so summary.notChecked is how a client sees "not a pass".
func TestVerifyMigrationHandler_Manifests(t *testing.T) {
	t.Parallel()
	labels := map[string]string{"app": "web"}
	tests := []struct {
		name  string
		files map[string]string
		args  map[string]any
		pod   *string
		want  schema.VerifySummary
	}{
		{
			name:  "the pods have the class",
			files: map[string]string{"web.yaml": verifySandboxedWeb, "api.yaml": verifyPlainAPI},
			pod:   strPtr("gvisor"),
			want:  schema.VerifySummary{Total: 1, OK: 1, Excluded: 1},
		},
		{
			name:  "a pod does not have the class",
			files: map[string]string{"web.yaml": verifySandboxedWeb, "api.yaml": verifyPlainAPI},
			want:  schema.VerifySummary{Total: 1, Mismatch: 1, Excluded: 1},
		},
		{
			name:  "a workload with no verdict",
			files: map[string]string{"web.yaml": verifySandboxedWeb, "fn.yaml": verifyKnative},
			pod:   strPtr("gvisor"),
			want:  schema.VerifySummary{Total: 1, OK: 1, NotChecked: 1},
		},
		{
			// Each input reaches the library: the file has no namespace
			// and the class name is another one.
			name: "namespace and runtime_class_name",
			files: map[string]string{"web.yaml": `apiVersion: apps/v1
kind: Deployment
metadata: {name: web}
spec:
  template:
    spec:
      runtimeClassName: sandbox
      containers: [{name: web, image: example.test/web:1}]
`},
			args: map[string]any{"namespace": "shop", "runtime_class_name": "sandbox"},
			pod:  strPtr("sandbox"),
			want: schema.VerifySummary{Total: 1, OK: 1},
		},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset(
			deploymentWithSelector("shop", "web", labels),
			runningPodWithRCN("shop", "web-0", labels, tc.pod),
		)
		args := map[string]any{"manifest_paths": []any{writeVerifyManifests(t, tc.files)}}
		for k, v := range tc.args {
			args[k] = v
		}
		result, err := callTool(t, newTestServer(client), "verify_migration", args)
		if err != nil {
			t.Fatalf("%s: call: %v", tc.name, err)
		}
		var report schema.VerifyReport
		readTool(t, result, &report)
		if got := report.Spec.Summary; got != tc.want {
			t.Errorf("%s: summary = %+v, want %+v", tc.name, got, tc.want)
		}
		if report.Metadata.Source == nil || report.Metadata.Source.Type != schema.SourceManifests || report.Metadata.PlanHash != "" {
			t.Errorf("%s: metadata = %+v", tc.name, report.Metadata)
		}
		if len(report.Spec.NotChecked) != tc.want.NotChecked {
			t.Errorf("%s: notChecked = %+v", tc.name, report.Spec.NotChecked)
		}
	}
}

// TestVerifyMigrationHandler_ManifestErrors: the tool takes one source, and
// input that the library refuses is a tool error. Not one of these calls
// reads the cluster.
func TestVerifyMigrationHandler_ManifestErrors(t *testing.T) {
	t.Parallel()
	sandboxed := writeVerifyManifests(t, map[string]string{"web.yaml": verifySandboxedWeb})
	plain := writeVerifyManifests(t, map[string]string{"api.yaml": verifyPlainAPI})
	overlay := writeVerifyManifests(t, map[string]string{
		"web.yaml":           verifySandboxedWeb,
		"kustomization.yaml": "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources: [web.yaml]\n",
	})
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no source", map[string]any{}, "needs one source: plan_path, or manifest_paths"},
		{"an empty list", map[string]any{"manifest_paths": []any{}}, "needs one source"},
		{"two sources", map[string]any{"plan_path": writeVerifyPlan(t, "shop", "web"), "manifest_paths": []any{sandboxed}}, "not the two"},
		// The standard input of the server is the protocol channel. The
		// call must be an error and must not wait for input.
		{"standard input", map[string]any{"manifest_paths": []any{"-"}}, "standard input"},
		{"nothing asks for the class", map[string]any{"manifest_paths": []any{plain}}, "there is nothing to verify"},
		{"input that is not rendered", map[string]any{"manifest_paths": []any{overlay}}, "verify -f cannot answer"},
		{"a class name that is not a name", map[string]any{"manifest_paths": []any{sandboxed}, "runtime_class_name": "a\nb"}, "not a valid RuntimeClass name"},
		{"a path that is not there", map[string]any{"manifest_paths": []any{"/no/such/manifests"}}, "/no/such/manifests"},
	}
	for _, tc := range tests {
		client := fake.NewSimpleClientset()
		result, err := callTool(t, newTestServer(client), "verify_migration", tc.args)
		if err != nil {
			t.Fatalf("%s: unexpected go-level error: %v", tc.name, err)
		}
		expectToolError(t, result, tc.want)
		if n := len(client.Actions()); n != 0 {
			t.Errorf("%s: the cluster was read %d time(s)", tc.name, n)
		}
	}
}
