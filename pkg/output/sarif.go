// Package output: SARIF renderer (ScanReport only).
//
// SARIF 2.1.0 is the format that code scanning tools read: GitHub code
// scanning, and others. A scan of manifests has what SARIF needs: findings,
// each with a rule, a file, and a line. The structs here are written by
// hand. They cover the part of the format this tool writes, so no SARIF
// library is needed.
//
// What is in the output
//
//   - One result for each reason with severity "error" or "warn". An "info"
//     reason is a hint on a workload that is compatible; as an alert it
//     would be noise, so it is left out. The table, Markdown, JSON, and
//     YAML forms have it.
//   - One rule entry for each rule ID that has a result, sorted by ID. The
//     text of a rule entry is the same in every report: the title and the
//     explanation from docs/explanations. It is not the description of a
//     reason, because the classifier can add a sentence about one workload
//     to that. The description is the result message.
//
// The level follows the gate
//
// A result has the level "error" only when its workload makes the gate fail
// (Gate.Failing). Every other result has the level "warning".
//
// The reason is how GitHub treats levels. Its pull-request check for code
// scanning fails, by default, on a new result with the level "error". The
// exit code of `scan --fail-on sandboxed` does not fail on a CNI DaemonSet
// that will never move to gVisor. If the SARIF output gave that DaemonSet
// the level "error", the upload would be a second gate that does not agree
// with the first one. With this rule the two agree: what fails the gate is
// an error, and the rest is a warning. With no Gate, the levels are those
// of the default gate (--fail-on incompatible).
//
// Locations
//
// A result has a file location when the report knows the file (a scan of
// files). The line is the first line of the object, not the line of the
// field that the rule matched: the report does not record that line. A
// result always has a logical location too (namespace/Kind/name), which is
// all that a cluster scan, or a scan of standard input, can give. GitHub
// code scanning shows a result only when it has a file location, so upload
// SARIF from a scan of files. SARIFLocations counts the results that have
// none; the CLI prints a note from it.
//
// Fingerprints
//
// Each result has a fingerprint under the key "agentmoatFinding/v1": a hash
// of the rule and the workload identity. It does not change when the file
// is edited or the object moves in it. GitHub uses its own key
// (primaryLocationLineHash), which its upload action adds from the file
// content when the SARIF file has none; that action keeps other keys.

package output

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/0hardik1/agentmoat/docs"
	"github.com/0hardik1/agentmoat/internal/schema"
)

const (
	// sarifSchemaURL is the JSON schema that the document says it follows.
	sarifSchemaURL = "https://json.schemastore.org/sarif-2.1.0.json"
	// sarifVersion is the only SARIF version that GitHub code scanning reads.
	sarifVersion = "2.1.0"
	// sarifToolName and sarifToolURL identify this tool in the run.
	sarifToolName = "agentmoat"
	sarifToolURL  = "https://github.com/0hardik1/agentmoat"
	// sarifFingerprintKey names the fingerprint this tool writes. The "/v1"
	// is the version of the way it is computed (SARIF asks for one).
	sarifFingerprintKey = "agentmoatFinding/v1"
	// sarifSourceRoot is the base that relative file paths are relative
	// to. SARIF wants a relative URI to name its base; "%SRCROOT%" is the
	// conventional name for the root of the repository.
	sarifSourceRoot = "%SRCROOT%"
	// sarifMaxText is under the 1024 character limit that GitHub sets for
	// the description texts of a rule.
	sarifMaxText = 1000
)

// The SARIF document. Only the properties this tool writes.

type sarifLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []sarifRun `json:"runs"`
}

type sarifRun struct {
	Tool sarifTool `json:"tool"`
	// Results has no omitempty and is never nil: a run with no findings
	// must say "results": [], which means "looked, found nothing". A
	// missing or null array is rejected by the GitHub upload.
	Results []sarifResult `json:"results"`
}

type sarifTool struct {
	Driver sarifDriver `json:"driver"`
}

type sarifDriver struct {
	Name           string `json:"name"`
	InformationURI string `json:"informationUri"`
	// Version holds a version that is not a semantic version ("dev").
	// SemanticVersion holds one that is. Only one of the two is set.
	Version         string      `json:"version,omitempty"`
	SemanticVersion string      `json:"semanticVersion,omitempty"`
	Rules           []sarifRule `json:"rules"`
}

