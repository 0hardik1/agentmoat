// Tests for the edit. The targets of each test come from the scanner
// (targetsOf), as in production: the position of an object is the one that
// FromManifests gives, and the test does not make it up.
//
// The expected text is written by hand for the common shapes, so that the
// reader sees the edit. For the other shapes the test builds it from the
// input (addAfter): the claim is always "the input, plus this one line after
// that line, and not one other byte".
package patcher

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/pkg/scanner"
)

// targetsOf scans text and returns one target for each workload, sorted as
// the scanner sorts (namespace, kind, name).
func targetsOf(t *testing.T, text string) []Target {
	t.Helper()
	found, err := scanner.FromManifests([]scanner.ManifestInput{{Name: "test.yaml", Data: []byte(text)}}, scanner.ManifestOptions{})
	if err != nil {
		t.Fatalf("FromManifests: %v", err)
	}
	out := make([]Target, 0, len(found.Workloads))
	for i, w := range found.Workloads {
		out = append(out, Target{Document: found.Origins[i].Document, Object: found.Origins[i].Object, Kind: w.Kind})
	}
	return out
}

// addAfter returns text with one line added after the line that contains
// marker (which must be in the text one time). The new line gets the line
// ending of the marker line.
func addAfter(t *testing.T, text, marker, line string) string {
	t.Helper()
	if strings.Count(text, marker) != 1 {
		t.Fatalf("marker %q is in the text %d times, want 1", marker, strings.Count(text, marker))
	}
	at := strings.Index(text, marker)
	end := at + strings.Index(text[at:], "\n") + 1
	ending := "\n"
	if strings.HasSuffix(text[:end], "\r\n") {
		ending = "\r\n"
	}
	return text[:end] + line + ending + text[end:]
}

const deployment = `# The web front end.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  replicas: 2
  template:
    metadata:
      labels: {app: web}
    spec:
      # The one container.
      containers:
        - name: web
          image: "nginx:1.27"   # quoted on purpose
`

func TestAddRuntimeClass_AddsOneLine(t *testing.T) {
	t.Parallel()
	const want = `# The web front end.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: shop
spec:
  replicas: 2
  template:
    metadata:
      labels: {app: web}
    spec:
      runtimeClassName: gvisor
      # The one container.
      containers:
        - name: web
          image: "nginx:1.27"   # quoted on purpose
`
	res := AddRuntimeClass([]byte(deployment), "gvisor", targetsOf(t, deployment))
	if got := string(res.Text); got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
	if len(res.Outcomes) != 1 || !res.Outcomes[0].Edited || res.Outcomes[0].Line != 13 || res.Outcomes[0].Reason != "" {
		t.Errorf("outcomes = %+v, want one edit on line 13", res.Outcomes)
	}
	if len(res.Added) != 1 || res.Added[0] != 13 {
		t.Errorf("added = %v, want [13]", res.Added)
	}
}

