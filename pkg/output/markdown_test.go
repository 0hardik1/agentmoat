// Tests for the Markdown renderer.
//
// Two things matter more here than the exact layout:
//
//   - The gate result is in the report, first, for every outcome. A job
//     summary shows no stderr, and a report with no gate line reads as a
//     pass.
//   - Text from a manifest cannot change the structure of the report. The
//     hostile-manifest test puts Markdown, HTML, and newlines in every
//     string a pull request controls, then checks the output the way a
//     Markdown reader would see it.

package output

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// renderMarkdown renders doc as Markdown with the given gate.
//
// With AGENTMOAT_MD_DUMP set to a directory, it also writes each document to
// a file there, named for the test. That is how to look at the output (the
// hostile documents too) in a real Markdown renderer: these tests check the
// text with a small model of Markdown, and a renderer is the ground truth.
func renderMarkdown(t *testing.T, doc any, gate *Gate) string {
	t.Helper()
	var buf bytes.Buffer
	if err := Render(doc, FormatMarkdown, &buf, RenderOptions{Gate: gate}); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if dir := os.Getenv("AGENTMOAT_MD_DUMP"); dir != "" {
		dumpMarkdown(t, dir, buf.Bytes())
	}
	return buf.String()
}

// dumpMarkdown writes one rendered document into dir. os.Root keeps the
// write inside that directory, whatever the name of the test is.
func dumpMarkdown(t *testing.T, dir string, doc []byte) {
	t.Helper()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatalf("AGENTMOAT_MD_DUMP: %v", err)
	}
	defer func() { _ = root.Close() }()
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	if err := root.WriteFile(fmt.Sprintf("%s-%03d.md", name, dumpSeq.Add(1)), doc, 0o600); err != nil {
		t.Fatalf("AGENTMOAT_MD_DUMP: %v", err)
	}
}

// dumpSeq numbers the files that renderMarkdown writes, because one test can
// render more than one document.
var dumpSeq atomic.Int64

func TestMdCode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a plain value", "web", "`web`"},
		{"an empty value is a dash", "", "-"},
		{"only spaces is a dash", "   ", "-"},
		{"a backtick inside needs a longer delimiter", "a`b", "``a`b``"},
		{"a run of backticks", "a```b", "````a```b````"},
		{"a backtick at the start gets padding", "`a", "`` `a ``"},
		{"a backtick at the end gets padding", "a`", "`` a` ``"},
		{"a newline cannot end the span", "a\nb", "`a b`"},
		{"a tab is a space", "a\tb", "`a b`"},
		{"an escape character is made visible", "a\x1b[31mb", "`a\uFFFD[31mb`"},
		{"a bidirectional override is made visible", "a\u202Eb", "`a\uFFFDb`"},
		{"markdown inside is left alone, it is not read there", "[x](http://e) <img> **b**", "`[x](http://e) <img> **b**`"},
	}
	for _, tc := range tests {
		if got := mdCode(tc.in); got != tc.want {
			t.Errorf("%s: mdCode(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	// In a table cell, a pipe must be escaped or it ends the cell. The run
	// of backslashes before each pipe is odd in the output, whatever the
	// value has: an even run is the end of the cell for a parser that
	// pairs backslashes.
	cells := []struct{ in, want string }{
		{`a|b`, "`a\\|b`"},
		{`a||b`, "`a\\|\\|b`"},
		{`a\|b`, "`a\\|b`"},       // one backslash in the value: odd, left as it is
		{`a\\|b`, "`a\\\\\\|b`"},  // two: made three
		{`a\\\|b`, "`a\\\\\\|b`"}, // three: left as it is
		{`a\b|c`, "`a\\b\\|c`"},   // a backslash that does not touch the pipe
		{`|`, "`\\|`"},
		{`a\`, "`a\\`"}, // no pipe: nothing to do
	}
	for _, tc := range cells {
		if got := mdCodeCell(tc.in); got != tc.want {
			t.Errorf("mdCodeCell(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Outside a table it must not be: the backslash would show.
	if got, want := mdCode("a|b"), "`a|b`"; got != want {
		t.Errorf("mdCode(a|b) = %q, want %q", got, want)
	}

	long := strings.Repeat("x", mdMaxValue+50)
	got := mdCode(long)
	if n := len([]rune(got)); n != mdMaxValue+2 || !strings.HasSuffix(got, "…`") {
		t.Errorf("mdCode(long) is %d characters and ends %q; want %d with the cut marked", n, got[len(got)-6:], mdMaxValue+2)
	}
}

func TestMdText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"Pod uses hostNetwork: true; gVisor cannot.", "Pod uses hostNetwork: true; gVisor cannot."},
		{"a [link](target)", `a \[link\](target)`},
		{"an <img src=x> tag", "an &lt;img src=x&gt; tag"},
		{"**bold** and _em_ and `code`", "\\*\\*bold\\*\\* and \\_em\\_ and \\`code\\`"},
		{"a | pipe", `a \| pipe`},
		{"an &amp; entity", "an &amp;amp; entity"},
		{"a\nnew line", "a new line"},
		{"![image](x.png)", `\!\[image\](x.png)`},
		{`a \ backslash`, `a \\ backslash`},
		{"~~strike~~ and # heading", `\~\~strike\~\~ and \# heading`},
		{`a \| b`, `a \\\| b`},

		// A word that a renderer links by itself goes in a code span. The
		// rest of the sentence is escaped as before.
		{"see http://evil.test/x now", "see `http://evil.test/x` now"},
		{"see HTTPS://EVIL.TEST now", "see `HTTPS://EVIL.TEST` now"},
		{"see www.evil.test now", "see `www.evil.test` now"},
		{"mail a@evil.test now", "mail `a@evil.test` now"},
		{"ping @evil-team now", "ping `@evil-team` now"},
		{"a mailto:a.b now", "a `mailto:a.b` now"},
		{"a [link](http://e) *b*", "a `[link](http://e)` \\*b\\*"},
		{"![image](http://e/x.png)", "`![image](http://e/x.png)`"},
		{"x|http://e|y", "x\\|`http://e`\\|y"},
		{"a `http://e` b", "a `` `http://e` `` b"},
		// A resource name with a domain is not a link: no scheme, no www.
		{"The request is for nvidia.com/mig-1g.5gb.", "The request is for nvidia.com/mig-1g.5gb."},
	}
	for _, tc := range tests {
		if got := mdText(tc.in); got != tc.want {
			t.Errorf("mdText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMdLink(t *testing.T) {
	t.Parallel()
	ok := []string{
		"https://gvisor.dev/docs/user_guide/compatibility/#networking",
		"http://example.test/a?b=c&d=e",
	}
	for _, u := range ok {
		if got := mdLink("read", u); got != "[read]("+u+")" {
			t.Errorf("mdLink(%q) = %q, want a link", u, got)
		}
	}
	bad := []string{
		"",
		"javascript:alert(1)",
		"https://example.test/a) [x](https://evil.test",
		"https://example.test/a b",
		"https://example.test/<x>",
		"https://example.test/\"x",
		"ftp://example.test/x",
		"//example.test/x",
	}
	for _, u := range bad {
		if got := mdLink("read", u); got != "" {
			t.Errorf("mdLink(%q) = %q, want no link", u, got)
		}
	}
}

func TestMdFence(t *testing.T) {
	t.Parallel()
	var d mdDoc
	d.fence([]string{"plain", "has ``` three", "has ````` five\nand a newline"})
	got := d.b.String()
	// The fence is six backticks: one more than the longest run inside.
	want := "``````text\nplain\nhas ``` three\nhas ````` five and a newline\n``````\n\n"
	if got != want {
		t.Errorf("fence =\n%q\nwant\n%q", got, want)
	}

	var capped mdDoc
	lines := make([]string, mdMaxRows+7)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i)
	}
	capped.fence(lines)
	out := capped.b.String()
	if strings.Contains(out, fmt.Sprintf("line %d\n", mdMaxRows)) || !strings.Contains(out, "_7 more not shown.") {
		t.Errorf("a long fenced block must stop at %d lines and say that 7 are left out:\n%s", mdMaxRows, out)
	}
}

// ---------------------------------------------------------------------------
// The gate line.
// ---------------------------------------------------------------------------

func TestScanMarkdown_GateLine(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Summary = schema.Summary{Total: 1, Compatible: 1}
	report.Spec.Workloads = []schema.WorkloadResult{
		{Kind: "Deployment", Namespace: "ns", Name: "web", Compatibility: schema.CompatibilityCompatible},
	}

	tests := []struct {
		name string
		gate *Gate
		want string // the gate line must start with this
	}{
		{"passed", &Gate{FailOn: "sandboxed", Outcome: GatePassed, Summary: "1 workload asks."},
			"✅ **Gate passed** (`--fail-on sandboxed`). 1 workload asks."},
		{"failed", &Gate{FailOn: "incompatible", Outcome: GateFailed, Summary: "1 of 1 cannot run."},
			"❌ **Gate failed** (`--fail-on incompatible`). 1 of 1 cannot run."},
		{"checked nothing is not shown as a pass", &Gate{FailOn: "sandboxed", Outcome: GateNothingChecked, Summary: "No workload asks."},
			"⚠️ **Gate checked nothing** (`--fail-on sandboxed`). No workload asks."},
		{"refused", &Gate{FailOn: "sandboxed", Outcome: GateRefused, Summary: "The input is not rendered."},
			"❌ **Gate could not answer** (`--fail-on sandboxed`). The input is not rendered."},
		{"off", &Gate{FailOn: "none", Outcome: GateOff, Summary: "This report does not pass or fail."},
			"➖ **No gate** (`--fail-on none`). This report does not pass or fail."},
		{"an outcome this renderer does not know is not shown as a pass", &Gate{FailOn: "sandboxed", Outcome: "future", Summary: "x"},
			"⚠️ **Gate result unknown**"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := renderMarkdown(t, report, tc.gate)
			lines := strings.Split(out, "\n")
			// Line 0 is the title, line 1 is empty, line 2 is the gate:
			// before the numbers and before every table.
			if len(lines) < 3 || lines[0] != "## agentmoat scan" || !strings.HasPrefix(lines[2], tc.want) {
				t.Errorf("the gate line must be the first thing under the title and start with %q:\n%s", tc.want, out)
			}
			if tc.gate.Outcome != GatePassed && strings.Contains(out, "Gate passed") {
				t.Errorf("outcome %q must not say \"Gate passed\":\n%s", tc.gate.Outcome, out)
			}
		})
	}

	// No gate from the caller: no gate line, and no word "Gate" at all.
	if out := renderMarkdown(t, report, nil); strings.Contains(out, "Gate") {
		t.Errorf("with a nil gate the report must not have a gate line:\n%s", out)
	}
}