type sarifRule struct {
	ID               string              `json:"id"`
	Name             string              `json:"name"`
	ShortDescription sarifText           `json:"shortDescription"`
	FullDescription  sarifText           `json:"fullDescription"`
	Help             sarifHelp           `json:"help"`
	HelpURI          string              `json:"helpUri,omitempty"`
	Properties       sarifRuleProperties `json:"properties"`
}

type sarifText struct {
	Text string `json:"text"`
}

type sarifHelp struct {
	Text     string `json:"text"`
	Markdown string `json:"markdown,omitempty"`
}

type sarifRuleProperties struct {
	Tags []string `json:"tags"`
}

type sarifResult struct {
	RuleID              string                `json:"ruleId"`
	RuleIndex           int                   `json:"ruleIndex"`
	Level               string                `json:"level"`
	Message             sarifText             `json:"message"`
	Locations           []sarifLocation       `json:"locations"`
	PartialFingerprints map[string]string     `json:"partialFingerprints"`
	Properties          sarifResultProperties `json:"properties"`
}

type sarifLocation struct {
	PhysicalLocation *sarifPhysicalLocation `json:"physicalLocation,omitempty"`
	LogicalLocations []sarifLogicalLocation `json:"logicalLocations"`
}

type sarifPhysicalLocation struct {
	ArtifactLocation sarifArtifactLocation `json:"artifactLocation"`
	Region           *sarifRegion          `json:"region,omitempty"`
}

type sarifArtifactLocation struct {
	URI       string `json:"uri"`
	URIBaseID string `json:"uriBaseId,omitempty"`
}

type sarifRegion struct {
	StartLine int `json:"startLine"`
}

type sarifLogicalLocation struct {
	Name               string `json:"name"`
	FullyQualifiedName string `json:"fullyQualifiedName"`
	Kind               string `json:"kind"`
}

// sarifResultProperties carries the workload fields of the report, so a
// tool that reads the SARIF file does not have to parse the message.
type sarifResultProperties struct {
	Kind             string `json:"kind"`
	Namespace        string `json:"namespace,omitempty"`
	Name             string `json:"name"`
	RuntimeClassName string `json:"runtimeClassName,omitempty"`
	Compatibility    string `json:"compatibility"`
	Severity         string `json:"severity"`
}

// renderSARIFAny writes a ScanReport as SARIF. No other document has
// findings with locations; formatKinds[FormatSARIF] says the same, and a
// test checks that the two agree.
func renderSARIFAny(doc any, w io.Writer, opts RenderOptions) error {
	report, ok := doc.(*schema.ScanReport)
	if !ok {
		return fmt.Errorf("output: no SARIF renderer for %T (only a ScanReport has findings with locations)", doc)
	}
	return renderJSONAny(buildSARIF(report, opts.Gate), w)
}

// workloadID is what makes two report rows the same workload. The gate
// works by identity (pkg/agentmoat/gate.go), so the levels do too.
type workloadID struct{ kind, namespace, name string }

// sarifFailing returns the workloads whose error findings get the level
// "error". With a gate, it is the gate's list. With no gate, it is every
// incompatible workload, which is the list of the default gate.
func sarifFailing(report *schema.ScanReport, gate *Gate) map[workloadID]bool {
	failing := map[workloadID]bool{}
	if gate != nil {
		for _, ref := range gate.Failing {
			failing[workloadID{ref.Kind, ref.Namespace, ref.Name}] = true
		}
		return failing
	}
	for _, w := range report.Spec.Workloads {
		if w.Compatibility == schema.CompatibilityIncompatible {
			failing[workloadID{w.Kind, w.Namespace, w.Name}] = true
		}
	}
	return failing
}

// sarifLevel maps a reason to a SARIF level, or "" when the reason is not a
// result. See "The level follows the gate" in the file header.
func sarifLevel(severity schema.Severity, workloadFailsGate bool) string {
	switch severity {
	case schema.SeverityError:
		if workloadFailsGate {
			return "error"
		}
		return "warning"
	case schema.SeverityWarn:
		return "warning"
	}
	return ""
}

