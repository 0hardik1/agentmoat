// Tests for FromManifests: workloads from YAML / JSON text with no cluster.
//
// Most cases are short inline manifests, one behavior each. The last test
// in the file is the important one for trust: it sends the same objects
// through the cluster path (Enumerate, with a fake clientset) and through
// FromManifests, and requires identical Workloads.
package scanner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

// one wraps a single manifest text as the input list.
func one(data string) []ManifestInput {
	return []ManifestInput{{Name: "m.yaml", Data: []byte(data)}}
}

// mustManifests runs FromManifests and fails the test on error.
func mustManifests(t *testing.T, inputs []ManifestInput, opts ManifestOptions) *ManifestResult {
	t.Helper()
	res, err := FromManifests(inputs, opts)
	if err != nil {
		t.Fatalf("FromManifests: %v", err)
	}
	return res
}

// ids renders the workload identities as "Kind/namespace/name" strings, in
// result order, so a test can compare a whole result in one line.
func ids(res *ManifestResult) []string {
	out := make([]string, 0, len(res.Workloads))
	for _, w := range res.Workloads {
		out = append(out, w.Kind+"/"+w.Namespace+"/"+w.Name)
	}
	return out
}

func equalStrings(a, b []string) bool {
	return strings.Join(a, "\n") == strings.Join(b, "\n")
}

const deploymentWeb = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
  labels:
    tier: frontend
spec:
  template:
    spec:
      containers:
        - name: web
          image: nginx:1.27
