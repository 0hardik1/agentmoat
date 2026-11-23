// Tests for the SARIF renderer.
//
// The consumer that matters most is GitHub code scanning, and it is strict
// in a few places that are easy to get wrong: arrays must be arrays and not
// null, a rule needs text in three properties, and a result with the level
// "error" fails a pull-request check. The tests are named for those rules.

package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/docs"
	"github.com/0hardik1/agentmoat/internal/schema"
)

// renderSARIF renders the report as SARIF and decodes it again, so the
// tests read what a consumer reads. It also returns the raw text.
func renderSARIF(t *testing.T, report *schema.ScanReport, gate *Gate) (sarifLog, string) {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(report, FormatSARIF, &buf, RenderOptions{Gate: gate}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	var log sarifLog
	if err := json.Unmarshal(buf.Bytes(), &log); err != nil {
		t.Fatalf("the SARIF output is not JSON: %v\n%s", err, buf.String())
	}
	return log, buf.String()
}

// sarifWorkload builds a report row with one reason for each severity given.
func sarifWorkload(name, runtimeClass string, verdict schema.Compatibility, severities ...schema.Severity) schema.WorkloadResult {
	w := schema.WorkloadResult{
		Kind: "Deployment", Namespace: "edge", Name: name,
		RuntimeClassName: runtimeClass, Compatibility: verdict,
		Origin: &schema.Origin{File: "k8s/" + name + ".yaml", Line: 3},
	}
	ruleFor := map[schema.Severity]string{
		schema.SeverityError: "host-network",
		schema.SeverityWarn:  "host-path-mount",
		schema.SeverityInfo:  "network-throughput",
	}
	for _, sev := range severities {
		w.Reasons = append(w.Reasons, schema.Reason{
			RuleID: ruleFor[sev], Severity: sev, Description: "Why " + ruleFor[sev] + " matched.",
		})
	}
	return w
}

func sarifReport(rows ...schema.WorkloadResult) *schema.ScanReport {
	r := schema.NewScanReport()
	r.Metadata.AgentmoatVersion = "v0.3.0"
	r.Spec.Workloads = rows
	return r
}

// levels returns "name/rule=level" for each result, in order.
func levels(log sarifLog) []string {
	var out []string
	for _, r := range log.Runs[0].Results {
		out = append(out, r.Properties.Name+"/"+r.RuleID+"="+r.Level)
	}
	return out
}

func TestSARIF_Envelope(t *testing.T) {
	t.Parallel()
	log, raw := renderSARIF(t, sarifReport(
		sarifWorkload("gateway", "gvisor", schema.CompatibilityIncompatible, schema.SeverityError),
	), nil)

	if log.Schema != "https://json.schemastore.org/sarif-2.1.0.json" || log.Version != "2.1.0" {
		t.Errorf("$schema = %q, version = %q", log.Schema, log.Version)
	}
	if len(log.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(log.Runs))
	}
	driver := log.Runs[0].Tool.Driver
	if driver.Name != "agentmoat" || driver.InformationURI != "https://github.com/0hardik1/agentmoat" {
		t.Errorf("driver = %+v", driver)
	}
	if driver.SemanticVersion != "0.3.0" || driver.Version != "" {
		t.Errorf("semanticVersion = %q, version = %q; want 0.3.0 and none", driver.SemanticVersion, driver.Version)
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Errorf("the output must end with a newline")
	}

	res := log.Runs[0].Results[0]
	if res.Message.Text != "Deployment edge/gateway: Why host-network matched." {
		t.Errorf("message = %q", res.Message.Text)
	}
	wantProps := sarifResultProperties{
		Kind: "Deployment", Namespace: "edge", Name: "gateway", RuntimeClassName: "gvisor",
		Compatibility: "incompatible", Severity: "error",
	}
	if res.Properties != wantProps {
		t.Errorf("properties = %+v, want %+v", res.Properties, wantProps)
	}
}