func TestScanMarkdown_GateDetailsAreListed(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	gate := &Gate{
		FailOn: "sandboxed", Outcome: GateFailed, Summary: "2 workload(s) ask and cannot run.",
		Details: []string{"Deployment edge/gateway: host-network (k8s/gw.yaml:4)", "Deployment edge/proxy: privileged (k8s/px.yaml:1)"},
	}
	out := renderMarkdown(t, report, gate)
	want := "```text\nDeployment edge/gateway: host-network (k8s/gw.yaml:4)\nDeployment edge/proxy: privileged (k8s/px.yaml:1)\n```\n"
	if !strings.Contains(out, want) {
		t.Errorf("the failing workloads must be listed in a fenced block under the gate line:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// The report body.
// ---------------------------------------------------------------------------

func TestScanMarkdown_Sections(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Metadata.RuntimeClassName = "gvisor"
	report.Metadata.Source = &schema.ReportSource{
		Type:       schema.SourceManifests,
		Inputs:     3,
		Skipped:    map[string]int{"Service": 2, "ConfigMap": 1},
		Duplicates: []string{"Deployment edge/web is defined 2 times: a.yaml:1, b.yaml:1"},
	}
	report.Spec.Summary = schema.Summary{Total: 3, Compatible: 1, NeedsReview: 1, Incompatible: 1, Sandboxed: 1}
	hostNet := schema.Reason{RuleID: "host-network", Severity: schema.SeverityError,
		Description: "Pod uses hostNetwork: true.", RemediationURL: "https://gvisor.dev/docs/user_guide/compatibility/#networking"}
	report.Spec.Workloads = []schema.WorkloadResult{
		{Kind: "Deployment", Namespace: "edge", Name: "gateway", RuntimeClassName: "gvisor",
			Compatibility: schema.CompatibilityIncompatible, Reasons: []schema.Reason{hostNet},
			Origin: &schema.Origin{File: "k8s/gw.yaml", Line: 4}},
		{Kind: "Deployment", Namespace: "edge", Name: "store",
			Compatibility: schema.CompatibilityReview,
			Reasons:       []schema.Reason{{RuleID: "host-path-mount", Severity: schema.SeverityWarn, Description: "Pod mounts a hostPath volume."}},
			Origin:        &schema.Origin{File: "chart/templates/store.yaml"}},
		{Kind: "Deployment", Namespace: "edge", Name: "web", Compatibility: schema.CompatibilityCompatible},
	}

	out := renderMarkdown(t, report, nil)
	for _, want := range []string{
		"| Workloads | Compatible | Review | Incompatible | Sandboxed |\n| ---: | ---: | ---: | ---: | ---: |\n| 3 | 1 | 1 | 1 | 1 |\n",
		"Source: `manifests` (3 input(s)). No cluster was read. Sandboxed: the workloads that ask for RuntimeClass `gvisor`.",
		"### Incompatible (1)\n",
		"| `edge` | `Deployment` | `gateway` | `gvisor` | `host-network` (error) | `k8s/gw.yaml:4` |",
		"### Review (1)\n",
		// No line number is known for this one: the file alone.
		"| `edge` | `Deployment` | `store` | - | `host-path-mount` (warn) | `chart/templates/store.yaml` |",
		// Compatible workloads are there, in a collapsed block.
		"<details>\n<summary>Compatible (1)</summary>\n\n| Namespace |",
		"| `edge` | `Deployment` | `web` | - | - | - |",
		"</details>\n",
		"### What the rules say\n",
		"- `host-network` (error, 1 workload(s)): Pod uses hostNetwork: true. " +
			"[explanation](https://github.com/0hardik1/agentmoat/blob/main/docs/explanations/host-network.md) · " +
			"[gVisor documentation](https://gvisor.dev/docs/user_guide/compatibility/#networking)",
		"### Not classified\n",
		"`ConfigMap` 1, `Service` 2.",
		"### Defined more than one time\n",
		"```text\nDeployment edge/web is defined 2 times: a.yaml:1, b.yaml:1\n```\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks:\n%s\n\nfull output:\n%s", want, out)
		}
	}
	// The incompatible section comes before the review section, and both
	// before the collapsed compatible block.
	bad, rev, ok := strings.Index(out, "### Incompatible"), strings.Index(out, "### Review"), strings.Index(out, "<details>")
	if bad >= rev || rev >= ok {
		t.Errorf("order must be incompatible, review, compatible; offsets %d, %d, %d", bad, rev, ok)
	}
	if strings.Contains(out, "\x1b[") {
		t.Errorf("Markdown must have no ANSI escape codes")
	}
	if !strings.HasSuffix(out, "\n") {
		t.Errorf("the output must end with a newline")
	}
}

func TestScanMarkdown_ClusterSource(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Metadata.Cluster = "prod-eu"
	out := renderMarkdown(t, report, nil)
	if !strings.Contains(out, "Cluster: `prod-eu`. Sandboxed: the workloads that ask for RuntimeClass `gvisor`.") {
		t.Errorf("a cluster scan names the cluster, and an empty class name means the default:\n%s", out)
	}
	for _, absent := range []string{"### Incompatible", "### Review", "<details>", "### What the rules say", "### Not classified"} {
		if strings.Contains(out, absent) {
			t.Errorf("an empty report must not have the section %q:\n%s", absent, out)
		}
	}
}

func TestScanMarkdown_UnrenderedWarning(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Metadata.Source = &schema.ReportSource{
		Type:       schema.SourceManifests,
		Inputs:     2,
		Unrendered: []string{"kustomization file: overlay/kustomization.yaml"},
	}
	out := renderMarkdown(t, report, nil)
	for _, want := range []string{
		"> **Warning: this input is not rendered.**",
		"`kustomize build <dir> | agentmoat scan -f -`",
		"```text\nkustomization file: overlay/kustomization.yaml\n```\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

// TestScanMarkdown_LegendGroupsByText: the classifier can add a sentence
// about one workload to a rule's description. The legend must not show the
// text of one workload as the meaning of the rule for all of them.
func TestScanMarkdown_LegendGroupsByText(t *testing.T) {
	t.Parallel()
	gpu := func(note string) []schema.Reason {
		return []schema.Reason{{RuleID: "gpu-passthrough", Severity: schema.SeverityWarn, Description: "Container requests a GPU." + note}}
	}
	report := schema.NewScanReport()
	report.Spec.Workloads = []schema.WorkloadResult{
		{Kind: "Pod", Namespace: "ml", Name: "a", Compatibility: schema.CompatibilityReview, Reasons: gpu("")},
		{Kind: "Pod", Namespace: "ml", Name: "b", Compatibility: schema.CompatibilityReview, Reasons: gpu("")},
		{Kind: "Pod", Namespace: "ml", Name: "c", Compatibility: schema.CompatibilityReview, Reasons: gpu(" The request is for a MIG slice.")},
	}
	out := renderMarkdown(t, report, nil)
	for _, want := range []string{
		"- `gpu-passthrough` (warn, 2 workload(s)): Container requests a GPU. [",
		"- `gpu-passthrough` (warn, 1 workload(s)): Container requests a GPU. The request is for a MIG slice. [",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the legend lacks %q:\n%s", want, out)
		}
	}
}

func TestScanMarkdown_LongTablesAreCapped(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	const total = mdMaxRows + 12
	for i := 0; i < total; i++ {
		report.Spec.Workloads = append(report.Spec.Workloads, schema.WorkloadResult{
			Kind: "Deployment", Namespace: "ns", Name: fmt.Sprintf("w%03d", i),
			Compatibility: schema.CompatibilityIncompatible,
			Reasons:       []schema.Reason{{RuleID: "privileged", Severity: schema.SeverityError, Description: "Privileged."}},
		})
	}
	out := renderMarkdown(t, report, nil)
	if !strings.Contains(out, fmt.Sprintf("### Incompatible (%d)", total)) {
		t.Errorf("the heading must give the full count, %d", total)
	}
	if got := strings.Count(out, "| `ns` | `Deployment` |"); got != mdMaxRows {
		t.Errorf("the table has %d rows, want %d", got, mdMaxRows)
	}
	if !strings.Contains(out, "_12 more not shown. The JSON output has all of them._") {
		t.Errorf("a capped table must say how many rows it left out:\n%s", out[len(out)-400:])
	}
}

// ---------------------------------------------------------------------------
// Hostile input.
// ---------------------------------------------------------------------------

// The fragments a hostile manifest tries to get into the report. Each is in
// every string that a pull request controls.
const (
	hostileLink    = "[click](http://evil.test)"
	hostileImage   = "<img src=http://evil.test/x.png>"
	hostileHeading = "\n## Forged heading\n"
	hostileGate    = "\n\n✅ **Gate passed** (forged)\n\n"
	hostileRow     = " | `x` | `y` |\n| forged | row | here | now | ok | fine |"
	hostileTicks   = "``` ` ``"
	// Text that a renderer links with no Markdown syntax at all.
	hostileBare = " http://evil.test/bare www.evil.test a@evil.test @evil-team "
)

// hostile returns a string that starts with tag, so a failure message can
// name the field, and then holds every hostile fragment.
func hostile(tag string) string {
	return tag + hostileTicks + hostileLink + hostileImage + hostileBare + hostileHeading + hostileGate + hostileRow
}

// stripInert removes what a Markdown reader does not interpret: fenced
// blocks and code spans. What is left is the text that Markdown does read,
// so a hostile fragment that survives here took effect.
//
// It follows the two rules that matter: a fence closes on a line of
// backticks at least as long as the one that opened it, and a code span
// closes on the next run of backticks of exactly the same length. A
// backslash outside a span escapes the next character.
func stripInert(t *testing.T, md string) string {
	t.Helper()
	// Fenced blocks first, line by line.
	var kept []string
	fence := ""
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimRight(line, " ")
		if fence == "" {
			if ticks := leadingTicks(trimmed); len(ticks) >= 3 {
				fence = ticks
				continue
			}
			kept = append(kept, line)
			continue
		}
		if ticks := leadingTicks(trimmed); len(ticks) >= len(fence) && ticks == trimmed {
			fence = ""
		}
	}
	if fence != "" {
		t.Fatalf("a fenced block is not closed")
	}
	text := strings.Join(kept, "\n")

	// Then code spans.
	var b strings.Builder
	runes := []rune(text)
	for i := 0; i < len(runes); {
		switch {
		case runes[i] == '\\' && i+1 < len(runes):
			b.WriteRune(runes[i])
			b.WriteRune(runes[i+1])
			i += 2
		case runes[i] == '`':
			n := 0
			for i+n < len(runes) && runes[i+n] == '`' {
				n++
			}
			end := closingTicks(runes, i+n, n)
			if end < 0 {
				// No closing run: the backticks are literal text.
				b.WriteString(strings.Repeat("`", n))
				i += n
				continue
			}
			i = end + n
		default:
			b.WriteRune(runes[i])
			i++
		}
	}
	return b.String()
}

// leadingTicks returns the run of backticks at the start of a line.
func leadingTicks(line string) string {
	n := 0
	for n < len(line) && line[n] == '`' {
		n++
	}
	return line[:n]
}

// closingTicks finds the next run of exactly n backticks at or after from,
// on the same line (a newline in this output always ends a block).
func closingTicks(runes []rune, from, n int) int {
	for i := from; i < len(runes); {
		if runes[i] == '\n' {
			return -1
		}
		if runes[i] != '`' {
			i++
			continue
		}
		run := 0
		for i+run < len(runes) && runes[i+run] == '`' {
			run++
		}
		if run == n {
			return i
		}
		i += run
	}
	return -1
}

// unescapedPipes counts the "|" in a table line that split it into cells.
func unescapedPipes(line string) int {
	n := 0
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			i++
		case '|':
			n++
		}
	}
	return n
}

