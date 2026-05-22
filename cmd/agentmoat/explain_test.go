// Unit tests for `agentmoat explain`. We test the subcommand wiring (flag
// parsing, output format dispatch, exit-code shaping) by spawning a fresh
// root command in-process and asserting on the captured stdout/stderr. No
// child binary is built; the tests run as part of `go test ./cmd/...`.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/pkg/explainer"
)

// runRoot drives the root command with the given args. Returns
// stdout/stderr/err for assertion. Mirrors what a user would see from a real
// binary invocation but in-process so we sidestep build/path concerns.
func runRoot(args ...string) (stdout, stderr string, err error) {
	// Reset the package-global exitCode between cases so tests stay
	// independent.
	exitCode = 0

	cmd := newRootCmd()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

// TestExplainHelpListsTopicArg confirms `agentmoat explain --help` mentions
// the optional positional [topic] argument so users discover the API.
func TestExplainHelpListsTopicArg(t *testing.T) {
	stdout, _, err := runRoot("explain", "--help")
	if err != nil {
		t.Fatalf("explain --help: %v", err)
	}
	if !strings.Contains(stdout, "[topic]") {
		t.Errorf("explain --help should mention [topic]: %s", stdout)
	}
}

// TestExplainNoTopicListsTopics confirms `agentmoat explain` (no arg) exits 0
// and prints the topic vocabulary on stdout. The e2e relies on
// "runtimeclass" appearing in the output.
func TestExplainNoTopicListsTopics(t *testing.T) {
	stdout, _, err := runRoot("explain")
	if err != nil {
		t.Fatalf("explain: unexpected error: %v", err)
	}
	if exitCode != 0 {
		t.Errorf("exit code should be 0, got %d", exitCode)
	}
	for _, topic := range explainer.Topics() {
		if !strings.Contains(stdout, topic) {
			t.Errorf("stdout should list topic %q: %s", topic, stdout)
		}
	}
}

// TestExplainKnownTopicPrintsMarkdown confirms a valid topic prints content
// starting with "# " (the markdown title).
func TestExplainKnownTopicPrintsMarkdown(t *testing.T) {
	stdout, _, err := runRoot("explain", "runtimeclass")
	if err != nil {
		t.Fatalf("explain runtimeclass: %v", err)
	}
	if !strings.HasPrefix(stdout, "# ") {
		t.Errorf("stdout should start with '# ': %s", stdout[:min(80, len(stdout))])
	}
	if len(stdout) < 200 {
		t.Errorf("stdout should be > 200 bytes for a real topic: got %d", len(stdout))
	}
}

// TestExplainUnknownTopicErrors confirms an unknown topic returns a non-nil
// error from Execute() AND the error message lists every valid topic so the
// user can self-correct. The e2e asserts the same.
func TestExplainUnknownTopicErrors(t *testing.T) {
	_, _, err := runRoot("explain", "bogus-topic")
	if err == nil {
		t.Fatalf("expected error for bogus topic")
	}
	for _, topic := range explainer.Topics() {
		if !strings.Contains(err.Error(), topic) {
			t.Errorf("error should mention valid topic %q: %v", topic, err)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