// TestSARIF_NoFindingsIsEmptyArrays: the usual passing run has no results.
// GitHub rejects a run whose "results" is missing or null, so both arrays
// must be written as [].
func TestSARIF_NoFindingsIsEmptyArrays(t *testing.T) {
	t.Parallel()
	reports := map[string]*schema.ScanReport{
		"no workloads":         sarifReport(),
		"a compatible one":     sarifReport(sarifWorkload("web", "", schema.CompatibilityCompatible)),
		"info reasons only":    sarifReport(sarifWorkload("web", "", schema.CompatibilityCompatible, schema.SeverityInfo)),
		"a nil workload slice": schema.NewScanReport(),
	}
	for name, report := range reports {
		_, raw := renderSARIF(t, report, nil)
		for _, want := range []string{`"results": []`, `"rules": []`} {
			if !strings.Contains(raw, want) {
				t.Errorf("%s: the output lacks %s:\n%s", name, want, raw)
			}
		}
		if strings.Contains(raw, "null") {
			t.Errorf("%s: the output has a null:\n%s", name, raw)
		}
	}
}

// TestSARIF_LevelFollowsTheGate: a result is "error" only when its workload
// makes the gate fail. GitHub fails a pull-request check on a new "error"
// result, so a level that does not follow the gate is a second gate that
// does not agree with the exit code.
func TestSARIF_LevelFollowsTheGate(t *testing.T) {
	t.Parallel()
	const (
		bad = schema.CompatibilityIncompatible
		rev = schema.CompatibilityReview
		ok  = schema.CompatibilityCompatible
	)
	report := sarifReport(
		// A CNI DaemonSet in spirit: incompatible, not in the sandbox.
		sarifWorkload("cni", "", bad, schema.SeverityError, schema.SeverityWarn),
		// Incompatible and in the sandbox.
		sarifWorkload("gateway", "gvisor", bad, schema.SeverityError),
		sarifWorkload("store", "gvisor", rev, schema.SeverityWarn, schema.SeverityInfo),
		sarifWorkload("web", "gvisor", ok, schema.SeverityInfo),
	)
	gatewayOnly := []schema.WorkloadRef{{Kind: "Deployment", Namespace: "edge", Name: "gateway"}}

	tests := []struct {
		name string
		gate *Gate
		want []string
	}{
		{
			name: "no gate from the caller: the levels of the default gate",
			gate: nil,
			want: []string{
				"cni/host-network=error", "cni/host-path-mount=warning",
				"gateway/host-network=error", "store/host-path-mount=warning",
			},
		},
		{
			name: "--fail-on sandboxed: the workload outside the sandbox is a warning",
			gate: &Gate{FailOn: "sandboxed", Outcome: GateFailed, Failing: gatewayOnly},
			want: []string{
				"cni/host-network=warning", "cni/host-path-mount=warning",
				"gateway/host-network=error", "store/host-path-mount=warning",
			},
		},
		{
			name: "--fail-on none: nothing fails the gate, so nothing is an error",
			gate: &Gate{FailOn: "none", Outcome: GateOff},
			want: []string{
				"cni/host-network=warning", "cni/host-path-mount=warning",
				"gateway/host-network=warning", "store/host-path-mount=warning",
			},
		},
		{
			name: "a gate that refused has no failing list, so nothing is an error",
			gate: &Gate{FailOn: "sandboxed", Outcome: GateRefused},
			want: []string{
				"cni/host-network=warning", "cni/host-path-mount=warning",
				"gateway/host-network=warning", "store/host-path-mount=warning",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, _ := renderSARIF(t, report, tc.gate)
			if got := levels(log); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("levels =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}

// TestSARIF_FailingIsByIdentity: the gate reads two copies of one workload
// as one workload. Both copies get the level of that workload.
func TestSARIF_FailingIsByIdentity(t *testing.T) {
	t.Parallel()
	sandboxedCopy := sarifWorkload("web", "gvisor", schema.CompatibilityCompatible)
	brokenCopy := sarifWorkload("web", "", schema.CompatibilityIncompatible, schema.SeverityError)
	gate := &Gate{FailOn: "sandboxed", Outcome: GateFailed,
		Failing: []schema.WorkloadRef{{Kind: "Deployment", Namespace: "edge", Name: "web"}}}
	log, _ := renderSARIF(t, sarifReport(sandboxedCopy, brokenCopy), gate)
	if got, want := levels(log), []string{"web/host-network=error"}; !reflect.DeepEqual(got, want) {
		t.Errorf("levels = %v, want %v", got, want)
	}
}

// TestSARIF_RulesAreSortedAndIndexed: rules by ID, and each result points
// at its rule by index.
func TestSARIF_RulesAreSortedAndIndexed(t *testing.T) {
	t.Parallel()
	report := sarifReport(
		sarifWorkload("b", "", schema.CompatibilityIncompatible, schema.SeverityWarn, schema.SeverityError),
		sarifWorkload("a", "", schema.CompatibilityIncompatible, schema.SeverityError, schema.SeverityInfo),
	)
	log, _ := renderSARIF(t, report, nil)
	rules := log.Runs[0].Tool.Driver.Rules
	var ids []string
	for _, r := range rules {
		ids = append(ids, r.ID)
	}
	// network-throughput has only an info reason: no result, so no rule.
	if want := []string{"host-network", "host-path-mount"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("rule IDs = %v, want %v", ids, want)
	}
	for _, res := range log.Runs[0].Results {
		if res.RuleIndex < 0 || res.RuleIndex >= len(rules) || rules[res.RuleIndex].ID != res.RuleID {
			t.Errorf("result %s has ruleIndex %d, which is not its rule", res.RuleID, res.RuleIndex)
		}
	}
}

// TestSARIF_RuleTextIsTheSameInEveryReport: the classifier can add a
// sentence about one workload to a description. The rule entry must not
// change with it, or the rule would look edited on every upload.
func TestSARIF_RuleTextIsTheSameInEveryReport(t *testing.T) {
	t.Parallel()
	plain := sarifWorkload("a", "", schema.CompatibilityReview, schema.SeverityWarn)
	noted := sarifWorkload("b", "", schema.CompatibilityReview, schema.SeverityWarn)
	noted.Reasons[0].Description += " The request is for a MIG slice (nvidia.com/mig-1g.5gb)."

	first, _ := renderSARIF(t, sarifReport(plain), nil)
	second, _ := renderSARIF(t, sarifReport(noted), nil)
	if !reflect.DeepEqual(first.Runs[0].Tool.Driver.Rules, second.Runs[0].Tool.Driver.Rules) {
		t.Errorf("the rule entry changed with the description of a workload")
	}
	// The sentence about the workload is in the result message.
	if msg := second.Runs[0].Results[0].Message.Text; !strings.Contains(msg, "MIG slice") {
		t.Errorf("message = %q, want the sentence about the workload", msg)
	}
}

// TestSARIF_BuiltInRuleText: GitHub requires text in shortDescription,
// fullDescription, and help, and limits the first two to 1024 characters
// and the name to 255. Every built-in rule must fit.
func TestSARIF_BuiltInRuleText(t *testing.T) {
	t.Parallel()
	ids := docs.RuleIDs()
	if len(ids) < 15 {
		t.Fatalf("docs.RuleIDs() has %d rules, want 15 or more", len(ids))
	}
	for _, id := range ids {
		rule := sarifRuleFor(id)
		for _, problem := range ruleTextProblems(id, rule) {
			t.Errorf("%s: %s", id, problem)
		}
		if want := "https://github.com/0hardik1/agentmoat/blob/main/docs/explanations/" + id + ".md"; rule.HelpURI != want {
			t.Errorf("%s: helpUri = %q, want %q", id, rule.HelpURI, want)
		}
	}
}

// ruleTextProblems checks one rule entry against what GitHub code scanning
// requires, and returns one line for each thing that is wrong.
func ruleTextProblems(id string, rule sarifRule) []string {
	var problems []string
	bad := func(cond bool, format string, args ...any) {
		if cond {
			problems = append(problems, fmt.Sprintf(format, args...))
		}
	}
	size := func(s string) int { return len([]rune(s)) }

	bad(rule.ID != id, "id = %q", rule.ID)
	bad(size(rule.Name) == 0 || size(rule.Name) > 255 || strings.ContainsAny(rule.Name, "-_ "),
		"name = %q, want PascalCase of 255 characters or fewer", rule.Name)

	short := rule.ShortDescription.Text
	bad(size(short) == 0 || size(short) > 1024 || short == id, "shortDescription = %q, want a title", short)

	full := rule.FullDescription.Text
	bad(size(full) == 0 || size(full) > 1024, "fullDescription is %d characters, want 1 to 1024", size(full))
	bad(strings.HasPrefix(full, "#") || strings.Contains(full, "\n"),
		"fullDescription must be one line of prose, not a heading: %q", full)

	bad(rule.Help.Text == "" || rule.Help.Markdown == "", "help text is empty")
	bad(len(rule.Properties.Tags) == 0, "no tags")
	return problems
}

// TestSARIF_UnknownRule: a report from a newer release can name a rule that
// this build has no text for. The required properties are still not empty.
func TestSARIF_UnknownRule(t *testing.T) {
	t.Parallel()
	rule := sarifRuleFor("rule-from-the-future")
	if rule.Name != "RuleFromTheFuture" || rule.ShortDescription.Text == "" || rule.FullDescription.Text == "" || rule.Help.Text == "" {
		t.Errorf("rule = %+v, want text in every required property", rule)
	}
	if rule.HelpURI != "" {
		t.Errorf("helpUri = %q, want none: there is no explanation file to link to", rule.HelpURI)
	}
}

func TestSARIF_Locations(t *testing.T) {
	t.Parallel()
	row := func(origin *schema.Origin) schema.WorkloadResult {
		w := sarifWorkload("gateway", "", schema.CompatibilityIncompatible, schema.SeverityError)
		w.Origin = origin
		return w
	}
	logical := []sarifLogicalLocation{{Name: "gateway", FullyQualifiedName: "edge/Deployment/gateway", Kind: "resource"}}

	tests := []struct {
		name   string
		origin *schema.Origin
		want   sarifLocation
	}{
		{
			name:   "a file and a line",
			origin: &schema.Origin{File: "k8s/gateway.yaml", Line: 12},
			want: sarifLocation{
				PhysicalLocation: &sarifPhysicalLocation{
					ArtifactLocation: sarifArtifactLocation{URI: "k8s/gateway.yaml", URIBaseID: "%SRCROOT%"},
					Region:           &sarifRegion{StartLine: 12},
				},
				LogicalLocations: logical,
			},
		},
		{
			// A Helm "# Source:" path from standard input: the file is
			// known, the line is not. No region, which means the whole
			// file. A made-up line 1 would be a claim the report cannot
			// support.
			name:   "a file and no line",
			origin: &schema.Origin{File: "chart/templates/gateway.yaml"},
			want: sarifLocation{
				PhysicalLocation: &sarifPhysicalLocation{
					ArtifactLocation: sarifArtifactLocation{URI: "chart/templates/gateway.yaml", URIBaseID: "%SRCROOT%"},
				},
				LogicalLocations: logical,
			},
		},
		{
			name:   "standard input is not a file",
			origin: &schema.Origin{File: "<stdin>", Line: 4},
			want:   sarifLocation{LogicalLocations: logical},
		},
		{
			name:   "a cluster scan has no origin",
			origin: nil,
			want:   sarifLocation{LogicalLocations: logical},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log, _ := renderSARIF(t, sarifReport(row(tc.origin)), nil)
			locs := log.Runs[0].Results[0].Locations
			if len(locs) != 1 || !reflect.DeepEqual(locs[0], tc.want) {
				got, _ := json.Marshal(locs)
				want, _ := json.Marshal(tc.want)
				t.Errorf("locations = %s\nwant [%s]", got, want)
			}
		})
	}

	// A manifest that names no namespace: the qualified name has none.
	w := row(nil)
	w.Namespace = ""
	log, _ := renderSARIF(t, sarifReport(w), nil)
	if got := log.Runs[0].Results[0].Locations[0].LogicalLocations[0].FullyQualifiedName; got != "Deployment/gateway" {
		t.Errorf("fullyQualifiedName = %q, want Deployment/gateway", got)
	}
	if got := log.Runs[0].Results[0].Message.Text; !strings.HasPrefix(got, "Deployment gateway: ") {
		t.Errorf("message = %q, want it to start with the kind and the name", got)
	}
}

func TestSarifURI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		file     string
		wantURI  string
		wantBase string
		wantOK   bool
	}{
		{"k8s/web.yaml", "k8s/web.yaml", "%SRCROOT%", true},
		{"./k8s/web.yaml", "k8s/web.yaml", "%SRCROOT%", true},
		{"k8s//sub/../web.yaml", "k8s/web.yaml", "%SRCROOT%", true},
		{"../shared/web.yaml", "../shared/web.yaml", "%SRCROOT%", true},
		{"my manifests/web #1.yaml", "my%20manifests/web%20%231.yaml", "%SRCROOT%", true},
		{"a:b/web.yaml", "a%3Ab/web.yaml", "%SRCROOT%", true},
		{"/work/repo/k8s/web.yaml", "file:///work/repo/k8s/web.yaml", "", true},
		{"/work/my repo/web.yaml", "file:///work/my%20repo/web.yaml", "", true},
		{"C:/repo/web.yaml", "file:///C:/repo/web.yaml", "", true},
		{"<stdin>", "", "", false},
		{"", "", "", false},
	}
	for _, tc := range tests {
		uri, base, ok := sarifURI(&schema.Origin{File: tc.file})
		if uri != tc.wantURI || base != tc.wantBase || ok != tc.wantOK {
			t.Errorf("sarifURI(%q) = %q, %q, %v; want %q, %q, %v", tc.file, uri, base, ok, tc.wantURI, tc.wantBase, tc.wantOK)
		}
	}
	if _, _, ok := sarifURI(nil); ok {
		t.Errorf("sarifURI(nil) must not be ok")
	}
}

func TestSarifVersions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, wantVersion, wantSemantic string
	}{
		{"v0.3.0", "", "0.3.0"},
		{"0.3.0", "", "0.3.0"},
		{"v1.2.3-rc.1+build.5", "", "1.2.3-rc.1+build.5"},
		// A build from source. "dev" is not a semantic version.
		{"dev", "dev", ""},
		{"v0.3", "v0.3", ""},
		{"", "", ""},
	}
	for _, tc := range tests {
		version, semantic := sarifVersions(tc.in)
		if version != tc.wantVersion || semantic != tc.wantSemantic {
			t.Errorf("sarifVersions(%q) = %q, %q; want %q, %q", tc.in, version, semantic, tc.wantVersion, tc.wantSemantic)
		}
	}
}