// buildSARIF converts a report to a SARIF log with one run.
func buildSARIF(report *schema.ScanReport, gate *Gate) sarifLog {
	failing := sarifFailing(report, gate)

	// The rules first, sorted by ID, so that ruleIndex is known for each
	// result and the list is the same for the same findings.
	seen := map[string]bool{}
	for _, w := range report.Spec.Workloads {
		for _, r := range w.Reasons {
			if sarifLevel(r.Severity, false) != "" {
				seen[r.RuleID] = true
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	rules := make([]sarifRule, 0, len(ids))
	index := make(map[string]int, len(ids))
	for i, id := range ids {
		index[id] = i
		rules = append(rules, sarifRuleFor(id))
	}

	// The results, in report order (the report is sorted).
	results := make([]sarifResult, 0)
	for _, w := range report.Spec.Workloads {
		fails := failing[workloadID{w.Kind, w.Namespace, w.Name}]
		for _, r := range w.Reasons {
			level := sarifLevel(r.Severity, fails)
			if level == "" {
				continue
			}
			results = append(results, sarifResult{
				RuleID:    r.RuleID,
				RuleIndex: index[r.RuleID],
				Level:     level,
				Message: sarifText{Text: sarifPlain(fmt.Sprintf("%s %s: %s",
					w.Kind, sarifWorkloadName(w.Namespace, w.Name), r.Description))},
				Locations: []sarifLocation{sarifLocationFor(w)},
				PartialFingerprints: map[string]string{
					sarifFingerprintKey: sarifFingerprint(r.RuleID, w),
				},
				Properties: sarifResultProperties{
					Kind:             w.Kind,
					Namespace:        w.Namespace,
					Name:             w.Name,
					RuntimeClassName: w.RuntimeClassName,
					Compatibility:    string(w.Compatibility),
					Severity:         string(r.Severity),
				},
			})
		}
	}

	driver := sarifDriver{Name: sarifToolName, InformationURI: sarifToolURL, Rules: rules}
	driver.Version, driver.SemanticVersion = sarifVersions(report.Metadata.AgentmoatVersion)

	return sarifLog{
		Schema:  sarifSchemaURL,
		Version: sarifVersion,
		Runs:    []sarifRun{{Tool: sarifTool{Driver: driver}, Results: results}},
	}
}

// SARIFLocations counts the results that the SARIF form of a report has,
// and how many of them have no file location. A caller uses it to tell the
// user that a code scanning service will not show those results.
func SARIFLocations(report *schema.ScanReport) (results, withoutFile int) {
	for _, w := range report.Spec.Workloads {
		_, _, located := sarifURI(w.Origin)
		for _, r := range w.Reasons {
			if sarifLevel(r.Severity, false) == "" {
				continue
			}
			results++
			if !located {
				withoutFile++
			}
		}
	}
	return results, withoutFile
}

// sarifRuleFor builds the rule entry for a rule ID. All of its text comes
// from the ID (docs.RuleTitle, docs.RuleExplanation), so the entry is the
// same in every report. A rule this build has no explanation for (a report
// from a newer release) gets short text that is still not empty: GitHub
// requires the two descriptions and the help text.
func sarifRuleFor(id string) sarifRule {
	rule := sarifRule{
		ID:               id,
		Name:             sarifRuleName(id),
		ShortDescription: sarifText{Text: mdShorten(docs.RuleTitle(id), sarifMaxText)},
		Properties:       sarifRuleProperties{Tags: []string{"gvisor", "kubernetes"}},
	}
	explanation := docs.RuleExplanation(id)
	if explanation == "" {
		text := fmt.Sprintf("agentmoat rule %s. The message of each result says why the workload matched it.", id)
		rule.FullDescription = sarifText{Text: text}
		rule.Help = sarifHelp{Text: text + " See " + sarifToolURL}
		return rule
	}
	full := firstParagraph(explanation)
	if full == "" {
		full = docs.RuleTitle(id)
	}
	rule.FullDescription = sarifText{Text: mdShorten(full, sarifMaxText)}
	rule.Help = sarifHelp{Text: explanation, Markdown: explanation}
	rule.HelpURI = ruleDocURL(id)
	return rule
}

// sarifRuleName converts a rule ID to the PascalCase name that SARIF uses
// for a rule ("host-network" to "HostNetwork").
func sarifRuleName(id string) string {
	var b strings.Builder
	for _, part := range strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' || r == '.' }) {
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	if b.Len() == 0 {
		return id
	}
	return b.String()
}

// firstParagraph returns the first paragraph of a Markdown text as one
// line: it skips headings and empty lines, then joins lines up to the next
// empty line.
func firstParagraph(markdown string) string {
	var lines []string
	for _, line := range strings.Split(markdown, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" && len(lines) > 0:
			return strings.Join(lines, " ")
		case line == "", strings.HasPrefix(line, "#"):
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, " ")
}

// sarifSemver matches a semantic version (without a leading "v").
var sarifSemver = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// sarifVersions puts the build version in the right one of the two SARIF
// properties. semanticVersion must be a semantic version, and a build from
// source has the version "dev".
func sarifVersions(v string) (version, semanticVersion string) {
	trimmed := strings.TrimPrefix(v, "v")
	if sarifSemver.MatchString(trimmed) {
		return "", trimmed
	}
	return v, ""
}

// sarifWorkloadName renders "namespace/name", or the name alone for a
// manifest that names no namespace.
func sarifWorkloadName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}

// sarifPlainEscaper escapes the characters that have a meaning in a SARIF
// message: the format reads "[text](target)" in a message as a link, and
// says to write a literal bracket with a backslash before it.
var sarifPlainEscaper = strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`)

// sarifPlain makes text from a manifest safe in a SARIF message: control
// characters are replaced (mdScrub), and brackets are escaped, so a
// workload name cannot put a link in the alert that describes it.
func sarifPlain(s string) string {
	return sarifPlainEscaper.Replace(mdScrub(s))
}

// sarifFingerprint is the identity of a finding: the rule and the workload.
// It is the same on every run for the same finding, and does not depend on
// the file, the line, or the text of the description.
func sarifFingerprint(ruleID string, w schema.WorkloadResult) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{ruleID, w.Kind, w.Namespace, w.Name}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

// sarifLocationFor builds the location of a workload: the logical location
// always, and the file when the report knows it.
func sarifLocationFor(w schema.WorkloadResult) sarifLocation {
	qualified := w.Kind + "/" + w.Name
	if w.Namespace != "" {
		qualified = w.Namespace + "/" + qualified
	}
	loc := sarifLocation{
		LogicalLocations: []sarifLogicalLocation{{
			Name:               mdScrub(w.Name),
			FullyQualifiedName: mdScrub(qualified),
			Kind:               "resource",
		}},
	}
	uri, base, ok := sarifURI(w.Origin)
	if !ok {
		return loc
	}
	phys := &sarifPhysicalLocation{ArtifactLocation: sarifArtifactLocation{URI: uri, URIBaseID: base}}
	if w.Origin.Line > 0 {
		phys.Region = &sarifRegion{StartLine: w.Origin.Line}
	}
	loc.PhysicalLocation = phys
	return loc
}

// sarifDrive matches a Windows drive at the start of a path ("C:/").
var sarifDrive = regexp.MustCompile(`^[A-Za-z]:/`)

// sarifURI converts the file of an origin to a URI.
//
// ok is false when there is no file to name: no origin (a cluster scan), or
// a placeholder such as "<stdin>" that is not a path.
//
// A relative path becomes a relative URI with the base "%SRCROOT%". GitHub
// reads it as relative to the root of the repository, so the scan must run
// from that root for the alerts to land on the files. An absolute path
// becomes a file:// URI with no base. Each path segment is
// percent-encoded, so a space or a "#" in a file name is still a valid URI.
func sarifURI(o *schema.Origin) (uri, baseID string, ok bool) {
	if o == nil || o.File == "" || strings.HasPrefix(o.File, "<") {
		return "", "", false
	}
	p := filepath.ToSlash(o.File)
	absolute := strings.HasPrefix(p, "/") || sarifDrive.MatchString(p)
	p = path.Clean(p)
	segments := strings.Split(p, "/")
	for i, seg := range segments {
		seg = url.PathEscape(seg)
		if !absolute {
			// A colon in the first segment of a relative reference
			// would read as a URI scheme. Encode it everywhere; it is
			// valid in every segment in that form.
			seg = strings.ReplaceAll(seg, ":", "%3A")
		}
		segments[i] = seg
	}
	joined := strings.Join(segments, "/")
	if !absolute {
		return joined, sarifSourceRoot, true
	}
	if !strings.HasPrefix(joined, "/") {
		joined = "/" + joined // "C:/x" becomes "file:///C:/x"
	}
	return "file://" + joined, "", true
}