var (
	// A link or an image that Markdown would read: "](" with no backslash
	// before the bracket.
	liveLink = regexp.MustCompile(`(^|[^\\])\]\(`)
	// Emphasis markers with no backslash before them.
	liveBold = regexp.MustCompile(`(^|[^\\*])\*\*Gate passed\*\*`)
)

// assertInert checks a rendered document that had hostile strings in it.
func assertInert(t *testing.T, out string, wantHeadings []string) {
	t.Helper()
	assertNoLiveMarkup(t, out)
	assertLinesAreOurs(t, out, wantHeadings)
	assertTableShape(t, out)
}

// assertNoLiveMarkup: in the text that Markdown reads (outside code spans
// and fenced blocks), there is no tag, link, or bold gate line from the
// input.
func assertNoLiveMarkup(t *testing.T, out string) {
	t.Helper()
	read := stripInert(t, out)
	if strings.Contains(read, "<img") {
		t.Errorf("an HTML tag from the input is in the text that Markdown reads")
	}
	if liveLink.MatchString(read) {
		t.Errorf("a link from the input is in the text that Markdown reads: %q", liveLink.FindString(read))
	}
	if liveBold.MatchString(read) {
		t.Errorf("a forged, bold \"Gate passed\" is in the text that Markdown reads")
	}
	// No caller of this helper renders a document with a link of its own,
	// so any address or "@" outside a code span is from the input.
	for _, auto := range []string{"://", "www.", "@"} {
		if strings.Contains(read, auto) {
			t.Errorf("%q from the input is in the text that Markdown reads; a renderer makes a link or a mention of it", auto)
		}
	}
}