// TestSARIF_Fingerprint: the fingerprint is the finding (the rule and the
// workload). It must not change when the file is edited, moved, or the
// description gets another sentence.
func TestSARIF_Fingerprint(t *testing.T) {
	t.Parallel()
	base := sarifWorkload("gateway", "", schema.CompatibilityIncompatible, schema.SeverityError)
	fingerprint := func(w schema.WorkloadResult) string {
		log, _ := renderSARIF(t, sarifReport(w), nil)
		fps := log.Runs[0].Results[0].PartialFingerprints
		if len(fps) != 1 || fps["agentmoatFinding/v1"] == "" {
			t.Fatalf("partialFingerprints = %v, want the one key agentmoatFinding/v1", fps)
		}
		return fps["agentmoatFinding/v1"]
	}
	want := fingerprint(base)
	if len(want) != 32 {
		t.Errorf("fingerprint = %q, want 32 hex characters", want)
	}

	same := base
	same.Origin = &schema.Origin{File: "moved/elsewhere.yaml", Line: 400}
	same.Reasons = []schema.Reason{{RuleID: "host-network", Severity: schema.SeverityError, Description: "Other words."}}
	same.RuntimeClassName = "gvisor"
	if got := fingerprint(same); got != want {
		t.Errorf("the fingerprint changed with the file, the line, or the description")
	}

	otherName := base
	otherName.Name = "proxy"
	otherNamespace := base
	otherNamespace.Namespace = "core"
	otherRule := base
	otherRule.Reasons = []schema.Reason{{RuleID: "privileged", Severity: schema.SeverityError, Description: "x"}}
	for name, w := range map[string]schema.WorkloadResult{"name": otherName, "namespace": otherNamespace, "rule": otherRule} {
		if got := fingerprint(w); got == want {
			t.Errorf("another %s must give another fingerprint", name)
		}
	}
}

