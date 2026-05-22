// Unit tests for parseWorkloadRef (used by `agentmoat explain workload`).
//
// We deliberately do NOT exercise the full subcommand RunE here: it
// requires a live kube client and lands in the e2e harness instead.
// parseWorkloadRef is pure-string and the single piece of CLI logic that
// can fail without ever reaching the cluster, so it is the highest-value
// unit-test target on this surface.
package main

import (
	"strings"
	"testing"

	"github.com/0hardik1/agentmoat/pkg/agentmoat"
)

// TestParseWorkloadRef is the table-driven test for the
// `<ns>/<name>` and `<ns>/<kind>/<name>` parser.
func TestParseWorkloadRef(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		in         string
		wantNs     string
		wantFilter *agentmoat.WorkloadFilter
		wantErr    bool
		// errSubstr is checked when wantErr is true. When empty, any
		// non-nil error is accepted; otherwise the substring must
		// appear in the error message so we know the help text is
		// useful.
		errSubstr string
	}{
		{
			name:       "two-arg ns/name",
			in:         "payments/api-gateway",
			wantNs:     "payments",
			wantFilter: &agentmoat.WorkloadFilter{Name: "api-gateway"},
		},
		{
			name:       "three-arg ns/kind/name",
			in:         "payments/Deployment/api-gateway",
			wantNs:     "payments",
			wantFilter: &agentmoat.WorkloadFilter{Kind: "Deployment", Name: "api-gateway"},
		},
		{
			name:      "single token rejected",
			in:        "payments",
			wantErr:   true,
			errSubstr: "<namespace>/<name>",
		},
		{
			name:      "empty input rejected",
			in:        "",
			wantErr:   true,
			errSubstr: "empty workload reference",
		},
		{
			name:      "too many slashes rejected",
			in:        "a/b/c/d",
			wantErr:   true,
			errSubstr: "<namespace>/<kind>/<name>",
		},
		{
			name:      "empty namespace rejected",
			in:        "/foo",
			wantErr:   true,
			errSubstr: "non-empty",
		},
		{
			name:      "empty name rejected (two-arg)",
			in:        "ns/",
			wantErr:   true,
			errSubstr: "non-empty",
		},
		{
			name:      "empty kind rejected (three-arg)",
			in:        "ns//foo",
			wantErr:   true,
			errSubstr: "non-empty",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ns, filter, err := parseWorkloadRef(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseWorkloadRef(%q): want error, got ns=%q filter=%+v", tc.in, ns, filter)
				}
				if tc.errSubstr != "" && !strings.Contains(err.Error(), tc.errSubstr) {
					t.Errorf("parseWorkloadRef(%q): error %q should contain %q", tc.in, err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWorkloadRef(%q): unexpected error: %v", tc.in, err)
			}
			if ns != tc.wantNs {
				t.Errorf("ns: got %q, want %q", ns, tc.wantNs)
			}
			if filter == nil {
				t.Fatalf("filter: got nil, want %+v", tc.wantFilter)
			}
			if filter.Kind != tc.wantFilter.Kind {
				t.Errorf("filter.Kind: got %q, want %q", filter.Kind, tc.wantFilter.Kind)
			}
			if filter.Name != tc.wantFilter.Name {
				t.Errorf("filter.Name: got %q, want %q", filter.Name, tc.wantFilter.Name)
			}
		})
	}
}

// TestExplainWorkloadHelp confirms the subcommand registers under
// `agentmoat explain` and its help text mentions both supported forms,
// so an operator who runs --help can discover the syntax.
func TestExplainWorkloadHelp(t *testing.T) {
	stdout, _, err := runRoot("explain", "workload", "--help")
	if err != nil {
		t.Fatalf("explain workload --help: %v", err)
	}
	for _, want := range []string{
		"<namespace>/<name>",
		"<namespace>/<kind>/<name>",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help should mention %q: %s", want, stdout)
		}
	}
}

// TestExplainNamespaceHelp confirms the sibling namespace subcommand is
// also wired in. The help text lives in explain_namespace.go but the
// wiring (AddCommand) is checked here so a regression that drops the
// AddCommand call fails this test.
func TestExplainNamespaceHelp(t *testing.T) {
	stdout, _, err := runRoot("explain", "namespace", "--help")
	if err != nil {
		t.Fatalf("explain namespace --help: %v", err)
	}
	for _, want := range []string{
		"namespace <name>",
		"per-workload explanation",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("help should mention %q: %s", want, stdout)
		}
	}
}