`

func TestFromManifests_Shapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		data        string
		want        []string
		wantSkipped map[string]int
		wantObjects int
	}{
		{
			name: "multi-document YAML with comments and empty documents",
			data: "# leading comment\n---\n" + deploymentWeb + "---\n# only a comment\n---\n\n---\n" +
				"apiVersion: v1\nkind: Pod\nmetadata: {name: p, namespace: shop}\nspec: {containers: [{name: c, image: busybox}]}\n",
			want:        []string{"Deployment/shop/web", "Pod/shop/p"},
			wantObjects: 2,
		},
		{
			name: "a separator with a trailing comment still separates",
			data: deploymentWeb + "--- # next one\n" +
				"apiVersion: v1\nkind: Pod\nmetadata: {name: p, namespace: shop}\nspec: {containers: [{name: c, image: busybox}]}\n",
			want:        []string{"Deployment/shop/web", "Pod/shop/p"},
			wantObjects: 2,
		},
		{
			name:        "one JSON object",
			data:        `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"p","namespace":"shop"},"spec":{"containers":[{"name":"c","image":"busybox"}]}}`,
			want:        []string{"Pod/shop/p"},
			wantObjects: 1,
		},
		{
			name: "a JSON array of objects",
			data: `[{"apiVersion":"v1","kind":"Pod","metadata":{"name":"a","namespace":"shop"},"spec":{"containers":[{"name":"c","image":"busybox"}]}},
 {"apiVersion":"v1","kind":"Service","metadata":{"name":"svc","namespace":"shop"}}]`,
			want:        []string{"Pod/shop/a"},
			wantSkipped: map[string]int{"Service": 1},
			wantObjects: 2,
		},
		{
			name: "a stream of JSON values (jq -c output)",
			data: `{"apiVersion":"v1","kind":"Pod","metadata":{"name":"a","namespace":"shop"},"spec":{"containers":[{"name":"c","image":"busybox"}]}}
{"apiVersion":"v1","kind":"Pod","metadata":{"name":"b","namespace":"shop"},"spec":{"containers":[{"name":"c","image":"busybox"}]}}
`,
			want:        []string{"Pod/shop/a", "Pod/shop/b"},
			wantObjects: 2,
		},
		{
			name: "kind List is expanded (kubectl get -o yaml)",
			data: "apiVersion: v1\nkind: List\nitems:\n" +
				"  - apiVersion: v1\n    kind: Pod\n    metadata: {name: a, namespace: shop}\n    spec: {containers: [{name: c, image: busybox}]}\n" +
				"  - apiVersion: v1\n    kind: ConfigMap\n    metadata: {name: cm, namespace: shop}\n",
			want:        []string{"Pod/shop/a"},
			wantSkipped: map[string]int{"ConfigMap": 1},
			wantObjects: 2,
		},
		{
			name: "a typed list gives its items the kind and group they omit",
			data: "apiVersion: apps/v1\nkind: DeploymentList\nitems:\n" +
				"  - metadata: {name: a, namespace: shop}\n    spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n" +
				"  - metadata: {name: b, namespace: shop}\n    spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n",
			want:        []string{"Deployment/shop/a", "Deployment/shop/b"},
			wantObjects: 2,
		},
		{
			name:        "flow-style YAML starts with a brace but is not JSON",
			data:        "{apiVersion: v1, kind: Pod, metadata: {name: flow, namespace: shop}, spec: {containers: [{name: c, image: busybox}]}}\n",
			want:        []string{"Pod/shop/flow"},
			wantObjects: 1,
		},
		{
			name:        "an empty list has no objects",
			data:        "apiVersion: v1\nkind: List\nitems: []\n",
			want:        nil,
			wantObjects: 0,
		},
		{
			name: "old API versions decode into the same kinds",
			data: "apiVersion: apps/v1beta2\nkind: Deployment\nmetadata: {name: old-dep, namespace: legacy}\nspec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n---\n" +
				"apiVersion: extensions/v1beta1\nkind: DaemonSet\nmetadata: {name: old-ds, namespace: legacy}\nspec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n---\n" +
				"apiVersion: batch/v1beta1\nkind: CronJob\nmetadata: {name: old-cron, namespace: legacy}\nspec: {jobTemplate: {spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}}}\n",
			want:        []string{"CronJob/legacy/old-cron", "DaemonSet/legacy/old-ds", "Deployment/legacy/old-dep"},
			wantObjects: 3,
		},
		{
			name: "a Job in another API group is not a batch Job",
			data: "apiVersion: batch.volcano.sh/v1alpha1\nkind: Job\nmetadata: {name: train, namespace: ml}\n" +
				"spec: {tasks: [{name: worker, template: {spec: {containers: [{name: c, image: busybox}]}}}]}\n",
			want:        nil,
			wantSkipped: map[string]int{"Job.batch.volcano.sh": 1},
			wantObjects: 1,
		},
		{
			name: "kinds with a pod template at spec.template are workloads",
			data: "apiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata: {name: canary, namespace: shop}\nspec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n---\n" +
				"apiVersion: apps/v1\nkind: ReplicaSet\nmetadata: {name: rs, namespace: shop}\nspec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n---\n" +
				"apiVersion: v1\nkind: ReplicationController\nmetadata: {name: rc, namespace: shop}\nspec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n---\n" +
				"apiVersion: serving.knative.dev/v1\nkind: Service\nmetadata: {name: fn, namespace: shop}\nspec: {template: {spec: {containers: [{image: busybox}]}}}\n",
			want: []string{
				"ReplicaSet.apps/shop/rs",
				"ReplicationController/shop/rc",
				"Rollout.argoproj.io/shop/canary",
				"Service.serving.knative.dev/shop/fn",
			},
			wantObjects: 4,
		},
		{
			name: "a pod template at spec.jobTemplate is found too",
			data: "apiVersion: example.test/v1\nkind: ScheduledTask\nmetadata: {name: nightly, namespace: ops}\n" +
				"spec: {jobTemplate: {spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}}}\n",
			want:        []string{"ScheduledTask.example.test/ops/nightly"},
			wantObjects: 1,
		},
		{
			name: "a Rollout that only references a Deployment has no template",
			data: "apiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata: {name: ref, namespace: shop}\n" +
				"spec: {workloadRef: {apiVersion: apps/v1, kind: Deployment, name: web}}\n",
			want:        nil,
			wantSkipped: map[string]int{"Rollout.argoproj.io": 1},
			wantObjects: 1,
		},
		{
			name: "a spec.template that is not a pod template is skipped, not an error",
			data: "apiVersion: cluster.x-k8s.io/v1beta1\nkind: MachineDeployment\nmetadata: {name: md, namespace: capi}\n" +
				"spec: {template: {spec: {clusterName: c1, version: v1.30.0, containers: not-a-list}}}\n",
			want:        nil,
			wantSkipped: map[string]int{"MachineDeployment.cluster.x-k8s.io": 1},
			wantObjects: 1,
		},
		{
			name:        "an object with no kind is counted, not classified",
			data:        "apiVersion: v2\nname: my-chart\nversion: 1.0.0\n",
			want:        nil,
			wantSkipped: map[string]int{NoKindKey: 1},
			wantObjects: 1,
		},
		{
			name:        "a scalar document is counted, not classified",
			data:        "just a string\n",
			want:        nil,
			wantSkipped: map[string]int{NoKindKey: 1},
			wantObjects: 1,
		},
		{
			name:        "empty input",
			data:        "",
			want:        nil,
			wantObjects: 0,
		},
		{
			name: "CRLF line endings and a byte-order mark",
			data: "\xEF\xBB\xBF" + strings.ReplaceAll(deploymentWeb, "\n", "\r\n") + "---\r\n" +
				"apiVersion: v1\r\nkind: Pod\r\nmetadata: {name: p, namespace: shop}\r\nspec: {containers: [{name: c, image: busybox}]}\r\n",
			want:        []string{"Deployment/shop/web", "Pod/shop/p"},
			wantObjects: 2,
		},
		{
			name: "a system namespace is not skipped: the user pointed at the file",
			data: "apiVersion: apps/v1\nkind: DaemonSet\nmetadata: {name: cni, namespace: kube-system}\n" +
				"spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n",
			want:        []string{"DaemonSet/kube-system/cni"},
			wantObjects: 1,
		},
		{
			name: "generateName stands in for a missing name",
			data: "apiVersion: batch/v1\nkind: Job\nmetadata: {generateName: backup-, namespace: ops}\n" +
				"spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n",
			want:        []string{"Job/ops/backup-"},
			wantObjects: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := mustManifests(t, one(tc.data), ManifestOptions{})
			if got := ids(res); !equalStrings(got, tc.want) {
				t.Errorf("workloads = %v, want %v", got, tc.want)
			}
			if res.Objects != tc.wantObjects {
				t.Errorf("Objects = %d, want %d", res.Objects, tc.wantObjects)
			}
			wantSkipped := tc.wantSkipped
			if wantSkipped == nil {
				wantSkipped = map[string]int{}
			}
			if !reflect.DeepEqual(res.Skipped, wantSkipped) {
				t.Errorf("Skipped = %v, want %v", res.Skipped, wantSkipped)
			}
			if len(res.Origins) != len(res.Workloads) {
				t.Errorf("Origins has %d entries for %d workloads", len(res.Origins), len(res.Workloads))
			}
		})
	}
}

// TestFromManifests_PodSpecAndMetadata: the decoded Workload carries the pod
// spec fields the classifier reads, and the controller's own labels and
// annotations (rules such as io-uring read an annotation on the controller,
// not on the pod template).
func TestFromManifests_PodSpecAndMetadata(t *testing.T) {
	t.Parallel()
	const data = `apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: vector-db
  namespace: data
  labels: {app: vector-db}
  annotations: {agentmoat.io/uses-iouring: "true"}
spec:
  template:
    metadata:
      labels: {pod-label: "yes"}
      annotations: {pod-annotation: "yes"}
    spec:
      hostNetwork: true
      runtimeClassName: gvisor
      initContainers:
        - {name: init, image: busybox:1.36}
      containers:
        - name: db
          image: example.test/vector-db:2
          securityContext:
            privileged: true
            capabilities: {add: [NET_RAW]}
      volumes:
        - name: logs
          hostPath: {path: /var/log}
