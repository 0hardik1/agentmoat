// Server-assembly drift sentinel. If a future contributor adds, renames,
// or deletes a tool/resource/prompt, this test fails first with a clear
// diff: every public surface element of the MCP server is enumerated
// here. Update the wantX slices when intentionally adding new ones.
package main

import (
	"io"
	"sort"
	"strings"
	"testing"
)

func TestNewServer_RegistersAllTools(t *testing.T) {
	t.Parallel()
	srv := newServer(Deps{Stderr: io.Discard, Version: "test"})

	want := []string{
		"apply_plan",
		"assess_workload",
		"explain",
		"preflight_cluster",
		"probe_nvproxy",
		"propose_plan",
		"rollback_plan",
		"scan_cluster",
		"verify_migration",
	}
	got := make([]string, 0, len(srv.ListTools()))
	for name := range srv.ListTools() {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tool set drift:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestNewServer_RegistersAllResources(t *testing.T) {
	t.Parallel()
	srv := newServer(Deps{Stderr: io.Discard, Version: "test"})

	want := []string{
		"agentmoat://compatibility-rules",
		"agentmoat://known-gotchas",
	}
	got := make([]string, 0, len(srv.ListResources()))
	for uri := range srv.ListResources() {
		got = append(got, uri)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("resource set drift:\n  got:  %v\n  want: %v", got, want)
	}
}

func TestNewServer_RegistersAllPrompts(t *testing.T) {
	t.Parallel()
	srv := newServer(Deps{Stderr: io.Discard, Version: "test"})

	want := []string{"audit-cluster-for-agentic-workloads"}
	got := make([]string, 0, len(srv.ListPrompts()))
	for name := range srv.ListPrompts() {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("prompt set drift:\n  got:  %v\n  want: %v", got, want)
	}
}