// assertLinesAreOurs: no newline from the input became a line of the
// document. Every heading is one that the renderer wrote, and no line
// starts with the mark of a passing gate (no caller of this helper renders
// a document that passes).
func assertLinesAreOurs(t *testing.T, out string, wantHeadings []string) {
	t.Helper()
	var headings []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "#") {
			headings = append(headings, line)
		}
		if strings.HasPrefix(line, "✅") {
			t.Errorf("a line from the input starts a line of the document: %q", line)
		}
	}
	if strings.Join(headings, "\n") != strings.Join(wantHeadings, "\n") {
		t.Errorf("headings =\n%s\nwant\n%s", strings.Join(headings, "\n"), strings.Join(wantHeadings, "\n"))
	}
}

// assertTableShape: every row of every table has the cell count of its
// header. A pipe from the input did not add a cell, and no forged row is
// there.
func assertTableShape(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i := 0; i < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "| ") {
			continue
		}
		header := unescapedPipes(lines[i])
		for i++; i < len(lines) && strings.HasPrefix(lines[i], "|"); i++ {
			if got := unescapedPipes(lines[i]); got != header {
				t.Errorf("a table row has %d cell separators, its header has %d: %q", got, header, lines[i])
			}
			if strings.HasPrefix(lines[i], "| forged") {
				t.Errorf("a forged table row is in the document: %q", lines[i])
			}
		}
	}
}