`
	res := mustManifests(t, one(data), ManifestOptions{})
	if len(res.Workloads) != 1 {
		t.Fatalf("workloads = %v", ids(res))
	}
	w := res.Workloads[0]

	// Compare whole values, not single fields: one DeepEqual for each part
	// of the Workload says more than a chain of nil checks.
	runtimeClass, privileged := "gvisor", true
	wantSpec := corev1.PodSpec{
		HostNetwork:      true,
		RuntimeClassName: &runtimeClass,
		InitContainers:   []corev1.Container{{Name: "init", Image: "busybox:1.36"}},
		Containers: []corev1.Container{{
			Name:  "db",
			Image: "example.test/vector-db:2",
			SecurityContext: &corev1.SecurityContext{
				Privileged:   &privileged,
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW"}},
			},
		}},
		Volumes: []corev1.Volume{{
			Name:         "logs",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/log"}},
		}},
	}
	if !reflect.DeepEqual(w.PodSpec, wantSpec) {
		t.Errorf("PodSpec =\n  %+v\nwant\n  %+v", w.PodSpec, wantSpec)
	}
	if want := []string{"busybox:1.36", "example.test/vector-db:2"}; !equalStrings(w.ImageRefs, want) {
		t.Errorf("ImageRefs = %v, want %v (init containers first)", w.ImageRefs, want)
	}
	// The controller's own metadata, not the pod template's.
	if want := map[string]string{"app": "vector-db"}; !reflect.DeepEqual(w.Labels, want) {
		t.Errorf("Labels = %v, want %v", w.Labels, want)
	}
	if want := map[string]string{"agentmoat.io/uses-iouring": "true"}; !reflect.DeepEqual(w.Annotations, want) {
		t.Errorf("Annotations = %v, want %v", w.Annotations, want)
	}
}

// TestFromManifests_LenientDecode pins a limit that the docs state: this is
// not a schema validator. A misspelled field is ignored, so the workload is
// read without it.
func TestFromManifests_LenientDecode(t *testing.T) {
	t.Parallel()
	const data = `apiVersion: v1
kind: Pod
metadata: {name: typo, namespace: shop}
spec:
  hostNetwrok: true
  containers: [{name: c, image: busybox}]
`
	res := mustManifests(t, one(data), ManifestOptions{})
	if len(res.Workloads) != 1 || res.Workloads[0].PodSpec.HostNetwork {
		t.Fatalf("workloads = %+v, want one workload with HostNetwork false", res.Workloads)
	}
}

func TestFromManifests_Origins(t *testing.T) {
	t.Parallel()
	const data = `# Header comment for the file.

---
# Source: shop/templates/web.yaml
apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}
---
# a comment

# Source: shop/templates/list.yaml
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Pod
    metadata: {name: a, namespace: shop}
    spec: {containers: [{name: c, image: busybox}]}
  - apiVersion: v1
    kind: Pod
    metadata: {name: b, namespace: shop}
    spec: {containers: [{name: c, image: busybox}]}
