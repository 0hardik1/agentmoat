#!/usr/bin/env bash
# agentmoat: tear down the local kind cluster created by scripts/kind-up.sh.
#
# Idempotent: missing cluster is not an error.
#
# Environment overrides:
#   CLUSTER_NAME  cluster name (default: agentmoat-e2e)

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-agentmoat-e2e}"

if ! command -v kind >/dev/null 2>&1; then
  echo "error: kind not installed." >&2
  exit 1
fi

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "kind cluster '$CLUSTER_NAME' not present; nothing to delete."
  exit 0
fi

echo "deleting kind cluster '$CLUSTER_NAME'..."
kind delete cluster --name "$CLUSTER_NAME"
echo "kind cluster '$CLUSTER_NAME' deleted."
