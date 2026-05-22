// Integration test for cmd/agentmoat-mcp.
//
// The unit suite drives handlers via the in-process server. This test
// instead BUILDS the binary and pipes JSON-RPC messages through its
// stdin/stdout, so it catches anything the unit tests can't (flag
// parsing, framing, encoding/decoding round-trip on the wire).
//
// We use the offline `explain` tool because it does not need a cluster.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0hardik1/agentmoat/internal/schema"
)

// rpc is the minimal JSON-RPC envelope we serialize on the wire.
type rpc struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// buildBinary compiles the MCP binary into a temp dir and returns the
// path. Using `go build` keeps the integration test self-contained: it
// doesn't depend on `make build` having been run first.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agentmoat-mcp")
	// #nosec G204 -- args are test-controlled literals (bin path is from
	// t.TempDir), no user input.
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v", err)
	}
	return bin
}

// rpcSession spawns the MCP binary and exposes a thin "send/recv"
// surface for the test. The Close() returns when stdin is closed and
// the binary exits.
type rpcSession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
}

func startSession(t *testing.T, bin string) *rpcSession {
	t.Helper()
	// #nosec G204 -- bin is built by buildBinary into t.TempDir(); not user input.
	cmd := exec.Command(bin, "--log-level=silent")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return &rpcSession{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout)}
}

// send writes one JSON-RPC message (followed by a newline, as the stdio
// transport requires).
func (s *rpcSession) send(t *testing.T, method string, id int, params any) {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("marshal params: %v", err)
	}
	msg := rpc{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := s.stdin.Write(append(out, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// sendNotification writes one JSON-RPC notification (no id) so the
// server's initialized handshake completes.
func (s *rpcSession) sendNotification(t *testing.T, method string, params any) {
	t.Helper()
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal params: %v", err)
		}
		raw = b
	}
	msg := rpc{JSONRPC: "2.0", Method: method, Params: raw}
	out, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := s.stdin.Write(append(out, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// recv reads one JSON-RPC line and returns it parsed.
func (s *rpcSession) recv(t *testing.T) rpc {
	t.Helper()
	line, err := s.stdout.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var r rpc
	if err := json.Unmarshal(line, &r); err != nil {
		t.Fatalf("decode: %v\nline: %s", err, line)
	}
	return r
}

func (s *rpcSession) close(t *testing.T) {
	t.Helper()
	_ = s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = s.cmd.Process.Kill()
		t.Fatalf("binary did not exit after stdin close")
	}
}

// TestIntegration_ExplainOverStdio drives initialize + tools/call(explain)
// against the actual binary. This is the smallest end-to-end check that
// exercises flag parsing, stdio framing, schema decode, and the JSON
// result-body shape, with no cluster dependency.
func TestIntegration_ExplainOverStdio(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped under -short")
	}
	bin := buildBinary(t)
	s := startSession(t, bin)
	defer s.close(t)

	// 1. initialize
	s.send(t, "initialize", 1, map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "mcp-integration-test", "version": "0"},
	})
	if r := s.recv(t); r.ID != 1 || len(r.Result) == 0 {
		t.Fatalf("initialize response unexpected: %+v", r)
	}

	// 2. notifications/initialized (no response expected).
	s.sendNotification(t, "notifications/initialized", map[string]any{})

	// 3. tools/call explain (offline; no cluster).
	s.send(t, "tools/call", 2, map[string]any{
		"name":      "explain",
		"arguments": map[string]any{"topic": "gvisor"},
	})
	r := s.recv(t)
	if r.ID != 2 {
		t.Fatalf("tools/call: id mismatch: %+v", r)
	}
	if len(r.Result) == 0 {
		t.Fatalf("tools/call: empty result")
	}

	// 4. Drill into the result: it's a CallToolResult whose content[0].text is
	//    the JSON-encoded schema.ExplainDocument. Use a permissive
	//    intermediate type to avoid pulling mcp-go's CallToolResult into the
	//    decoded path (which carries non-trivial polymorphism).
	var outer struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent any `json:"structuredContent,omitempty"`
	}
	if err := json.Unmarshal(r.Result, &outer); err != nil {
		t.Fatalf("decode result: %v\nresult: %s", err, r.Result)
	}
	if len(outer.Content) == 0 || outer.Content[0].Type != "text" {
		t.Fatalf("expected text content, got %+v", outer.Content)
	}
	var doc schema.ExplainDocument
	if err := json.Unmarshal([]byte(outer.Content[0].Text), &doc); err != nil {
		t.Fatalf("decode ExplainDocument: %v\ntext: %s", err, outer.Content[0].Text)
	}
	if doc.Kind != schema.KindExplainDocument {
		t.Errorf("Kind: got %q, want %q", doc.Kind, schema.KindExplainDocument)
	}
	if doc.Spec.Content == "" {
		t.Errorf("expected non-empty Spec.Content for topic 'gvisor'")
	}
}

// TestIntegration_Help confirms `--help` prints something sensible on
// stdout and exits 0. Adds a small amount of coverage on main.go's flag
// parsing.
func TestIntegration_Help(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped under -short")
	}
	bin := buildBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// #nosec G204 -- bin is built by buildBinary into t.TempDir().
	out, err := exec.CommandContext(ctx, bin, "--help").Output()
	if err != nil {
		t.Fatalf("--help: %v", err)
	}
	if !strings.Contains(string(out), "stdio") {
		t.Errorf("--help should mention stdio mode; got:\n%s", out)
	}
}

// TestIntegration_Version confirms `--version` prints something and exits 0.
func TestIntegration_Version(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped under -short")
	}
	bin := buildBinary(t)
	// #nosec G204 -- bin is built by buildBinary into t.TempDir().
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Fatalf("--version: %v", err)
	}
	if !strings.Contains(string(out), "agentmoat-mcp") {
		t.Errorf("--version should mention the binary name; got: %s", out)
	}
}