// TestScanMarkdown_HostileManifest: a pull request controls every string in
// its manifests. None of them may add structure to the report.
func TestScanMarkdown_HostileManifest(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Metadata.RuntimeClassName = hostile("class")
	report.Metadata.Source = &schema.ReportSource{
		Type:       schema.SourceManifests,
		Inputs:     1,
		Skipped:    map[string]int{hostile("skipped-kind"): 1},
		Duplicates: []string{hostile("duplicate")},
		Unrendered: []string{hostile("unrendered")},
	}
	reasons := []schema.Reason{{
		RuleID:   hostile("rule"),
		Severity: schema.Severity(hostile("severity")),
		// The GPU rule puts a resource name from the pod spec in here.
		Description:    hostile("description"),
		RemediationURL: "https://ok.test/a) " + hostile("url"),
	}}
	for _, verdict := range []schema.Compatibility{
		schema.CompatibilityIncompatible, schema.CompatibilityReview, schema.CompatibilityCompatible,
	} {
		report.Spec.Workloads = append(report.Spec.Workloads, schema.WorkloadResult{
			Kind:             hostile("kind"),
			Namespace:        hostile("namespace"),
			Name:             hostile("name"),
			RuntimeClassName: hostile("runtime"),
			Compatibility:    verdict,
			Reasons:          reasons,
			Origin:           &schema.Origin{File: hostile("file"), Line: 7},
		})
	}
	gate := &Gate{
		FailOn:  "sandboxed",
		Outcome: GateFailed,
		// The gate texts hold names and paths from the manifests.
		Summary: hostile("summary"),
		Details: []string{hostile("detail-1"), hostile("detail-2")},
	}

	out := renderMarkdown(t, report, gate)
	assertInert(t, out, []string{
		"## agentmoat scan",
		"### Incompatible (1)",
		"### Review (1)",
		"### What the rules say",
		"### Not classified",
		"### Defined more than one time",
	})

	// The real gate line is still the first thing, and it says "failed".
	if lines := strings.Split(out, "\n"); !strings.HasPrefix(lines[2], "❌ **Gate failed** (`--fail-on sandboxed`). ") {
		t.Errorf("the gate line changed: %q", lines[2])
	}
	// Three workload tables, one row each: no row was added or lost.
	if got := strings.Count(out, "\n| ````namespace"); got != 3 {
		t.Errorf("the tables have %d workload rows, want 3:\n%s", got, out)
	}
	// The bad URL gave no link at all, and the rule has no doc link.
	if strings.Contains(stripInert(t, out), "gVisor documentation") {
		t.Errorf("a URL that is not safe must not become a link")
	}
}

// TestScanMarkdown_ShortHostileValues: the long hostile strings above are
// cut at the length limit before their pipes. These are short, so the pipe
// and the backtick are in the cell.
func TestScanMarkdown_ShortHostileValues(t *testing.T) {
	t.Parallel()
	report := schema.NewScanReport()
	report.Spec.Workloads = []schema.WorkloadResult{{
		Kind:             "K|ind",
		Namespace:        "n`|`s",
		Name:             "a | b | c",
		RuntimeClassName: "`|",
		Compatibility:    schema.CompatibilityIncompatible,
		Reasons:          []schema.Reason{{RuleID: "r|d", Severity: "e|r", Description: "d | e"}},
		Origin:           &schema.Origin{File: "f|g.yaml", Line: 2},
	}}
	out := renderMarkdown(t, report, nil)
	assertInert(t, out, []string{"## agentmoat scan", "### Incompatible (1)", "### What the rules say"})
	want := "| ``n`\\|`s`` | `K\\|ind` | `a \\| b \\| c` | `` `\\| `` | `r\\|d` (e\\|r) | `f\\|g.yaml:2` |"
	if !strings.Contains(out, want) {
		t.Errorf("the row is not\n%s\nin:\n%s", want, out)
	}
}

// TestScanMarkdown_BackslashBeforeAPipe: a value with a backslash before a
// pipe (a file named "a\|b.yaml" is legal on Linux). If the escape made the
// run of backslashes even, a parser that pairs them would end the cell at
// the pipe and read the rest of the value as Markdown.
func TestScanMarkdown_BackslashBeforeAPipe(t *testing.T) {
	t.Parallel()
	const breakout = `x\|[click](target) **Gate passed** | y\\|z`
	report := schema.NewScanReport()
	report.Spec.Workloads = []schema.WorkloadResult{{
		Kind:             breakout,
		Namespace:        breakout,
		Name:             breakout,
		RuntimeClassName: breakout,
		Compatibility:    schema.CompatibilityIncompatible,
		Reasons:          []schema.Reason{{RuleID: breakout, Severity: schema.Severity(breakout), Description: breakout}},
		Origin:           &schema.Origin{File: breakout, Line: 2},
	}}
	out := renderMarkdown(t, report, &Gate{FailOn: "sandboxed", Outcome: GateFailed, Summary: breakout, Details: []string{breakout}})
	assertInert(t, out, []string{"## agentmoat scan", "### Incompatible (1)", "### What the rules say"})

	// Every pipe from the value has an odd run of backslashes before it,
	// in a code span and in a sentence.
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "| ") {
			continue
		}
		for i := 0; i < len(line); i++ {
			if line[i] != '|' || i == 0 || line[i-1] != '\\' {
				continue
			}
			run := 0
			for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
				run++
			}
			if run%2 == 0 {
				t.Errorf("a pipe has an even run of %d backslashes before it: %q", run, line)
			}
		}
	}
}

// TestScanMarkdown_SizeOfALargeHostileReport: a job summary on GitHub is
// limited to 1 MiB for each step. A report with many more workloads than a
// table shows, each with long names and a description of its own (so the
// legend cannot group them), in characters that grow when they are escaped,
// must stay below half of that. Every section has a cap, so more workloads
// than this do not make the output larger.
func TestScanMarkdown_SizeOfALargeHostileReport(t *testing.T) {
	t.Parallel()
	long := func(tag string, i int) string {
		return fmt.Sprintf("%s-%05d-", tag, i) + strings.Repeat("&<|`*", 200)
	}
	report := schema.NewScanReport()
	report.Metadata.RuntimeClassName = long("class", 0)
	const workloads = 12 * mdMaxRows // 200 for each verdict: four times the cap
	report.Metadata.Source = &schema.ReportSource{Type: schema.SourceManifests, Inputs: workloads, Skipped: map[string]int{}}
	for i := 0; i < workloads; i++ {
		report.Metadata.Source.Skipped[long("kind", i)] = 1
		report.Metadata.Source.Duplicates = append(report.Metadata.Source.Duplicates, long("duplicate", i))
		report.Metadata.Source.Unrendered = append(report.Metadata.Source.Unrendered, long("unrendered", i))
		verdict := []schema.Compatibility{schema.CompatibilityIncompatible, schema.CompatibilityReview, schema.CompatibilityCompatible}[i%3]
		w := schema.WorkloadResult{
			Kind: long("kind", i), Namespace: long("ns", i), Name: long("name", i), RuntimeClassName: long("rc", i),
			Compatibility: verdict, Origin: &schema.Origin{File: long("file", i), Line: i + 1},
		}
		// More reasons than there are built-in rules.
		for r := 0; r < 20; r++ {
			w.Reasons = append(w.Reasons, schema.Reason{
				RuleID: long("rule", r), Severity: schema.Severity(long("severity", r)),
				Description: long("description", i*100+r), RemediationURL: "https://ok.test/" + strings.Repeat("a", 2000),
			})
		}
		report.Spec.Workloads = append(report.Spec.Workloads, w)
	}
	gate := &Gate{FailOn: "sandboxed", Outcome: GateFailed, Summary: long("summary", 0)}
	for i := 0; i < workloads; i++ {
		gate.Details = append(gate.Details, long("detail", i))
	}

	out := renderMarkdown(t, report, gate)
	const limit = 1 << 20 // 1 MiB, the GitHub limit for one step
	if len(out) > limit/2 {
		t.Errorf("the report is %d bytes; want less than half of the %d byte limit", len(out), limit)
	}
	t.Logf("a report of %d hostile workloads is %d bytes", workloads, len(out))

	// The cap on rules in one row says how many it left out.
	if !strings.Contains(out, ", 14 more |") {
		t.Errorf("a row with 20 rules must name %d and say \"14 more\"", mdMaxRowRules)
	}
}

