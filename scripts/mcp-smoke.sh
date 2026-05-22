#!/usr/bin/env bash
# scripts/mcp-smoke.sh: end-to-end smoke test for cmd/agentmoat-mcp.
#
# What this script does
#
#   1. Confirms an existing kind cluster context is reachable
#      (NAMESPACE/CLUSTER_NAME env vars mirror scripts/e2e.sh's defaults).
#   2. Spawns ./bin/agentmoat-mcp with --log-level=silent so only JSON-RPC
#      traffic lands on stdout.
#   3. Sends a minimal JSON-RPC session:
#        initialize -> notifications/initialized -> tools/call scan_cluster
#        -> tools/call propose_plan
#   4. Asserts the tool-result bodies parse with Kind=ScanReport and
#      Kind=MigrationPlan respectively.
#
# Why this script lives outside scripts/e2e.sh
#
#   make e2e is the CLI's end-to-end harness; coupling it to the MCP-side
#   regressions would make a stable CLI release more brittle. Keeping the
#   MCP smoke in its own target (make mcp-smoke) lets the CLI suite pass
#   without kind support for the MCP binary, and vice versa.
#
# Exit codes
#
#   0  smoke passed
#   1  prerequisite missing (binary, jq, kind context)
#   2  JSON-RPC assertion failed

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-agentmoat-e2e}"
NAMESPACE="${NAMESPACE:-agentmoat-e2e}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AGENTMOAT_MCP_BIN="${AGENTMOAT_MCP_BIN:-$REPO_ROOT/bin/agentmoat-mcp}"
CTX="kind-$CLUSTER_NAME"

# -----------------------------------------------------------------------------
# Prerequisite checks
# -----------------------------------------------------------------------------

if [[ ! -x "$AGENTMOAT_MCP_BIN" ]]; then
  echo "ERROR: agentmoat-mcp binary not found at $AGENTMOAT_MCP_BIN" >&2
  echo "Hint: run 'make build' first." >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq is required for assertions; install it and retry." >&2
  exit 1
fi

if ! kubectl --context "$CTX" get nodes >/dev/null 2>&1; then
  echo "ERROR: kind context '$CTX' not reachable." >&2
  echo "Hint: run 'make kind-up' first (this script does NOT bring kind up)." >&2
  exit 1
fi

# -----------------------------------------------------------------------------
# Drive the JSON-RPC session
# -----------------------------------------------------------------------------

# Build the request stream in a temp file rather than a heredoc piped into
# the binary; mcp-go reads one line per message and using a file lets us
# tee the request log for debugging.
REQ_FILE="$(mktemp -t agentmoat-mcp-req.XXXXXX)"
RESP_FILE="$(mktemp -t agentmoat-mcp-resp.XXXXXX)"
trap 'rm -f "$REQ_FILE" "$RESP_FILE"' EXIT

cat > "$REQ_FILE" <<EOF
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"mcp-smoke","version":"0"}}}
{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}
{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"scan_cluster","arguments":{"context":"$CTX","namespaces":["$NAMESPACE"]}}}
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"propose_plan","arguments":{"context":"$CTX","namespaces":["$NAMESPACE"]}}}
EOF

# Drive the binary. --log-level=silent keeps stderr quiet so the JSON-RPC
# transcript on stdout is uncluttered.
"$AGENTMOAT_MCP_BIN" --log-level=silent < "$REQ_FILE" > "$RESP_FILE"

# -----------------------------------------------------------------------------
# Assertions
# -----------------------------------------------------------------------------

# Each tools/call response carries the schema struct as JSON text inside
# .result.content[0].text. The schema-level "kind" is the load-bearing
# field: if it parses, scan_cluster -> "ScanReport", propose_plan -> "MigrationPlan".
SCAN_KIND="$(jq -r 'select(.id==2) | .result.content[0].text | fromjson | .kind' "$RESP_FILE")"
if [[ "$SCAN_KIND" != "ScanReport" ]]; then
  echo "FAIL: scan_cluster: expected kind=ScanReport, got kind='$SCAN_KIND'" >&2
  echo "----- response file -----" >&2
  cat "$RESP_FILE" >&2
  exit 2
fi

PLAN_KIND="$(jq -r 'select(.id==3) | .result.content[0].text | fromjson | .kind' "$RESP_FILE")"
if [[ "$PLAN_KIND" != "MigrationPlan" ]]; then
  echo "FAIL: propose_plan: expected kind=MigrationPlan, got kind='$PLAN_KIND'" >&2
  echo "----- response file -----" >&2
  cat "$RESP_FILE" >&2
  exit 2
fi

echo "mcp-smoke OK (scan_cluster -> ScanReport, propose_plan -> MigrationPlan)"
