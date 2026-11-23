// Unit tests for `agentmoat verify`. The deep behavior (per-step status
// matrix, in-pod-probe seam) is exercised by pkg/verifier; the CLI tests
// here just confirm flag wiring, help text, and the exit-code-shaping
// helper.
package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// TestVerifyHelpListsFlags confirms `agentmoat verify --help` mentions the
// two subcommand-local flags the user needs to discover (--plan,
// --in-pod-probe).
func TestVerifyHelpListsFlags(t *testing.T) {
	stdout, _, err := runRoot("verify", "--help")
	if err != nil {
		t.Fatalf("verify --help: %v", err)
	}
	wantFlags := []string{"--plan", "--in-pod-probe"}
	for _, f := range wantFlags {
		if !strings.Contains(stdout, f) {
			t.Errorf("verify --help should mention %q", f)
		}
	}
}

// TestVerifyRequiresPlan asserts cobra's MarkFlagRequired is wired up:
// `agentmoat verify` with no --plan errors out before any K8s call.
func TestVerifyRequiresPlan(t *testing.T) {
	_, _, err := runRoot("verify")
	if err == nil {
		t.Fatalf("expected error when --plan is missing")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "plan") {
		t.Errorf("error should mention --plan: %v", err)
	}
}

// TestHasVerifyFailures pins the exit-code-shaping helper. The CLI uses
// these summary counts to choose between exit 0 and exit 4.
func TestHasVerifyFailures(t *testing.T) {
	cases := []struct {
		name string
		s    schema.VerifySummary
		want bool
	}{
		{"all_ok", schema.VerifySummary{Total: 2, OK: 2}, false},
		{"mismatch", schema.VerifySummary{Total: 2, OK: 1, Mismatch: 1}, true},
		{"error", schema.VerifySummary{Total: 2, OK: 1, Error: 1}, true},
		{"both", schema.VerifySummary{Total: 3, OK: 1, Mismatch: 1, Error: 1}, true},
		{"empty", schema.VerifySummary{}, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := &schema.VerifyReport{Spec: schema.VerifySpec{Summary: tc.s}}
			if got := hasVerifyFailures(r); got != tc.want {
				t.Errorf("hasVerifyFailures(%+v) = %v, want %v", tc.s, got, tc.want)
			}
		})
	}
}

// TestVerify_WaitFlag: --wait takes a Go duration. A value that is not one,
// and a negative one, are errors before the plan file or the cluster is
// read. What the verifier does with a wait time is tested in pkg/verifier
// (wait_test.go).
func TestVerify_WaitFlag(t *testing.T) {
	deadCluster(t)
	missing := filepath.Join(t.TempDir(), "no-such-plan.json")

	tests := []struct {
		name    string
		wait    string
		wantErr string
	}{
		{"not a duration", "soon", `invalid argument "soon" for "--wait"`},
		{"a number with no unit", "180", `invalid argument "180" for "--wait"`},
		{"negative", "-5s", "the wait time is -5s; it must be zero or more"},
		// A good value gets past the flag, and then fails on the plan
		// file. That is a later error, and it is the one to expect.
		{"a good value", "3m", "no-such-plan.json"},
		{"zero is no wait", "0", "no-such-plan.json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := runRoot("verify", "--plan", missing, "--wait", tc.wait)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestVerifyHelp_ExplainsWait: the help says what a wait changes (pods must
// be Ready), because that changes what exit code 4 means.
func TestVerifyHelp_ExplainsWait(t *testing.T) {
	stdout, _, err := runRoot("verify", "--help")
	if err != nil {
		t.Fatalf("verify --help: %v", err)
	}
	for _, want := range []string{"--wait duration", "stay Ready", "pod template", "rollback --plan plan.json --dry-run=false"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("verify --help lacks %q:\n%s", want, stdout)
		}
	}
}

// TestCLIInstallsNoSignalHandler: no file of the CLI imports os/signal.
//
// This is a safety property, not a style rule. A pipeline runs
//
//	agentmoat verify --plan plan.json --wait 3m || agentmoat rollback --plan plan.json --dry-run=false
//
// A shell goes on with the right side of that list when the left side
// returned an exit code. It stops the full list when the left side died by a
// signal. If the CLI handles SIGINT or SIGTERM and then returns (a wait that
// was cut short always has a step that is not ok, so exit 4), Ctrl-C on a
// healthy rollout starts a live rollback. With no handler, the Go runtime
// ends the process by the signal.
//
// If a command needs to clean up on an interrupt, it must end by the same
// signal after the cleanup (reset the handler, then send the signal to its
// own process), and this test must then check that instead.
func TestCLIInstallsNoSignalHandler(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v (%d files)", err, len(files))
	}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		src, err := os.ReadFile(file) //nolint:gosec // a file of this package, from the glob above
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		parsed, err := parser.ParseFile(fset, file, src, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		for _, imp := range parsed.Imports {
			if imp.Path.Value == `"os/signal"` {
				t.Errorf("%s imports os/signal: an interrupt must end the process by the signal (see the comment on this test)", file)
			}
		}
	}
}