// ---------------------------------------------------------------------------
// The pipeline documents.
// ---------------------------------------------------------------------------

func TestPlanMarkdown(t *testing.T) {
	t.Parallel()
	plan := schema.NewMigrationPlan()
	plan.Metadata.PlanHash = "sha256:abc123"
	plan.Metadata.Cluster = "prod-eu"
	plan.Spec.Options.RuntimeClassName = "gvisor"
	plan.Spec.Summary = schema.PlanSummary{Total: 2, Included: 1, Excluded: 1}
	plan.Spec.Warnings = []schema.PlanWarning{{ID: "manifest-source", Message: "this plan is a preview"}}
	plan.Spec.Steps = []schema.PlanStep{{
		Order: 1, Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "edge", Name: "web"},
		RiskScore: 10, WaitFor: "rollout", Notes: "stateless",
	}}
	plan.Spec.Excluded = []schema.ExcludedWorkload{{
		Target:        schema.WorkloadRef{Kind: "DaemonSet", Namespace: "kube-system", Name: "cni"},
		Compatibility: schema.CompatibilityIncompatible, Reason: "host-network",
	}}

	out := renderMarkdown(t, plan, nil)
	for _, want := range []string{
		"## agentmoat plan\n",
		"Plan hash: `sha256:abc123` · RuntimeClass: `gvisor` · Cluster: `prod-eu`",
		"| 2 | 1 | 1 |",
		"### Warnings (1)\n\n- ⚠️ `manifest-source`: this plan is a preview\n",
		"### Steps (1)\n",
		"| 1 | `edge` | `Deployment` | `web` | 10 | `rollout` | stateless |",
		"<summary>Excluded (1)</summary>",
		"| `kube-system` | `DaemonSet` | `cni` | incompatible | host-network |",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}

	empty := renderMarkdown(t, schema.NewMigrationPlan(), nil)
	if !strings.Contains(empty, "### Steps (0)\n\nThe plan has no steps.") {
		t.Errorf("an empty plan must say that it has no steps:\n%s", empty)
	}
}

// applyWith builds an ApplyResult with the given step statuses, in order.
func applyWith(dryRun bool, statuses ...schema.StepStatus) *schema.ApplyResult {
	res := schema.NewApplyResult()
	res.Metadata.PlanHash = "sha256:abc123"
	res.Metadata.DryRun = dryRun
	for i, st := range statuses {
		step := schema.StepResult{
			Order:  i + 1,
			Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "edge", Name: fmt.Sprintf("w%03d", i+1)},
			Status: st,
		}
		res.Spec.Summary.Total++
		switch st {
		case schema.StepStatusApplied:
			res.Spec.Summary.Applied++
		case schema.StepStatusAlreadyApplied:
			res.Spec.Summary.AlreadyApplied++
		case schema.StepStatusSkipped:
			res.Spec.Summary.Skipped++
		case schema.StepStatusFailed:
			res.Spec.Summary.Failed++
			step.Error = "the server refused the patch"
		}
		res.Spec.Steps = append(res.Spec.Steps, step)
	}
	return res
}

func TestApplyMarkdown_Headline(t *testing.T) {
	t.Parallel()
	const (
		applied = schema.StepStatusApplied
		already = schema.StepStatusAlreadyApplied
		skipped = schema.StepStatusSkipped
		failed  = schema.StepStatusFailed
	)
	tests := []struct {
		name string
		res  *schema.ApplyResult
		want []string
	}{
		{"a dry run says that nothing was changed",
			applyWith(true, applied, applied),
			[]string{"## agentmoat apply (dry run)\n", "➖ **Dry run.** Nothing was changed. Pass `--dry-run=false` to apply."}},
		{"a live run with no failure",
			applyWith(false, applied, already),
			[]string{"## agentmoat apply\n", "✅ **All 2 step(s) are done.**", "| 2 | 1 | 1 | 0 | 0 |"}},
		{"a failure is the headline, and its row is not in the collapsed block",
			applyWith(false, applied, failed),
			[]string{"❌ **1 of 2 step(s) failed.**", "### Failed or skipped (1)\n",
				"| 2 | `edge` | `Deployment` | `w002` | failed | the server refused the patch |",
				"<summary>Other steps (1)</summary>"}},
		{"skipped steps are a warning",
			applyWith(false, skipped, skipped),
			[]string{"⚠️ **2 of 2 step(s) were skipped.**", "### Failed or skipped (2)\n"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out := renderMarkdown(t, tc.res, nil)
			for _, want := range tc.want {
				if !strings.Contains(out, want) {
					t.Errorf("the output lacks %q:\n%s", want, out)
				}
			}
		})
	}
}

// TestApplyMarkdown_FailureIsNotHiddenByTheCap: sixty steps that are fine
// and one that failed, last. A table that stops at fifty rows must not be
// what hides it.
func TestApplyMarkdown_FailureIsNotHiddenByTheCap(t *testing.T) {
	t.Parallel()
	statuses := make([]schema.StepStatus, 0, 61)
	for i := 0; i < 60; i++ {
		statuses = append(statuses, schema.StepStatusApplied)
	}
	statuses = append(statuses, schema.StepStatusFailed)
	out := renderMarkdown(t, applyWith(false, statuses...), nil)

	failedRow := strings.Index(out, "| 61 | `edge` | `Deployment` | `w061` | failed |")
	collapsed := strings.Index(out, "<details>")
	if failedRow < 0 || collapsed < 0 || failedRow > collapsed {
		t.Errorf("the failed step must be listed before the collapsed block (offsets %d and %d)", failedRow, collapsed)
	}
	if !strings.Contains(out, "_10 more not shown.") {
		t.Errorf("the collapsed block of 60 must say that 10 rows are left out")
	}
}