// TestAddRuntimeClass_Shapes: the place and the indent for each kind and
// each layout. Each case is the input plus one line.
func TestAddRuntimeClass_Shapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		text   string
		marker string // the line that the new line follows
		line   string // the new line, with its indent
	}{
		{
			name:   "a Pod has its pod spec at spec",
			text:   "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n    - {name: c, image: busybox}\n",
			marker: "spec:", line: "  runtimeClassName: gvisor",
		},
		{
			name:   "a CronJob has it under jobTemplate",
			text:   "apiVersion: batch/v1\nkind: CronJob\nmetadata:\n  name: nightly\nspec:\n  schedule: \"0 3 * * *\"\n  jobTemplate:\n    spec:\n      template:\n        spec: # the pod\n          restartPolicy: Never\n          containers:\n            - {name: c, image: busybox}\n",
			marker: "spec: # the pod", line: "          runtimeClassName: gvisor",
		},
		{
			name:   "an indent of four spaces",
			text:   "apiVersion: apps/v1\nkind: StatefulSet\nmetadata:\n    name: db\nspec:\n    template:\n        spec:\n            containers:\n                - name: db\n                  image: postgres\n",
			marker: "        spec:", line: "            runtimeClassName: gvisor",
		},
		{
			name:   "a blank line and a comment after the key",
			text:   "apiVersion: apps/v1\nkind: DaemonSet\nmetadata:\n  name: agent\nspec:\n  template:\n    spec:\n\n      # tolerate every taint\n      tolerations: [{operator: Exists}]\n      containers:\n        - {name: c, image: busybox}\n",
			marker: "    spec:", line: "      runtimeClassName: gvisor",
		},
		{
			name:   "a kind that the scanner has no type for (Argo Rollout)",
			text:   "apiVersion: argoproj.io/v1alpha1\nkind: Rollout\nmetadata:\n  name: web\nspec:\n  strategy: {canary: {}}\n  template:\n    spec:\n      containers:\n        - {name: c, image: busybox}\n",
			marker: "    spec:", line: "      runtimeClassName: gvisor",
		},
		{
			name:   "another kind with a jobTemplate and no template",
			text:   "apiVersion: example.io/v1\nkind: Nightly\nmetadata:\n  name: nightly\nspec:\n  jobTemplate:\n    spec:\n      template:\n        spec:\n          containers:\n            - {name: c, image: busybox}\n",
			marker: "        spec:", line: "          runtimeClassName: gvisor",
		},
		{
			name:   "CRLF line endings stay",
			text:   "apiVersion: v1\r\nkind: Pod\r\nmetadata:\r\n  name: p\r\nspec:\r\n  containers:\r\n    - {name: c, image: busybox}\r\n",
			marker: "spec:", line: "  runtimeClassName: gvisor",
		},
		{
			name:   "a byte order mark stays",
			text:   "\xEF\xBB\xBFapiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n    - {name: c, image: busybox}\n",
			marker: "spec:", line: "  runtimeClassName: gvisor",
		},
		{
			name:   "a last line with no line ending stays as it is",
			text:   "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n    - {name: c, image: busybox}",
			marker: "spec:", line: "  runtimeClassName: gvisor",
		},
		{
			name:   "a pod spec in a list item with the compact form",
			text:   "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Pod\n  metadata: {name: p}\n  spec:\n    containers:\n    - {name: c, image: busybox}\n",
			marker: "  spec:", line: "    runtimeClassName: gvisor",
		},
		{
			name:   "a merge key in the pod spec: the new key is a normal key next to it",
			text:   "apiVersion: v1\nkind: Pod\nx-defaults: &d {restartPolicy: Never}\nmetadata:\n  name: p\nspec:\n  <<: *d\n  containers:\n    - {name: c, image: busybox}\n",
			marker: "spec:", line: "  runtimeClassName: gvisor",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			targets := targetsOf(t, tc.text)
			if len(targets) != 1 {
				t.Fatalf("the fixture has %d workloads, want 1", len(targets))
			}
			res := AddRuntimeClass([]byte(tc.text), "gvisor", targets)
			want := addAfter(t, tc.text, tc.marker, tc.line)
			if got := string(res.Text); got != want {
				t.Errorf("text =\n%q\nwant\n%q\noutcomes: %+v", got, want, res.Outcomes)
			}
			if !res.Outcomes[0].Edited {
				t.Errorf("not edited: %s", res.Outcomes[0].Reason)
			}
		})
	}
}

