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
	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

// TestVerifyHelpListsFlags confirms `agentmoat verify --help` mentions the
// two subcommand-local flags the user needs to discover (--plan,
// --in-pod-probe).
func TestVerifyHelpListsFlags(t *testing.T) {
	stdout, _, err := runRoot("verify", "--help")
	if err != nil {
		t.Fatalf("verify --help: %v", err)
	}
	wantFlags := []string{"--plan", "--in-pod-probe", "--filename", "--runtime-class", "--wait", "not checked"}
	for _, f := range wantFlags {
		if !strings.Contains(stdout, f) {
			t.Errorf("verify --help should mention %q", f)
		}
	}
}

// TestVerifyRequiresOneSource: verify needs a plan or manifests, and not the
// two. Each refusal comes before any Kubernetes call: the kubeconfig of the
// test points at no cluster.
func TestVerifyRequiresOneSource(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-kubeconfig"))
	manifests := writeScanFiles(t, map[string]string{"web.yaml": plainWeb})
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"no source", []string{"verify"}, "--plan <plan file>, or -f"},
		{"two sources", []string{"verify", "--plan", "plan.json", "-f", manifests}, "--plan or -f, not the two"},
		{"a class with a plan", []string{"verify", "--plan", "plan.json", "--runtime-class", "kata"}, "--runtime-class is for -f"},
		{"two namespaces", []string{"verify", "-f", manifests, "-n", "a", "-n", "b"}, "takes one namespace"},
		// The input errors of -f come before the client is built: this
		// file has no workload that asks for the class.
		{"nothing asks for the class", []string{"verify", "-f", manifests, "-n", "shop"}, "there is nothing to verify"},
		{"a class name that is not a name", []string{"verify", "-f", manifests, "--runtime-class", "a b"}, "not a valid RuntimeClass name"},
	}
	for _, tc := range tests {
		_, _, err := runRoot(tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to have %q", tc.name, err, tc.want)
		}
	}
}

// TestVerifyExitCode pins the exit code of a report. The CLI and the MCP
// server use the same function. A failure wins over "not checked": evidence
// in the cluster is the rollback signal.
func TestVerifyExitCode(t *testing.T) {
	cases := []struct {
		name string
		s    schema.VerifySummary
		want int
	}{
		{"all_ok", schema.VerifySummary{Total: 2, OK: 2}, 0},
		{"mismatch", schema.VerifySummary{Total: 2, OK: 1, Mismatch: 1}, 4},
		{"error", schema.VerifySummary{Total: 2, OK: 1, Error: 1}, 4},
		{"both", schema.VerifySummary{Total: 3, OK: 1, Mismatch: 1, Error: 1}, 4},
		{"empty", schema.VerifySummary{}, 0},
		{"not_checked", schema.VerifySummary{Total: 2, OK: 2, NotChecked: 1}, 1},
		{"not_checked_only", schema.VerifySummary{NotChecked: 1}, 1},
		{"not_checked_and_mismatch", schema.VerifySummary{Total: 1, Mismatch: 1, NotChecked: 1}, 4},
		{"excluded_is_not_a_failure", schema.VerifySummary{Total: 1, OK: 1, Excluded: 7}, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			r := &schema.VerifyReport{Spec: schema.VerifySpec{Summary: tc.s}}
			if got := agentmoat.VerifyExitCode(r); got != tc.want {
				t.Errorf("VerifyExitCode(%+v) = %d, want %d", tc.s, got, tc.want)
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