`
	res := mustManifests(t, []ManifestInput{{Name: "<stdin>", Data: []byte(data)}}, ManifestOptions{})

	want := map[string]ManifestOrigin{
		// The first document (the header comment) has no content and gets
		// no index of its own content, but it still occupies position 1.
		// Object is the position in the document: the two Pods of the List
		// have one document and one line, and are objects 1 and 2 of it.
		"Deployment/shop/web": {File: "<stdin>", Document: 2, Object: 1, Line: 5, TemplateSource: "shop/templates/web.yaml"},
		"Pod/shop/a":          {File: "<stdin>", Document: 3, Object: 1, Line: 13, TemplateSource: "shop/templates/list.yaml"},
		"Pod/shop/b":          {File: "<stdin>", Document: 3, Object: 2, Line: 13, TemplateSource: "shop/templates/list.yaml"},
	}
	got := map[string]ManifestOrigin{}
	for i, id := range ids(res) {
		got[id] = res.Origins[i]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("origins =\n  %+v\nwant\n  %+v", got, want)
	}
}

func TestFromManifests_Namespace(t *testing.T) {
	t.Parallel()
	const data = "apiVersion: v1\nkind: Pod\nmetadata: {name: bare}\nspec: {containers: [{name: c, image: busybox}]}\n---\n" +
		"apiVersion: v1\nkind: Pod\nmetadata: {name: placed, namespace: shop}\nspec: {containers: [{name: c, image: busybox}]}\n"

	t.Run("no default leaves the namespace empty and counts it", func(t *testing.T) {
		t.Parallel()
		res := mustManifests(t, one(data), ManifestOptions{})
		if got, want := ids(res), []string{"Pod//bare", "Pod/shop/placed"}; !equalStrings(got, want) {
			t.Errorf("workloads = %v, want %v", got, want)
		}
		if res.NoNamespace != 1 {
			t.Errorf("NoNamespace = %d, want 1", res.NoNamespace)
		}
	})

	t.Run("the default fills in a missing namespace and is not a filter", func(t *testing.T) {
		t.Parallel()
		res := mustManifests(t, one(data), ManifestOptions{DefaultNamespace: "team-a"})
		if got, want := ids(res), []string{"Pod/shop/placed", "Pod/team-a/bare"}; !equalStrings(got, want) {
			t.Errorf("workloads = %v, want %v", got, want)
		}
		if res.NoNamespace != 0 {
			t.Errorf("NoNamespace = %d, want 0", res.NoNamespace)
		}
	})
}

func TestFromManifests_LabelSelector(t *testing.T) {
	t.Parallel()
	data := deploymentWeb + "---\n" +
		"apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api, namespace: shop, labels: {tier: backend}}\n" +
		"spec: {template: {metadata: {labels: {tier: frontend}}, spec: {containers: [{name: c, image: busybox}]}}}\n"

	res := mustManifests(t, one(data), ManifestOptions{LabelSelector: "tier=frontend"})
	// `api` has tier=frontend on its pod template only; the selector reads
	// the controller's labels, as the cluster scan does.
	if got, want := ids(res), []string{"Deployment/shop/web"}; !equalStrings(got, want) {
		t.Errorf("workloads = %v, want %v", got, want)
	}
	if res.Filtered != 1 {
		t.Errorf("Filtered = %d, want 1", res.Filtered)
	}

	if _, err := FromManifests(one(data), ManifestOptions{LabelSelector: "tier in (("}); err == nil ||
		!strings.Contains(err.Error(), "label selector") {
		t.Errorf("bad selector: err = %v, want a label selector error", err)
	}
}

// ownedBy returns an ownerReferences block (YAML, 2-space indented under
// metadata) that names a controller.
func ownedBy(kind, name string) string {
	return "  ownerReferences:\n    - {apiVersion: apps/v1, kind: " + kind + ", name: " + name + ", uid: u1, controller: true}\n"
}

func TestFromManifests_Owners(t *testing.T) {
	t.Parallel()
	const podSpec = "spec: {containers: [{name: c, image: busybox}]}\n"
	const tmplSpec = "spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n"

	deployment := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: shop\n" + tmplSpec
	replicaSet := "apiVersion: apps/v1\nkind: ReplicaSet\nmetadata:\n  name: web-5d9\n  namespace: shop\n" + ownedBy("Deployment", "web") + tmplSpec
	pod := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: web-5d9-abc\n  namespace: shop\n" + ownedBy("ReplicaSet", "web-5d9") + podSpec
	cronJob := "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: nightly\n  namespace: shop\nspec: {jobTemplate: {spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}}}\n"
	job := "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: nightly-123\n  namespace: shop\n" + ownedBy("CronJob", "nightly") + tmplSpec

	tests := []struct {
		name        string
		data        string
		want        []string
		wantDropped int
	}{
		{
			name:        "kubectl get all: only the top controller stays",
			data:        strings.Join([]string{deployment, replicaSet, pod}, "---\n"),
			want:        []string{"Deployment/shop/web"},
			wantDropped: 2,
		},
		{
			name:        "the order of the documents does not matter",
			data:        strings.Join([]string{pod, replicaSet, deployment}, "---\n"),
			want:        []string{"Deployment/shop/web"},
			wantDropped: 2,
		},
		{
			name: "kubectl get pods: the owner is not in the text, so the Pod stays",
			data: pod,
			want: []string{"Pod/shop/web-5d9-abc"},
		},
		{
			name:        "a ReplicaSet and its Pod without the Deployment: the ReplicaSet stays",
			data:        strings.Join([]string{replicaSet, pod}, "---\n"),
			want:        []string{"ReplicaSet.apps/shop/web-5d9"},
			wantDropped: 1,
		},
		{
			name:        "a Job with its CronJob is dropped",
			data:        strings.Join([]string{cronJob, job}, "---\n"),
			want:        []string{"CronJob/shop/nightly"},
			wantDropped: 1,
		},
		{
			name: "an owner of the same name in another namespace does not count",
			data: strings.Replace(deployment, "namespace: shop", "namespace: other", 1) + "---\n" + replicaSet,
			want: []string{"Deployment/other/web", "ReplicaSet.apps/shop/web-5d9"},
		},
		{
			name: "an owner reference that is not a controller does not count",
			data: deployment + "---\n" + strings.Replace(replicaSet, "controller: true", "controller: false", 1),
			want: []string{"Deployment/shop/web", "ReplicaSet.apps/shop/web-5d9"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := mustManifests(t, one(tc.data), ManifestOptions{})
			if got := ids(res); !equalStrings(got, tc.want) {
				t.Errorf("workloads = %v, want %v", got, tc.want)
			}
			if res.OwnedDropped != tc.wantDropped {
				t.Errorf("OwnedDropped = %d, want %d", res.OwnedDropped, tc.wantDropped)
			}
		})
	}
}

func TestFromManifests_DuplicatesAndInputOrder(t *testing.T) {
	t.Parallel()
	a := ManifestInput{Name: "overlays/a/web.yaml", Data: []byte(deploymentWeb)}
	b := ManifestInput{Name: "overlays/b/web.yaml", Data: []byte(strings.Replace(deploymentWeb, "nginx:1.27", "nginx:1.28", 1))}
	c := ManifestInput{Name: "base/pod.yaml", Data: []byte(
		"apiVersion: v1\nkind: Pod\nmetadata: {name: p, namespace: shop}\nspec: {containers: [{name: c, image: busybox}]}\n")}

	forward := mustManifests(t, []ManifestInput{a, b, c}, ManifestOptions{})
	backward := mustManifests(t, []ManifestInput{c, b, a}, ManifestOptions{})

	// Both copies are kept, in file order.
	if got, want := ids(forward), []string{"Deployment/shop/web", "Deployment/shop/web", "Pod/shop/p"}; !equalStrings(got, want) {
		t.Fatalf("workloads = %v, want %v", got, want)
	}
	if forward.Origins[0].File != "overlays/a/web.yaml" || forward.Origins[1].File != "overlays/b/web.yaml" {
		t.Errorf("duplicate order = %s, %s; want a then b", forward.Origins[0].File, forward.Origins[1].File)
	}
	wantDup := "Deployment shop/web is defined 2 times (overlays/a/web.yaml:1, overlays/b/web.yaml:1)"
	if len(forward.Duplicates) != 1 || forward.Duplicates[0] != wantDup {
		t.Errorf("Duplicates = %q, want [%q]", forward.Duplicates, wantDup)
	}

	// `-f a -f b` and `-f b -f a` give the same result, byte for byte.
	if !reflect.DeepEqual(forward, backward) {
		t.Errorf("result depends on input order:\n forward  %+v\n backward %+v", forward, backward)
	}
}

func TestFromManifests_Problems(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		inputs        []ManifestInput
		wantProblems  int
		wantSubstring []string
		wantTemplated bool
	}{
		{
			name:         "YAML that does not parse names the file and the line",
			inputs:       one(deploymentWeb + "---\napiVersion: v1\nkind: Pod\nmetadata: {name: [unterminated\n"),
			wantProblems: 1,
			// 13 lines of Deployment, the separator on 14, and the broken
			// flow sequence on the third line of the second document.
			wantSubstring: []string{"1 manifest problem(s):", "m.yaml:17: did not find expected"},
		},
		{
			name: "an unrendered Helm template gets the render hint",
			inputs: one("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ include \"chart.fullname\" . }}\n" +
				"spec:\n  replicas: {{ .Values.replicaCount }}\n"),
			wantProblems:  1,
			wantSubstring: []string{"helm template <chart> | agentmoat scan -f -"},
			wantTemplated: true,
		},
		{
			// `name: {{ .Release.Name }}` IS valid YAML syntax: a mapping
			// whose key is a mapping. The library reports it with a printed
			// Go value; the message must not carry that.
			name:          "a template in a value position gives a readable message",
			inputs:        one("apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: {{ .Release.Name }}\n"),
			wantProblems:  1,
			wantSubstring: []string{"a mapping key is itself a mapping", "helm template"},
			wantTemplated: true,
		},
		{
			name:          "a templated JSON-looking file gets the hint too",
			inputs:        one("{{- if .Values.enabled }}\napiVersion: v1\nkind: Pod\n{{- end }}\n"),
			wantProblems:  1,
			wantSubstring: []string{"helm template"},
			wantTemplated: true,
		},
		{
			name: "every bad input is reported, not only the first",
			inputs: []ManifestInput{
				{Name: "a.yaml", Data: []byte("kind: [bad\n")},
				{Name: "b.yaml", Data: []byte(deploymentWeb)},
				{Name: "c.yaml", Data: []byte("kind: {also bad\n")},
			},
			wantProblems:  2,
			wantSubstring: []string{"2 manifest problem(s):", "a.yaml:1:", "c.yaml:1:"},
		},
		{
			name: "a field of the wrong type names the object and the field",
			inputs: one("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web, namespace: shop}\n" +
				"spec: {replicas: three, template: {spec: {containers: [{name: c, image: busybox}]}}}\n"),
			wantProblems:  1,
			wantSubstring: []string{`Deployment "web":`, "spec.replicas"},
		},
		{
			name: "a workload with no name and no generateName",
			inputs: one("apiVersion: apps/v1\nkind: Deployment\nmetadata: {namespace: shop}\n" +
				"spec: {template: {spec: {containers: [{name: c, image: busybox}]}}}\n"),
			wantProblems:  1,
			wantSubstring: []string{"metadata.name is missing"},
		},
		{
			name:          "metadata of the wrong shape",
			inputs:        one("apiVersion: v1\nkind: Pod\nmetadata: just-a-string\n"),
			wantProblems:  1,
			wantSubstring: []string{"reading apiVersion, kind and metadata"},
		},
		{
			name:          "a broken value in a JSON stream",
			inputs:        one(`{"apiVersion":"v1","kind":"Pod","metadata":{"name":"a"},"spec":{"containers":[{"name":"c"}]}}` + "\n{\"kind\": \n"),
			wantProblems:  1,
			wantSubstring: []string{"m.yaml:2:"},
		},
		{
			// The YAML parser reads one document. Text after "--- {...}"
			// would be dropped with no message, so the line is refused.
			name: "a separator line with content is refused, not passed over",
			inputs: one(deploymentWeb + "--- {apiVersion: v1, kind: Pod, metadata: {name: hidden}, " +
				"spec: {hostNetwork: true, containers: [{name: c, image: busybox}]}}\n"),
			wantProblems:  1,
			wantSubstring: []string{"m.yaml:14:", "document separator"},
		},
		{
			name:         "an alias bomb is refused, not expanded",
			inputs:       one(aliasBomb()),
			wantProblems: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res, err := FromManifests(tc.inputs, ManifestOptions{})
			if res != nil {
				t.Errorf("result = %+v, want nil when there is a problem", res)
			}
			var manifestErr *ManifestError
			if !errors.As(err, &manifestErr) {
				t.Fatalf("err = %v (%T), want a *ManifestError", err, err)
			}
			if len(manifestErr.Problems) != tc.wantProblems {
				t.Errorf("problems = %d, want %d: %v", len(manifestErr.Problems), tc.wantProblems, err)
			}
			for _, sub := range tc.wantSubstring {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error text lacks %q:\n%s", sub, err)
				}
			}
			templated := false
			for _, p := range manifestErr.Problems {
				templated = templated || p.Templated
			}
			if templated != tc.wantTemplated {
				t.Errorf("Templated = %v, want %v", templated, tc.wantTemplated)
			}
			if !tc.wantTemplated && strings.Contains(err.Error(), "helm template") {
				t.Errorf("error text has the Helm hint for input with no template syntax:\n%s", err)
			}
		})
	}
}

// aliasBomb is the "billion laughs" YAML document: nine levels of anchors,
// each referring to the one before nine times.
func aliasBomb() string {
	var b strings.Builder
	b.WriteString("a: &a [x,x,x,x,x,x,x,x,x]\n")
	prev := "a"
	for _, name := range []string{"b", "c", "d", "e", "f", "g", "h", "i"} {
		b.WriteString(name + ": &" + name + " [")
		for i := 0; i < 9; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			b.WriteString("*" + prev)
		}
		b.WriteString("]\n")
		prev = name
	}
	return b.String()
}

// TestFromManifests_ParityWithEnumerate is the trust test. The same six
// objects go through the cluster path (a fake clientset and Enumerate) and
// through FromManifests (written out as YAML first). The Workloads must be
// identical, field for field: a verdict from a pull-request check must be
// the verdict the cluster scan gives for that object.
func TestFromManifests_ParityWithEnumerate(t *testing.T) {
	t.Parallel()
	const ns = "parity"
	privileged := true
	rich := corev1.PodSpec{
		HostNetwork: true,
		InitContainers: []corev1.Container{{
			Name: "init", Image: "busybox:1.36",
		}},
		Containers: []corev1.Container{{
			Name:  "main",
			Image: "example.test/app:1",
			Env:   []corev1.EnvVar{{Name: "AGENTMOAT_USES_FUSE", Value: "true"}},
			SecurityContext: &corev1.SecurityContext{
				Privileged:   &privileged,
				Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"NET_RAW", "SYS_ADMIN"}},
			},
		}},
		Volumes: []corev1.Volume{{
			Name:         "kvm",
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/kvm"}},
		}},
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{
			Name: name, Namespace: ns,
			Labels:      map[string]string{"app": name},
			Annotations: map[string]string{"agentmoat.io/uses-iouring": "true"},
		}
	}
	template := corev1.PodTemplateSpec{Spec: rich}

	objects := []runtime.Object{
		&appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: meta("dep"),
			Spec: appsv1.DeploymentSpec{Template: template},
		},
		&appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "StatefulSet"}, ObjectMeta: meta("sts"),
			Spec: appsv1.StatefulSetSpec{Template: template},
		},
		&appsv1.DaemonSet{
			TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: meta("ds"),
			Spec: appsv1.DaemonSetSpec{Template: template},
		},
		&batchv1.Job{
			TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"}, ObjectMeta: meta("job"),
			Spec: batchv1.JobSpec{Template: template},
		},
		&batchv1.CronJob{
			TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "CronJob"}, ObjectMeta: meta("cron"),
			Spec: batchv1.CronJobSpec{JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{Template: template}}},
		},
		&corev1.Pod{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: meta("pod"),
			Spec: rich,
		},
	}

	fromCluster, err := Enumerate(context.Background(), fake.NewSimpleClientset(objects...), EnumerateOptions{})
	if err != nil {
		t.Fatalf("Enumerate: %v", err)
	}

	var stream strings.Builder
	for _, obj := range objects {
		data, err := yaml.Marshal(obj)
		if err != nil {
			t.Fatalf("marshal %T: %v", obj, err)
		}
		stream.WriteString("---\n")
		stream.Write(data)
	}
	fromFiles := mustManifests(t, one(stream.String()), ManifestOptions{})

	if len(fromCluster) != 6 || len(fromFiles.Workloads) != 6 {
		t.Fatalf("cluster gave %d workloads, files gave %d; want 6 and 6", len(fromCluster), len(fromFiles.Workloads))
	}
	for i := range fromCluster {
		if !reflect.DeepEqual(fromCluster[i], fromFiles.Workloads[i]) {
			t.Errorf("workload %d differs:\n cluster %+v\n files   %+v", i, fromCluster[i], fromFiles.Workloads[i])
		}
	}
}

// TestFromManifests_OverlayPatchIsNotAWorkload is the case that would turn a
// pipeline gate into a rubber stamp. A Kustomize overlay patch names a
// Deployment and carries only the field it changes. Read as a complete
// object it has no containers, so no rule fires: a "compatible" workload
// that asks for the sandbox, next to a base that is incompatible and does
// not. The object that really gets deployed is sandboxed AND incompatible,
// and nothing in the result would say so.
func TestFromManifests_OverlayPatchIsNotAWorkload(t *testing.T) {
	t.Parallel()
	inputs := []ManifestInput{
		{Name: "base/web.yaml", Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n" +
			"spec: {template: {spec: {hostNetwork: true, containers: [{name: c, image: busybox}]}}}\n")},
		{Name: "overlay/patch.yaml", Data: []byte("apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\n" +
			"spec: {template: {spec: {runtimeClassName: gvisor}}}\n")},
		{Name: "overlay/kustomization.yaml", Data: []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n" +
			"resources: [../base]\npatches: [{path: patch.yaml}]\n")},
	}
	res := mustManifests(t, inputs, ManifestOptions{DefaultNamespace: "shop"})

	// One workload: the base. Its pod spec is the base's, with no class.
	if got, want := ids(res), []string{"Deployment/shop/web"}; !equalStrings(got, want) {
		t.Fatalf("workloads = %v, want %v", got, want)
	}
	if res.Origins[0].File != "base/web.yaml" || res.Workloads[0].PodSpec.RuntimeClassName != nil {
		t.Errorf("the workload is not the base: origin %+v, runtimeClassName %v", res.Origins[0], res.Workloads[0].PodSpec.RuntimeClassName)
	}
	if len(res.Duplicates) != 0 {
		t.Errorf("Duplicates = %v, want none: the patch is not a second workload", res.Duplicates)
	}

	wantPartial := []ManifestPartial{{
		Kind: "Deployment", Namespace: "shop", Name: "web", RuntimeClassName: "gvisor",
		Origin: ManifestOrigin{File: "overlay/patch.yaml", Document: 1, Object: 1, Line: 1},
	}}
	if !reflect.DeepEqual(res.Partial, wantPartial) {
		t.Errorf("Partial = %+v, want %+v", res.Partial, wantPartial)
	}
	if want := []string{"overlay/kustomization.yaml"}; !equalStrings(res.Kustomizations, want) {
		t.Errorf("Kustomizations = %v, want %v", res.Kustomizations, want)
	}
	if want := map[string]int{"Kustomization.kustomize.config.k8s.io": 1}; !reflect.DeepEqual(res.Skipped, want) {
		t.Errorf("Skipped = %v, want %v (a partial object is not counted as skipped)", res.Skipped, want)
	}
	if res.Objects != 3 {
		t.Errorf("Objects = %d, want 3", res.Objects)
	}
}

func TestFromManifests_Partial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		data        string
		wantPartial []string // "Kind/name/runtimeClassName"
		wantSkipped map[string]int
	}{
		{
			name:        "a patch that changes only replicas",
			data:        "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: web}\nspec: {replicas: 5}\n",
			wantPartial: []string{"Deployment/web/"},
		},
		{
			name:        "a patch with no name (the kustomization selects the target)",
			data:        "apiVersion: apps/v1\nkind: StatefulSet\nspec: {template: {spec: {hostNetwork: true}}}\n",
			wantPartial: []string{"StatefulSet//"},
		},
		{
			name: "a CronJob and a Pod patch",
			data: "apiVersion: batch/v1\nkind: CronJob\nmetadata: {name: nightly}\nspec: {jobTemplate: {spec: {template: {spec: {runtimeClassName: gvisor}}}}}\n---\n" +
				"apiVersion: v1\nkind: Pod\nmetadata: {name: p}\nspec: {runtimeClassName: gvisor}\n",
			wantPartial: []string{"CronJob/nightly/gvisor", "Pod/p/gvisor"},
		},
		{
			name: "an Argo Rollout patch that sets the class is partial",
			data: "apiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata: {name: canary}\n" +
				"spec: {template: {spec: {runtimeClassName: gvisor}}}\n",
			wantPartial: []string{"Rollout.argoproj.io/canary/gvisor"},
		},
		{
			// KubeVirt, Cluster API and others have a spec.template that is
			// not a pod template. With no runtimeClassName there is nothing
			// to say it is one, so it stays "not a workload".
			name: "another kind with a template and no class is skipped, not partial",
			data: "apiVersion: kubevirt.io/v1\nkind: VirtualMachine\nmetadata: {name: vm}\n" +
				"spec: {template: {spec: {nodeSelector: {disk: ssd}, volumes: [{name: root}]}}}\n",
			wantSkipped: map[string]int{"VirtualMachine.kubevirt.io": 1},
		},
		{
			// Flux's Kustomization is a cluster object, not a build file.
			name: "a Flux Kustomization is not a kustomize build file",
			data: "apiVersion: kustomize.toolkit.fluxcd.io/v1\nkind: Kustomization\nmetadata: {name: apps}\n" +
				"spec: {path: ./apps, prune: true}\n",
			wantSkipped: map[string]int{"Kustomization.kustomize.toolkit.fluxcd.io": 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			res := mustManifests(t, one(tc.data), ManifestOptions{})
			if len(res.Workloads) != 0 {
				t.Errorf("workloads = %v, want none", ids(res))
			}
			var got []string
			for _, p := range res.Partial {
				got = append(got, p.Kind+"/"+p.Name+"/"+p.RuntimeClassName)
			}
			if !equalStrings(got, tc.wantPartial) {
				t.Errorf("Partial = %v, want %v", got, tc.wantPartial)
			}
			wantSkipped := tc.wantSkipped
			if wantSkipped == nil {
				wantSkipped = map[string]int{}
			}
			if !reflect.DeepEqual(res.Skipped, wantSkipped) {
				t.Errorf("Skipped = %v, want %v", res.Skipped, wantSkipped)
			}
			if len(res.Kustomizations) != 0 {
				t.Errorf("Kustomizations = %v, want none", res.Kustomizations)
			}
		})
	}
}

// TestFromManifests_E2EWorkloads reads the manifest file that `make e2e`
// applies to the kind cluster. The header of that file lists 14 workloads;
// the cluster scan in the e2e run must find 14, and so must this.
func TestFromManifests_E2EWorkloads(t *testing.T) {
	t.Parallel()
	const path = "../../test/e2e/manifests/workloads.yaml"
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the e2e manifests: %v", err)
	}
	res := mustManifests(t, []ManifestInput{{Name: path, Data: data}}, ManifestOptions{DefaultNamespace: "agentmoat-e2e"})

	kinds := map[string]int{}
	for _, w := range res.Workloads {
		kinds[w.Kind]++
		if w.Namespace != "agentmoat-e2e" {
			t.Errorf("%s %s: namespace = %q, want the default", w.Kind, w.Name, w.Namespace)
		}
	}
	want := map[string]int{"Deployment": 7, "StatefulSet": 1, "Pod": 6}
	if !reflect.DeepEqual(kinds, want) {
		t.Errorf("kinds = %v, want %v", kinds, want)
	}
	if res.Objects != 14 || len(res.Skipped) != 0 || res.NoNamespace != 0 {
		t.Errorf("Objects = %d, Skipped = %v, NoNamespace = %d; want 14, none, 0", res.Objects, res.Skipped, res.NoNamespace)
	}
}

func TestSplitYAMLDocuments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		data      string
		wantLines []int // first content line of each document
		wantIndex []int
		wantBad   []int // lines that start with "---" and are not separators
	}{
		{name: "empty", data: ""},
		{name: "comments only", data: "# a\n\n# b\n"},
		{name: "one document, no trailing newline", data: "a: 1", wantLines: []int{1}, wantIndex: []int{1}},
		{name: "leading separator", data: "---\na: 1\n", wantLines: []int{2}, wantIndex: []int{2}},
		{name: "two documents", data: "a: 1\n---\nb: 2\n", wantLines: []int{1, 3}, wantIndex: []int{1, 2}},
		{name: "an empty document in the middle keeps the numbering", data: "a: 1\n---\n---\nb: 2\n", wantLines: []int{1, 4}, wantIndex: []int{1, 3}},
		{name: "an indented line of dashes is content", data: "a: |\n  ---not a separator\nb: 2\n", wantLines: []int{1}, wantIndex: []int{1}},
		{name: "dashes with other text are reported", data: "a: 1\n----\nb: 2\n--- {c: 3}\n", wantLines: []int{1, 3}, wantIndex: []int{1, 2}, wantBad: []int{2, 4}},
		{name: "separator with trailing spaces", data: "a: 1\n---   \nb: 2\n", wantLines: []int{1, 3}, wantIndex: []int{1, 2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs, bad := splitYAMLDocuments([]byte(tc.data))
			if !reflect.DeepEqual(bad, tc.wantBad) {
				t.Errorf("bad separators = %v, want %v", bad, tc.wantBad)
			}
			var lines, index []int
			for _, d := range docs {
				lines = append(lines, d.line)
				index = append(index, d.index)
			}
			if !reflect.DeepEqual(lines, tc.wantLines) || !reflect.DeepEqual(index, tc.wantIndex) {
				t.Errorf("lines = %v index = %v, want lines %v index %v", lines, index, tc.wantLines, tc.wantIndex)
			}
		})
	}
}

// TestFromManifests_ObjectPosition: ManifestOrigin.Object counts the objects
// of one document in reading order. `agentmoat patch` finds the object it
// edits by this number, so the shapes that are easy to count wrong are here:
// an object that is not a workload takes a number, a List inside a List is
// counted in reading order, an empty List takes none, and each document
// starts at 1 again.
func TestFromManifests_ObjectPosition(t *testing.T) {
	t.Parallel()
	const pod = "{apiVersion: v1, kind: Pod, metadata: {name: %s, namespace: shop}, spec: {containers: [{name: c, image: busybox}]}}"
	item := func(name string) string { return fmt.Sprintf(pod, name) }
	data := "apiVersion: v1\nkind: List\nitems:\n" +
		"  - {apiVersion: v1, kind: ConfigMap, metadata: {name: settings, namespace: shop}}\n" + // object 1, not a workload
		"  - " + item("a") + "\n" + // object 2
		"  - {apiVersion: v1, kind: List, items: []}\n" + // an empty List: no object
		"  - apiVersion: v1\n    kind: List\n    items:\n      - " + item("b") + "\n" + // object 3
		"  - " + item("c") + "\n" + // object 4
		"---\n" +
		"- " + item("d") + "\n" + // a top-level array: object 1
		"- " + item("e") + "\n" // object 2
	res := mustManifests(t, []ManifestInput{{Name: "all.yaml", Data: []byte(data)}}, ManifestOptions{})

	type position struct{ document, object int }
	want := map[string]position{
		"Pod/shop/a": {1, 2}, "Pod/shop/b": {1, 3}, "Pod/shop/c": {1, 4},
		"Pod/shop/d": {2, 1}, "Pod/shop/e": {2, 2},
	}
	got := map[string]position{}
	for i, id := range ids(res) {
		got[id] = position{res.Origins[i].Document, res.Origins[i].Object}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("positions = %v, want %v", got, want)
	}
}

// TestSplitDocuments: the exported splitter gives the document numbers of
// FromManifests, the line where each document starts, and says when the text
// is read as JSON.
func TestSplitDocuments(t *testing.T) {
	t.Parallel()
	text := "# a header\n---\nkind: A\n---\n\n# only a comment\n---\nkind: B\nname: b\n"
	docs, isJSON := SplitDocuments(append([]byte("\xEF\xBB\xBF"), text...))
	if isJSON {
		t.Fatalf("YAML text was taken for JSON")
	}
	type doc struct {
		index, firstLine int
		data             string
	}
	got := make([]doc, 0, len(docs))
	for _, d := range docs {
		got = append(got, doc{d.Index, d.FirstLine, string(d.Data)})
	}
	// Documents 1 and 3 have no content line: they are not in the result,
	// and they keep their numbers.
	want := []doc{{2, 3, "kind: A\n"}, {4, 8, "kind: B\nname: b\n"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("documents = %+v, want %+v", got, want)
	}

	for name, tc := range map[string]struct {
		text string
		want bool
	}{
		"a JSON object":               {`{"kind": "Pod"}`, true},
		"a stream of JSON values":     {"{\"kind\": \"A\"}\n{\"kind\": \"B\"}\n", true},
		"a JSON array":                {`[{"kind": "Pod"}]`, true},
		"flow style YAML":             {"{kind: Pod}\n", false},
		"an unrendered Helm template": {"{{- if .Values.on }}\nkind: Pod\n{{- end }}\n", false},
		"block style YAML":            {"kind: Pod\n", false},
	} {
		if _, got := SplitDocuments([]byte(tc.text)); got != tc.want {
			t.Errorf("%s: isJSON = %v, want %v", name, got, tc.want)
		}
	}
}

// TestFromManifests_Written: the result keeps how the input writes each
// object, with no default and no change. `patch --kustomize` names the target
// of a Kustomize patch with these values, and Kustomize matches them exactly.
func TestFromManifests_Written(t *testing.T) {
	t.Parallel()
	const text = `apiVersion: apps/v1
