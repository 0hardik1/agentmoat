// Tests for the audit package. Each test points AGENTMOAT_AUDIT_PATH at a
// temp file so the developer's real ~/.agentmoat/audit.jsonl is never
// touched.
package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
)

func TestAppend_WritesJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	t.Setenv(EnvPathOverride, path)

	e := Entry{
		Action:   "apply",
		DryRun:   true,
		PlanHash: "abcd1234",
		Workload: schema.WorkloadRef{
			Kind:      "Deployment",
			Namespace: "default",
			Name:      "web",
		},
		Status: schema.StepStatusApplied,
	}

	written, err := Append(e)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if written != path {
		t.Errorf("path: got %q want %q", written, path)
	}

	// Append a second line to confirm O_APPEND keeps both.
	e2 := e
	e2.Workload.Name = "api"
	if _, err := Append(e2); err != nil {
		t.Fatalf("Append #2: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open audit file: %v", err)
	}
	defer f.Close()

	var lines []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var got Entry
		if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal audit line %q: %v", sc.Text(), err)
		}
		lines = append(lines, got)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanner: %v", err)
	}

	if len(lines) != 2 {
		t.Fatalf("line count: got %d want 2", len(lines))
	}
	if lines[0].Workload.Name != "web" || lines[1].Workload.Name != "api" {
		t.Errorf("order: got %q,%q want web,api", lines[0].Workload.Name, lines[1].Workload.Name)
	}
	if lines[0].Timestamp.IsZero() {
		t.Errorf("Timestamp not populated when zero")
	}
}

func TestAppend_PreservesProvidedTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	t.Setenv(EnvPathOverride, path)

	when := time.Date(2026, 5, 22, 11, 11, 11, 0, time.UTC)
	e := Entry{
		Timestamp: when,
		Action:    "rollback",
		Status:    schema.StepStatusApplied,
	}
	if _, err := Append(e); err != nil {
		t.Fatalf("Append: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got Entry
	if err := json.Unmarshal(data[:len(data)-1], &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Timestamp.Equal(when) {
		t.Errorf("Timestamp: got %v want %v", got.Timestamp, when)
	}
}

func TestResolvePath_HonorsEnv(t *testing.T) {
	t.Setenv(EnvPathOverride, "/tmp/custom-audit.jsonl")
	got, err := ResolvePath()
	if err != nil {
		t.Fatalf("ResolvePath: %v", err)
	}
	if got != "/tmp/custom-audit.jsonl" {
		t.Errorf("path: got %q want %q", got, "/tmp/custom-audit.jsonl")
	}
}