func TestRollbackMarkdown(t *testing.T) {
	t.Parallel()
	res := schema.NewRollbackResult()
	res.Metadata.PlanHash = "sha256:abc123"
	res.Spec.Summary = schema.ApplySummary{Total: 1, Applied: 1}
	res.Spec.Steps = []schema.StepResult{{
		Order: 1, Target: schema.WorkloadRef{Kind: "Deployment", Namespace: "edge", Name: "web"}, Status: schema.StepStatusApplied,
	}}
	out := renderMarkdown(t, res, nil)
	for _, want := range []string{"## agentmoat rollback\n", "✅ **All 1 step(s) are done.**", "| 1 | `edge` | `Deployment` | `web` | applied |"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
}

func TestVerifyMarkdown(t *testing.T) {
	t.Parallel()
	report := sampleVerifyReport()
	out := renderMarkdown(t, report, nil)
	sum := report.Spec.Summary
	wantHead := fmt.Sprintf("❌ **%d of %d workload(s) are not verified.**", sum.Mismatch+sum.Error, sum.Total)
	if sum.Mismatch+sum.Error == 0 {
		wantHead = fmt.Sprintf("✅ **All %d workload(s) are verified.**", sum.Total)
	}
	for _, want := range []string{"## agentmoat verify\n", wantHead, "| Workloads | OK | Mismatch | Error |"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	// The rows that are not ok come before the collapsed ok rows.
	if sum.Mismatch+sum.Error > 0 && sum.OK > 0 {
		attention, ok := strings.Index(out, "### Mismatch or error"), strings.Index(out, "<summary>OK (")
		if attention < 0 || ok < 0 || attention > ok {
			t.Errorf("mismatches must be listed before the ok rows (offsets %d and %d):\n%s", attention, ok, out)
		}
	}

	empty := renderMarkdown(t, schema.NewVerifyReport(), nil)
	if !strings.Contains(empty, "⚠️ **Nothing was verified.**") {
		t.Errorf("a verify of nothing must not say that all is verified:\n%s", empty)
	}
	for _, bad := range []string{"Wait:", "| Ready |"} {
		if strings.Contains(out, bad) {
			t.Errorf("a verify with no wait must not show %q:\n%s", bad, out)
		}
	}
}

// TestVerifyMarkdown_Wait: a job summary shows no stderr. The headline must
// say when "not verified" is the result after a wait, because that is the
// difference between "look again" and "roll back".
func TestVerifyMarkdown_Wait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		wait schema.VerifyWait
		ok   bool
		want []string
	}{
		{
			name: "timed out",
			wait: schema.VerifyWait{RequestedSeconds: 180, WaitedSeconds: 180, Rounds: 37, TimedOut: true},
			want: []string{
				"❌ **1 of 3 workload(s) are not verified after a wait of 3m0s.**",
				"Wait: timed out after 3m0s",
				"| Node | Ready | Message |",
				"| 1/3 | pod cache-1 is not Ready: container app is waiting: CrashLoopBackOff, 4 restart(s) (and 1 more pod(s) not Ready) |",
				"| 2/2 |",
				"| no pods |",
			},
		},
		{
			name: "interrupted",
			wait: schema.VerifyWait{RequestedSeconds: 180, WaitedSeconds: 45, Rounds: 10, Interrupted: true},
			want: []string{
				"❌ **1 of 3 workload(s) are not verified. The wait of 3m0s was interrupted after 45s.**",
				"Wait: interrupted after 45s of 3m0s",
			},
		},
		{
			name: "all ok",
			wait: schema.VerifyWait{RequestedSeconds: 180, WaitedSeconds: 20, Rounds: 5},
			ok:   true,
			want: []string{
				"✅ **All 2 workload(s) are verified, and their pods stayed Ready.**",
				"Wait: done after 20s of 3m0s",
			},
		},
	}
	for _, tc := range tests {
		report := waitedVerifyReport(tc.wait)
		if tc.ok {
			report.Spec.Results = report.Spec.Results[:2]
			report.Spec.Summary = schema.VerifySummary{Total: 2, OK: 2}
		}
		out := renderMarkdown(t, report, nil)
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: the output lacks %q:\n%s", tc.name, want, out)
			}
		}
	}
}

// TestVerifyMarkdown_HostileReadiness: the reason that a pod is not Ready
// has text from the cluster in it (a pod name, a container name, a
// scheduler message). It must be inert in the report, like text from a
// manifest.
func TestVerifyMarkdown_HostileReadiness(t *testing.T) {
	t.Parallel()
	report := waitedVerifyReport(schema.VerifyWait{RequestedSeconds: 60, WaitedSeconds: 60, Rounds: 13, TimedOut: true})
	last := &report.Spec.Results[2]
	last.Message = "pod " + hostile("pod") + " is not Ready: container " + hostile("container") + " is waiting: " + hostile("reason")
	last.Readiness.Message = last.Message
	last.Readiness.NotReady = []string{hostile("pod")}
	assertInert(t, renderMarkdown(t, report, nil), []string{"## agentmoat verify", "### Mismatch or error (1)"})
}

