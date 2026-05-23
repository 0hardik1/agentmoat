#!/usr/bin/env bash
# agentmoat: verify pinned gVisor release tags resolve on storage.googleapis.com
# and that packer/kind defaults stay in sync.
#
# Usage:
#   ./scripts/check-gvisor-version.sh
#   GVISOR_VERSION=20250811.0 ./scripts/check-gvisor-version.sh
#
# Exit codes:
#   0  all checks passed
#   1  mismatch, bad tag format, or download URL unreachable

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
PACKER_HCL="$REPO_ROOT/packer/eks-gvisor-al2023.pkr.hcl"
KIND_BUILD="$REPO_ROOT/scripts/build-gvisor-node.sh"
KIND_DOCKERFILE="$REPO_ROOT/kind/Dockerfile.gvisor-node"

read_default() {
  local file="$1" pattern="$2"
  local line
  line="$(grep -E "$pattern" "$file" | head -1)"
  if [[ "$line" =~ ([0-9]{8}\.[0-9]+) ]]; then
    echo "${BASH_REMATCH[1]}"
  else
    echo "error: could not parse gVisor version default from $file (matched: ${line:-<none>})" >&2
    exit 1
  fi
}

PACKER_VERSION="$(read_default "$PACKER_HCL" 'default[[:space:]]*=[[:space:]]*"[0-9]{8}\.[0-9]+"')"
KIND_SCRIPT_VERSION="$(read_default "$KIND_BUILD" 'GVISOR_VERSION:-[0-9]{8}\.[0-9]+')"
KIND_DOCKER_VERSION="$(read_default "$KIND_DOCKERFILE" 'ARG GVISOR_VERSION=[0-9]{8}\.[0-9]+')"

if [[ "$PACKER_VERSION" != "$KIND_SCRIPT_VERSION" || "$PACKER_VERSION" != "$KIND_DOCKER_VERSION" ]]; then
  echo "error: gVisor version defaults diverged:" >&2
  echo "  packer/eks-gvisor-al2023.pkr.hcl: $PACKER_VERSION" >&2
  echo "  scripts/build-gvisor-node.sh:     $KIND_SCRIPT_VERSION" >&2
  echo "  kind/Dockerfile.gvisor-node:      $KIND_DOCKER_VERSION" >&2
  exit 1
fi

GVISOR_VERSION="${GVISOR_VERSION:-$PACKER_VERSION}"

if [[ "$GVISOR_VERSION" == release-* ]]; then
  echo "error: GVISOR_VERSION must not include the runsc --version 'release-' prefix (got '$GVISOR_VERSION')" >&2
  exit 1
fi

if [[ ! "$GVISOR_VERSION" =~ ^[0-9]{8}\.[0-9]+$ ]]; then
  echo "error: GVISOR_VERSION must match yyyymmdd.RC (got '$GVISOR_VERSION')" >&2
  exit 1
fi

check_arch_url() {
  local arch="$1"
  local url="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION}/${arch}/runsc"
  if curl --fail --silent --show-error --location --output /dev/null --head "$url"; then
    echo "ok  $url"
  else
    echo "error: gVisor release not found at $url" >&2
    exit 1
  fi
}

echo "checking gVisor release $GVISOR_VERSION (packer/kind default: $PACKER_VERSION)..."
check_arch_url x86_64
check_arch_url aarch64