kind: Deployment
metadata: {name: web, namespace: shop}
spec:
  template:
    spec:
      containers: [{name: c, image: busybox}]
---
apiVersion: v1
kind: Pod
metadata: {name: once}
spec:
  containers: [{name: c, image: busybox}]
---
apiVersion: batch/v1
kind: CronJob
metadata: {name: nightly}
spec:
  schedule: "0 0 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers: [{name: c, image: busybox}]
---
apiVersion: argoproj.io/v1alpha1
kind: Rollout
metadata: {name: canary}
spec:
  template:
    spec:
      containers: [{name: c, image: busybox}]
---
# A kind with a job template that is not a CronJob.
apiVersion: example.test/v1
kind: Schedule
metadata: {name: later}
spec:
  jobTemplate:
    spec:
      template:
        spec:
          containers: [{name: c, image: busybox}]
---
apiVersion: batch/v1
kind: Job
metadata: {generateName: run-}
spec:
  template:
    spec:
      restartPolicy: Never
      containers: [{name: c, image: busybox}]
---
# A typed list: its items name no kind and no apiVersion.
apiVersion: apps/v1
kind: StatefulSetList
items:
  - metadata: {name: db}
    spec:
      template:
        spec:
          containers: [{name: c, image: busybox}]
`
	res, err := FromManifests([]ManifestInput{{Name: "all.yaml", Data: []byte(text)}}, ManifestOptions{DefaultNamespace: "from-flag"})
	if err != nil {
		t.Fatalf("FromManifests: %v", err)
	}
	if len(res.Written) != len(res.Workloads) {
		t.Fatalf("Written has %d entries for %d workloads", len(res.Written), len(res.Workloads))
	}
	template := []string{"spec", "template", "spec"}
	jobTemplate := []string{"spec", "jobTemplate", "spec", "template", "spec"}
	want := map[string]ManifestObject{
		"Deployment/web":              {APIVersion: "apps/v1", Kind: "Deployment", Name: "web", Namespace: "shop", PodSpecPath: template},
		"Pod/once":                    {APIVersion: "v1", Kind: "Pod", Name: "once", PodSpecPath: []string{"spec"}},
		"CronJob/nightly":             {APIVersion: "batch/v1", Kind: "CronJob", Name: "nightly", PodSpecPath: jobTemplate},
		"Rollout.argoproj.io/canary":  {APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "canary", PodSpecPath: template},
		"Schedule.example.test/later": {APIVersion: "example.test/v1", Kind: "Schedule", Name: "later", PodSpecPath: jobTemplate},
		// The workload has the name "run-" (from generateName) and the
		// namespace of the flag. The text has no name and no namespace.
		"Job/run-": {APIVersion: "batch/v1", Kind: "Job", PodSpecPath: template},
		// The workload has the kind of the list. The item names none.
		"StatefulSet/db": {Name: "db", PodSpecPath: template},
	}
	for i, w := range res.Workloads {
		key := w.Kind + "/" + w.Name
		wantObject, ok := want[key]
		if !ok {
			t.Errorf("a workload that the test does not expect: %s", key)
			continue
		}
		if !reflect.DeepEqual(res.Written[i], wantObject) {
			t.Errorf("%s: Written = %+v, want %+v", key, res.Written[i], wantObject)
		}
		delete(want, key)
	}
	for key := range want {
		t.Errorf("no workload %s in the result", key)
	}

	// A caller can change a path that it got: each object has its own.
	res.Written[0].PodSpecPath[0] = "changed"
	again, err := FromManifests([]ManifestInput{{Name: "all.yaml", Data: []byte(text)}}, ManifestOptions{})
	if err != nil {
		t.Fatalf("FromManifests: %v", err)
	}
	for _, o := range again.Written {
		if o.PodSpecPath[0] != "spec" {
			t.Errorf("a path is shared between results: %v", o.PodSpecPath)
		}
	}
}