// TestAddRuntimeClass_ManyObjects: targets in more than one document and in
// a List, with objects that are not targets between them. Each target is
// found by its position, and the lines of the result are right although
// each added line moves the ones below it.
func TestAddRuntimeClass_ManyObjects(t *testing.T) {
	t.Parallel()
	const text = `# A file with no object in its first document.
---
apiVersion: v1
kind: ConfigMap
metadata: {name: settings, namespace: shop}
data: {spec: "not a pod spec"}
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: api, namespace: shop}
spec:
  template:
    spec: # api
      containers:
        - {name: c, image: busybox}
---
apiVersion: v1
kind: List
items:
  - apiVersion: v1
    kind: Service
    metadata: {name: web, namespace: shop}
    spec: {ports: [{port: 80}]}
  - apiVersion: v1
    kind: Pod
    metadata: {name: one, namespace: shop}
    spec: # one
      containers:
        - {name: c, image: busybox}
  - apiVersion: v1
    kind: Pod
    metadata: {name: two, namespace: shop}
    spec: # two
      containers:
        - {name: c, image: busybox}
`
	targets := targetsOf(t, text) // sorted: Deployment api, Pod one, Pod two
	if len(targets) != 3 {
		t.Fatalf("targets = %+v, want 3", targets)
	}
	// Leave "one" out: only the first and the last are edited.
	picked := []Target{targets[2], targets[0]}
	res := AddRuntimeClass([]byte(text), "gvisor", picked)

	want := addAfter(t, text, "spec: # api", "      runtimeClassName: gvisor")
	want = addAfter(t, want, "spec: # two", "      runtimeClassName: gvisor")
	if got := string(res.Text); got != want {
		t.Fatalf("text =\n%s\nwant\n%s\noutcomes: %+v", got, want, res.Outcomes)
	}
	// Outcomes are parallel to the targets as given: "two" first.
	if res.Outcomes[0].Line != 35 || res.Outcomes[1].Line != 14 {
		t.Errorf("lines = %d and %d, want 35 (two) and 14 (api)", res.Outcomes[0].Line, res.Outcomes[1].Line)
	}
	if len(res.Added) != 2 || res.Added[0] != 14 || res.Added[1] != 35 {
		t.Errorf("added = %v, want [14 35]", res.Added)
	}
	lines := strings.Split(string(res.Text), "\n")
	for _, n := range res.Added {
		if got := strings.TrimSpace(lines[n-1]); got != "runtimeClassName: gvisor" {
			t.Errorf("line %d = %q, want the added line", n, got)
		}
	}
}

// TestAddRuntimeClass_NotEdited: the forms that the package does not edit.
// The text comes back byte for byte, with a reason that says what to do.
func TestAddRuntimeClass_NotEdited(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		text       string
		wantReason string
	}{
		{
			name:       "JSON",
			text:       `{"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "p"}, "spec": {"containers": [{"name": "c", "image": "busybox"}]}}` + "\n",
			wantReason: "JSON is not edited",
		},
		{
			name:       "a pod spec in flow style",
			text:       "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec: {containers: [{name: c, image: busybox}]}\n",
			wantReason: "flow-style YAML",
		},
		{
			name:       "an object in flow style",
			text:       "{apiVersion: v1, kind: Pod, metadata: {name: p}, spec: {containers: [{name: c, image: busybox}]}}\n",
			wantReason: "flow-style YAML",
		},
		{
			name:       "an anchor on the pod template: another object can share it",
			text:       "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\nspec:\n  template: &tpl\n    spec:\n      containers:\n        - {name: c, image: busybox}\n",
			wantReason: "anchor or alias",
		},
		{
			name:       "an anchor on the pod spec",
			text:       "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec: &podspec\n  containers:\n    - {name: c, image: busybox}\n",
			wantReason: "anchor or alias",
		},
		{
			name:       "a runtimeClassName key with no value",
			text:       "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  runtimeClassName:\n  containers:\n    - {name: c, image: busybox}\n",
			wantReason: "already has a runtimeClassName key",
		},
		{
			name:       "a runtimeClassName key with an empty string",
			text:       "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n    - {name: c, image: busybox}\n  runtimeClassName: \"\"\n",
			wantReason: "already has a runtimeClassName key",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			targets := targetsOf(t, tc.text)
			if len(targets) != 1 {
				t.Fatalf("the fixture has %d workloads, want 1", len(targets))
			}
			res := AddRuntimeClass([]byte(tc.text), "gvisor", targets)
			if string(res.Text) != tc.text {
				t.Errorf("the text changed:\n%q", res.Text)
			}
			o := res.Outcomes[0]
			if o.Edited || o.Line != 0 || !strings.Contains(o.Reason, tc.wantReason) {
				t.Errorf("outcome = %+v, want not edited with a reason that has %q", o, tc.wantReason)
			}
			if len(res.Added) != 0 {
				t.Errorf("added = %v, want none", res.Added)
			}
		})
	}
}