func TestPreflightMarkdown(t *testing.T) {
	t.Parallel()
	report := schema.NewPreflightReport()
	report.Metadata.RuntimeClassName = "gvisor"
	report.Metadata.Cluster = "prod-eu"
	report.Spec.Summary = schema.PreflightSummary{Ready: false, Total: 2, Error: 1, Info: 1}
	report.Spec.Facts.RuntimeClass = schema.RuntimeClassFacts{Name: "gvisor", Found: true, Handler: "runsc"}
	report.Spec.Findings = []schema.PreflightFinding{
		{ID: "gvisor-nodes-found", Severity: schema.SeverityInfo, Message: "1 node matches."},
		{ID: "runtimeclass-no-node-selector", Severity: schema.SeverityError,
			Message: "The RuntimeClass has no nodeSelector.", Remediation: "Add scheduling.nodeSelector."},
	}

	out := renderMarkdown(t, report, nil)
	for _, want := range []string{
		"## agentmoat preflight\n",
		"❌ **Blocked.** 1 error finding(s). `agentmoat apply` refuses to run on this cluster.",
		"RuntimeClass: `gvisor` · Cluster: `prod-eu`",
		"| 2 | 1 | 0 | 1 |",
		"| error | `runtimeclass-no-node-selector` | The RuntimeClass has no nodeSelector. | Add scheduling.nodeSelector. |",
		"- RuntimeClass `gvisor`: handler `runsc`, nodeSelector none.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	// The error finding is listed before the info finding, whatever the
	// order in the document.
	if strings.Index(out, "runtimeclass-no-node-selector") > strings.Index(out, "gvisor-nodes-found") {
		t.Errorf("error findings must be listed first:\n%s", out)
	}

	report.Spec.Summary = schema.PreflightSummary{Ready: true}
	report.Spec.Findings = nil
	if ready := renderMarkdown(t, report, nil); !strings.Contains(ready, "✅ **Ready.** Pods that ask for RuntimeClass `gvisor` can be scheduled.") {
		t.Errorf("a ready cluster must say so:\n%s", ready)
	}
}

// TestPreflightMarkdown_GPUFacts: `probe nvproxy -o markdown` writes a
// PreflightReport. Its result is the GPU facts, so they must be in the
// Markdown form.
func TestPreflightMarkdown_GPUFacts(t *testing.T) {
	t.Parallel()
	report := schema.NewPreflightReport()
	report.Metadata.RuntimeClassName = "gvisor"
	report.Metadata.Probe = &schema.ProbeMetadata{Namespace: "default", PodName: "agentmoat-probe", Node: "gpu-1", Succeeded: true}
	report.Spec.Summary = schema.PreflightSummary{Ready: true}
	report.Spec.Facts.GPU = &schema.GPUFacts{
		Nodes: 3, MatchingNodes: 2, MIGNodes: 1,
		Nvproxy: &schema.NvproxyFacts{RunscVersion: "release-20261116.0", SupportedDrivers: []string{"550.90.07", "560.35.03"}},
		Groups: []schema.GPUNodeGroup{
			{Product: "Tesla-T4", Driver: "550.90.07", Nodes: 2, MatchingNodes: 2,
				ProductSupport: schema.SupportSupported, DriverSupport: schema.SupportSupported},
			{Driver: "999.1", MIG: true, Nodes: 1,
				ProductSupport: schema.SupportUnknown, DriverSupport: schema.SupportUnsupported},
		},
	}

	out := renderMarkdown(t, report, nil)
	for _, want := range []string{
		"Probe: default/agentmoat-probe ran on node gpu-1",
		"- GPU: 3 node(s), 2 match the nodeSelector, 1 with MIG; nvproxy: runsc `release-20261116.0`, 2 supported driver(s).",
		"  - `Tesla-T4`, driver `550.90.07`: 2 node(s) (2 match), card support `supported`, driver support `supported`",
		"  - no GFD labels, driver `999.1`, MIG: 1 node(s) (0 match), card support `unknown`, driver support `unsupported`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}

	// GPU nodes and no probe: the report says how to get the driver list.
	report.Spec.Facts.GPU.Nvproxy = nil
	if out := renderMarkdown(t, report, nil); !strings.Contains(out, "nvproxy is not probed (run `agentmoat probe nvproxy`).") {
		t.Errorf("with no probe result, the GPU line must say so:\n%s", out)
	}
	// No GPU nodes: no GPU line.
	report.Spec.Facts.GPU = nil
	if out := renderMarkdown(t, report, nil); strings.Contains(out, "GPU") {
		t.Errorf("a cluster with no GPU nodes must have no GPU line:\n%s", out)
	}
}

// TestPipelineMarkdown_HostileText: the same rule for the other documents.
// An error string from an API server, a warning, and a finding are text
// that this tool did not write.
func TestPipelineMarkdown_HostileText(t *testing.T) {
	t.Parallel()
	target := schema.WorkloadRef{Kind: hostile("kind"), Namespace: hostile("namespace"), Name: hostile("name")}

	plan := schema.NewMigrationPlan()
	plan.Metadata.PlanHash = hostile("hash")
	plan.Metadata.Cluster = hostile("cluster")
	plan.Spec.Options.RuntimeClassName = hostile("class")
	plan.Spec.Warnings = []schema.PlanWarning{{ID: hostile("id"), Message: hostile("message")}}
	plan.Spec.Steps = []schema.PlanStep{{Order: 1, Target: target, WaitFor: hostile("wait"), Notes: hostile("notes")}}
	plan.Spec.Excluded = []schema.ExcludedWorkload{{Target: target, Compatibility: schema.Compatibility(hostile("verdict")), Reason: hostile("reason")}}
	assertInert(t, renderMarkdown(t, plan, nil), []string{"## agentmoat plan", "### Warnings (1)", "### Steps (1)"})

	apply := schema.NewApplyResult()
	apply.Metadata.PlanHash = hostile("hash")
	apply.Metadata.Cluster = hostile("cluster")
	apply.Spec.Summary = schema.ApplySummary{Total: 2, Applied: 1, Failed: 1}
	apply.Spec.PreflightFindings = []schema.PreflightFinding{{
		ID: hostile("id"), Severity: schema.Severity(hostile("severity")), Message: hostile("message"), Remediation: hostile("fix"),
	}}
	apply.Spec.Steps = []schema.StepResult{
		{Order: 1, Target: target, Status: schema.StepStatusFailed, Error: hostile("error")},
		{Order: 2, Target: target, Status: schema.StepStatus(hostile("status")), Patch: hostile("patch")},
	}
	assertInert(t, renderMarkdown(t, apply, nil),
		[]string{"## agentmoat apply", "### Preflight findings (1)", "### Failed or skipped (1)"})

	verify := schema.NewVerifyReport()
	verify.Metadata.PlanHash = hostile("hash")
	verify.Spec.Summary = schema.VerifySummary{Total: 1, Mismatch: 1}
	verify.Spec.Results = []schema.VerifyResult{{
		Order: 1, Target: target, Status: schema.VerifyStatusMismatch,
		Expected: hostile("expected"), Actual: hostile("actual"), Message: hostile("message"),
	}}
	assertInert(t, renderMarkdown(t, verify, nil), []string{"## agentmoat verify", "### Mismatch or error (1)"})

	preflight := schema.NewPreflightReport()
	preflight.Metadata.RuntimeClassName = hostile("class")
	preflight.Metadata.Cluster = hostile("cluster")
	preflight.Spec.Facts.RuntimeClass = schema.RuntimeClassFacts{
		Name: hostile("name"), Found: true, Handler: hostile("handler"),
		NodeSelector: map[string]string{hostile("key"): hostile("value")},
	}
	preflight.Spec.Findings = []schema.PreflightFinding{{
		ID: hostile("id"), Severity: schema.SeverityError, Message: hostile("message"), Remediation: hostile("fix"),
	}}
	// Node labels and the output of a binary on the node.
	preflight.Metadata.Probe = &schema.ProbeMetadata{Namespace: hostile("ns"), PodName: hostile("pod"), Node: hostile("node"), Succeeded: true}
	preflight.Spec.Facts.GPU = &schema.GPUFacts{
		Nodes:   1,
		Nvproxy: &schema.NvproxyFacts{RunscVersion: hostile("runsc")},
		Groups: []schema.GPUNodeGroup{{
			Product: hostile("product"), Driver: hostile("driver"), Nodes: 1,
			ProductSupport: schema.SupportStatus(hostile("card")), DriverSupport: schema.SupportStatus(hostile("support")),
		}},
	}
	assertInert(t, renderMarkdown(t, preflight, nil), []string{"## agentmoat preflight", "### Findings (1)", "### Facts"})
}