// TestSARIF_HostileText: SARIF reads "[text](target)" in a message as a
// link. A name from a manifest must not put one in the alert.
func TestSARIF_HostileText(t *testing.T) {
	t.Parallel()
	w := sarifWorkload("x", "", schema.CompatibilityIncompatible, schema.SeverityError)
	w.Name = "a[click](http://evil.test)\nb"
	w.Reasons[0].Description = "why [here](http://evil.test)"
	log, _ := renderSARIF(t, sarifReport(w), nil)
	res := log.Runs[0].Results[0]

	want := `Deployment edge/a\[click\](http://evil.test) b: why \[here\](http://evil.test)`
	if res.Message.Text != want {
		t.Errorf("message = %q\nwant      %q", res.Message.Text, want)
	}
	if name := res.Locations[0].LogicalLocations[0].Name; strings.ContainsAny(name, "\n\r") {
		t.Errorf("the logical name has a newline: %q", name)
	}
	// The properties are data, not text for display: the name is as it
	// is in the report, so a tool can match it.
	if res.Properties.Name != w.Name {
		t.Errorf("properties.name = %q, want the name as it is in the report", res.Properties.Name)
	}
}

func TestSARIFLocations(t *testing.T) {
	t.Parallel()
	fromFile := sarifWorkload("a", "", schema.CompatibilityIncompatible, schema.SeverityError, schema.SeverityWarn, schema.SeverityInfo)
	fromStdin := sarifWorkload("b", "", schema.CompatibilityIncompatible, schema.SeverityError)
	fromStdin.Origin = &schema.Origin{File: "<stdin>", Line: 9}
	fromCluster := sarifWorkload("c", "", schema.CompatibilityReview, schema.SeverityWarn)
	fromCluster.Origin = nil
	compatible := sarifWorkload("d", "", schema.CompatibilityCompatible, schema.SeverityInfo)
	compatible.Origin = nil

	results, withoutFile := SARIFLocations(sarifReport(fromFile, fromStdin, fromCluster, compatible))
	// a: error and warn (2). b: error (1). c: warn (1). d: info only (0).
	if results != 4 || withoutFile != 2 {
		t.Errorf("SARIFLocations = %d results, %d with no file; want 4 and 2", results, withoutFile)
	}

	// The count is the number of results that Render writes.
	log, _ := renderSARIF(t, sarifReport(fromFile, fromStdin, fromCluster, compatible), nil)
	if len(log.Runs[0].Results) != results {
		t.Errorf("Render wrote %d results, SARIFLocations counted %d", len(log.Runs[0].Results), results)
	}
}

func TestFirstParagraph(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"## Why\n\nFirst line\nsecond line.\n\nNext paragraph.", "First line second line."},
		{"No heading here.\nStill the first.\n", "No heading here. Still the first."},
		{"\n\n# A\n## B\n\nText.", "Text."},
		{"## Only a heading\n", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := firstParagraph(tc.in); got != tc.want {
			t.Errorf("firstParagraph(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