// TestAddRuntimeClass_SharedByAnAlias: two Deployments share one pod
// template through an anchor and an alias. Neither is edited: a line under
// the anchor would change the two, and the caller asked for a change to one
// object at a time.
func TestAddRuntimeClass_SharedByAnAlias(t *testing.T) {
	t.Parallel()
	const text = `apiVersion: apps/v1
kind: Deployment
metadata: {name: a}
spec:
  template: &tpl
    spec:
      containers:
        - {name: c, image: busybox}
---
apiVersion: v1
kind: List
items:
  - &shared
    apiVersion: v1
    kind: Pod
    metadata: {name: b}
    spec:
      containers:
        - {name: c, image: busybox}
  - *shared
`
	targets := targetsOf(t, text)
	if len(targets) != 3 {
		t.Fatalf("targets = %+v, want 3 (the Deployment, the Pod, and its alias)", targets)
	}
	res := AddRuntimeClass([]byte(text), "gvisor", targets)
	if string(res.Text) != text {
		t.Errorf("the text changed:\n%s", res.Text)
	}
	for _, o := range res.Outcomes {
		if o.Edited || !strings.Contains(o.Reason, "anchor or alias") {
			t.Errorf("outcome = %+v, want not edited because of the anchor", o)
		}
	}
}

// TestAddRuntimeClass_SomeEditedSomeNot: one file with a target that can be
// edited and one that cannot. The first gets its line; the second gets its
// reason.
func TestAddRuntimeClass_SomeEditedSomeNot(t *testing.T) {
	t.Parallel()
	const text = `apiVersion: v1
kind: Pod
metadata: {name: block}
spec: # block
  containers:
    - {name: c, image: busybox}
---
apiVersion: v1
kind: Pod
metadata: {name: flow}
spec: {containers: [{name: c, image: busybox}]}
`
	res := AddRuntimeClass([]byte(text), "gvisor", targetsOf(t, text)) // sorted: block, flow
	want := addAfter(t, text, "spec: # block", "  runtimeClassName: gvisor")
	if got := string(res.Text); got != want {
		t.Errorf("text =\n%s\nwant\n%s", got, want)
	}
	if !res.Outcomes[0].Edited || res.Outcomes[1].Edited || !strings.Contains(res.Outcomes[1].Reason, "flow-style") {
		t.Errorf("outcomes = %+v, want the first edited and the second not (flow style)", res.Outcomes)
	}
}

// TestAddRuntimeClass_ClassName: a name that YAML would read as a boolean or
// a number gets quotes, and a name that is not a valid RuntimeClass name
// (also one with a line break, which would add a line of its own) is
// refused.
func TestAddRuntimeClass_ClassName(t *testing.T) {
	t.Parallel()
	const pod = "apiVersion: v1\nkind: Pod\nmetadata:\n  name: p\nspec:\n  containers:\n    - {name: c, image: busybox}\n"
	for class, wantLine := range map[string]string{
		"gvisor":       "  runtimeClassName: gvisor",
		"kata-qemu.v2": "  runtimeClassName: kata-qemu.v2",
		"on":           `  runtimeClassName: "on"`,
		"123":          `  runtimeClassName: "123"`,
		"1e3":          `  runtimeClassName: "1e3"`,
		"null":         `  runtimeClassName: "null"`,
		"true":         `  runtimeClassName: "true"`,
		"y":            `  runtimeClassName: "y"`,
		"0x1f":         `  runtimeClassName: "0x1f"`,
		"1.5":          `  runtimeClassName: "1.5"`,
		"no":           `  runtimeClassName: "no"`,
		"sandbox-1":    "  runtimeClassName: sandbox-1",
	} {
		res := AddRuntimeClass([]byte(pod), class, targetsOf(t, pod))
		if want := addAfter(t, pod, "spec:", wantLine); string(res.Text) != want {
			t.Errorf("class %q: text =\n%s\nwant\n%s\noutcome: %+v", class, res.Text, want, res.Outcomes[0])
		}
	}

	for _, class := range []string{"", "Gvisor", "gvisor\n  hostNetwork: true", "a b", "gvisor # x", "-x", strings.Repeat("a", 254)} {
		if err := ValidClassName(class); err == nil {
			t.Errorf("ValidClassName(%q) = nil, want an error", class)
		}
		res := AddRuntimeClass([]byte(pod), class, targetsOf(t, pod))
		if string(res.Text) != pod || res.Outcomes[0].Edited || res.Outcomes[0].Reason == "" {
			t.Errorf("class %q: the text changed or there is no reason: %+v", class, res.Outcomes[0])
		}
	}
}

