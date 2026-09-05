#!/usr/bin/env bash
# agentmoat: bring up a local kind cluster for development and `make e2e`.
#
# Idempotent: if a cluster named $CLUSTER_NAME already exists, the script
# does nothing and exits 0. This lets `make e2e` re-run quickly while
# iterating on the CLI.
#
# The cluster created here is the real gVisor topology: a stock kind
# control-plane plus a worker node that ships runsc and the containerd
# v2 shim (see kind/Dockerfile.gvisor-node). The worker is labelled
# `runtime=gvisor` so RuntimeClass-pinned pods land there.
#
# Before `kind create` runs, we build the gVisor worker image (via
# scripts/build-gvisor-node.sh) because kind reads the per-node `image:`
# field from kind/cluster.yaml and will fail to start if the image is
# not present locally. The build step is itself idempotent.
#
# Environment overrides:
#   CLUSTER_NAME       cluster name (default: agentmoat-e2e)
#   KIND_CONFIG        path to a kind config (default: kind/cluster.yaml)
#   KIND_IMAGE         node image override (default: kind's bundled default)
#   SKIP_GVISOR_BUILD  set to 1 to skip the gVisor image build (useful if
#                      you've already built it manually and KIND_CONFIG
#                      points at a stock-runc-only config).
#   IMG_TAG, GVISOR_VERSION, KIND_NODE_VERSION  forwarded to the build
#                      script (see scripts/build-gvisor-node.sh).
#
# Exit codes:
#   0  cluster exists (created or already present)
#   1  kind not installed, image build failed, or `kind create cluster`
#      failed

set -euo pipefail

CLUSTER_NAME="${CLUSTER_NAME:-agentmoat-e2e}"
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
KIND_CONFIG="${KIND_CONFIG:-$REPO_ROOT/kind/cluster.yaml}"
SKIP_GVISOR_BUILD="${SKIP_GVISOR_BUILD:-0}"

if ! command -v kind >/dev/null 2>&1; then
  echo "error: kind not installed. See https://kind.sigs.k8s.io/docs/user/quick-start/" >&2
  exit 1
fi

# Short-circuit if the cluster is already up. `kind get clusters` is one
# line per cluster name; grep with anchors avoids substring matches.
#
# Note: an existing cluster keeps whatever node image it was created with.
# After a gVisor pin bump (docs/gvisor-version.md), run `make kind-down`
# first so the next `make kind-up` boots the freshly built image.
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "kind cluster '$CLUSTER_NAME' already exists; reusing."
  exit 0
fi

# Build the gVisor node image first; cluster.yaml references it by tag
# (agentmoat-kind-gvisor:dev). The build script is idempotent and will
# skip the docker build if the image is already cached.
if [[ "$SKIP_GVISOR_BUILD" != "1" ]]; then
  "$REPO_ROOT/scripts/build-gvisor-node.sh"
fi

echo "creating kind cluster '$CLUSTER_NAME'..."
# --wait blocks until the control-plane is ready, so callers can apply
# manifests immediately on return. The gVisor worker takes a few extra
# seconds to register as Ready after the control-plane settles, so we
# bump the wait to 180s; CI may need to bump it again via a wrapper.
args=(create cluster --name "$CLUSTER_NAME" --wait 180s --config "$KIND_CONFIG")
if [[ -n "${KIND_IMAGE:-}" ]]; then
  args+=(--image "$KIND_IMAGE")
fi

kind "${args[@]}"
echo "kind cluster '$CLUSTER_NAME' ready."
