// Unit tests for `agentmoat verify`. The deep behaviour (per-step status
// matrix, in-pod-probe seam) is exercised by pkg/verifier; the CLI tests
// here just confirm flag wiring, help text, and the exit-code-shaping
// helper.
package main

import (
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