// TestAddRuntimeClass_ATargetThatIsNotThere: a position that the text does
// not have is a reason, not a panic.
func TestAddRuntimeClass_ATargetThatIsNotThere(t *testing.T) {
	t.Parallel()
	for _, target := range []Target{
		{Document: 9, Object: 1, Kind: "Deployment"},
		{Document: 2, Object: 4, Kind: "Deployment"},
		{Document: 2, Object: 0, Kind: "Deployment"},
		{Document: 2, Object: 1, Kind: "CronJob"}, // the object is there; the path of a CronJob is not
	} {
		res := AddRuntimeClass([]byte(deployment), "gvisor", []Target{target})
		if string(res.Text) != deployment || res.Outcomes[0].Edited || res.Outcomes[0].Reason == "" {
			t.Errorf("target %+v: outcome = %+v, want not edited with a reason", target, res.Outcomes[0])
		}
	}
	// The header comment of the fixture is not a document of its own, so
	// the Deployment is document 1, object 1.
	if res := AddRuntimeClass([]byte(deployment), "gvisor", []Target{{Document: 1, Object: 1, Kind: "Deployment"}}); !res.Outcomes[0].Edited {
		t.Errorf("document 1, object 1: %+v, want edited", res.Outcomes[0])
	}
}

// TestCheck_RefusesAWrongEdit: the proof of an edit, given edits that are
// wrong. AddRuntimeClass never makes them; the check is there for the day
// when it does, so each one is made by hand here.
func TestCheck_RefusesAWrongEdit(t *testing.T) {
	t.Parallel()
	target := targetsOf(t, deployment)
	right := AddRuntimeClass([]byte(deployment), "gvisor", target).Text
	if err := check([]byte(deployment), right, "gvisor", target); err != nil {
		t.Fatalf("the right edit does not pass: %v", err)
	}

	cases := []struct {
		name    string
		newText string
		want    string
	}{
		{
			name:    "the line is in the spec of the Deployment, not in the pod spec",
			newText: addAfter(t, deployment, "\nspec:", "  runtimeClassName: gvisor"),
			want:    "has runtimeClassName \"\" in the new text",
		},
		{
			name:    "the line is right, and another value changed",
			newText: strings.Replace(string(right), "replicas: 2", "replicas: 3", 1),
			want:    ".spec.replicas changed",
		},
		{
			name:    "the line is right, and a key went away",
			newText: strings.Replace(string(right), "  replicas: 2\n", "", 1),
			want:    ".spec.replicas is gone",
		},
		{
			name:    "another key is new",
			newText: addAfter(t, string(right), "      runtimeClassName: gvisor", "      hostNetwork: true"),
			want:    ".spec.template.spec.hostNetwork is new",
		},
		{
			name:    "the class is another one",
			newText: strings.Replace(string(right), "runtimeClassName: gvisor", "runtimeClassName: kata", 1),
			want:    ".spec.template.spec.runtimeClassName is new",
		},
		{
			name:    "a document went away",
			newText: "# nothing\n",
			want:    "had 1 documents and now has 0",
		},
		{
			name:    "a document is new",
			newText: string(right) + "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: extra}\n",
			want:    "had 1 documents and now has 2",
		},
		{
			name:    "no line was added",
			newText: deployment,
			want:    "0 runtimeClassName keys are new, for 1 edits",
		},
		{
			name:    "the new text is not YAML",
			newText: strings.Replace(string(right), "      runtimeClassName: gvisor", "   runtimeClassName: gvisor", 1),
			want:    "after the edit",
		},
	}
	for _, tc := range cases {
		err := check([]byte(deployment), []byte(tc.newText), "gvisor", target)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: check = %v, want an error that has %q", tc.name, err, tc.want)
		}
	}
}
