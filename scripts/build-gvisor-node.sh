#!/usr/bin/env bash
# agentmoat: build the gVisor-enabled kind node image and (optionally)
# load it into a kind cluster.
#
# This is invoked automatically by scripts/kind-up.sh before
# `kind create cluster`, because kind reads the per-node `image:` field
# from kind/cluster.yaml and expects the image to already exist locally
# (kind does not push images, and the image name here is unqualified, so
# it would otherwise try to pull from Docker Hub and fail).
#
# Idempotent: if an image with $IMG_TAG already exists locally for the
# host arch AND was built for the requested GVISOR_VERSION (read from the
# org.agentmoat.gvisor-version label the Dockerfile stamps), the script
# exits 0 without rebuilding. A different arch or gVisor version triggers a
# rebuild, so a pin bump is never masked by the tag cache. Use REBUILD=1 to
# force a rebuild regardless.
#
# Environment overrides:
#
#   IMG_TAG          image ref (default: agentmoat-kind-gvisor:dev).
#                    Must match kind/cluster.yaml's nodes[].image.
#   GVISOR_VERSION   gVisor release tag (default: 20260817.0).
#   KIND_NODE_VERSION
#                    upstream kindest/node base (default: v1.32.5).
#   REBUILD          set to 1 to force docker build even if image exists.
#   LOAD_CLUSTER     if set to a kind cluster name, also `kind load
#                    docker-image` into it after building. Skipped by
#                    default because kind-up.sh handles the first load
#                    via the cluster.yaml per-node image field.
#
# Exit codes:
#   0  image present (built or already cached)
#   1  build failed, or docker/buildx unavailable

set -euo pipefail

IMG_TAG="${IMG_TAG:-agentmoat-kind-gvisor:dev}"
GVISOR_VERSION="${GVISOR_VERSION:-20260817.0}"
KIND_NODE_VERSION="${KIND_NODE_VERSION:-v1.32.5}"
REBUILD="${REBUILD:-0}"
LOAD_CLUSTER="${LOAD_CLUSTER:-}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BUILD_CONTEXT="$REPO_ROOT/kind"
DOCKERFILE="$BUILD_CONTEXT/Dockerfile.gvisor-node"

if ! command -v docker >/dev/null 2>&1; then
  echo "error: docker not installed (or not on PATH)." >&2
  exit 1
fi

# Map the host's `uname -m` to the docker buildx platform string. kind
# nodes always run on the daemon's native arch, so we build for that arch
# only (no manifest list). docker buildx accepts amd64|arm64 here.
host_arch=$(uname -m)
case "$host_arch" in
  x86_64)  build_platform="linux/amd64" ;;
  aarch64|arm64) build_platform="linux/arm64" ;;
  *) echo "error: unsupported host arch '$host_arch'" >&2; exit 1 ;;
esac

# Skip if an image already exists for the right platform and the right
# gVisor version, unless REBUILD=1. `docker image inspect` exits non-zero if
# the tag is missing. Images built before the label existed report an empty
# version and are rebuilt once.
GVISOR_LABEL="org.agentmoat.gvisor-version"
if [[ "$REBUILD" != "1" ]] && docker image inspect "$IMG_TAG" >/dev/null 2>&1; then
  existing_arch=$(docker image inspect "$IMG_TAG" --format '{{.Architecture}}')
  existing_gvisor=$(docker image inspect "$IMG_TAG" --format "{{ index .Config.Labels \"$GVISOR_LABEL\" }}")
  if [[ "$existing_arch" != "${build_platform#linux/}" ]]; then
    echo "image '$IMG_TAG' present but wrong arch ($existing_arch != ${build_platform#linux/}); rebuilding."
    REBUILD=1
  elif [[ "$existing_gvisor" != "$GVISOR_VERSION" ]]; then
    echo "image '$IMG_TAG' present but built for gVisor '${existing_gvisor:-unknown}' (want $GVISOR_VERSION); rebuilding."
    REBUILD=1
  else
    echo "image '$IMG_TAG' already present ($existing_arch, gVisor $existing_gvisor); skip build (REBUILD=1 to override)."
  fi
fi

if [[ "$REBUILD" == "1" ]] || ! docker image inspect "$IMG_TAG" >/dev/null 2>&1; then
  echo "building $IMG_TAG ($build_platform, gVisor $GVISOR_VERSION, base kindest/node:$KIND_NODE_VERSION)..."
  # --load is important: by default buildx writes to its own cache, not
  # to the local docker daemon. kind looks images up in the daemon, so
  # we need them loaded there.
  docker buildx build \
    --platform "$build_platform" \
    --build-arg "GVISOR_VERSION=$GVISOR_VERSION" \
    --build-arg "KIND_NODE_VERSION=$KIND_NODE_VERSION" \
    --tag "$IMG_TAG" \
    --file "$DOCKERFILE" \
    --load \
    "$BUILD_CONTEXT"
  echo "built $IMG_TAG."
fi

# Optional: push into an already-running kind cluster (useful when
# iterating on the image without recreating the cluster).
if [[ -n "$LOAD_CLUSTER" ]]; then
  if ! command -v kind >/dev/null 2>&1; then
    echo "error: kind not installed; cannot LOAD_CLUSTER='$LOAD_CLUSTER'." >&2
    exit 1
  fi
  echo "loading $IMG_TAG into kind cluster '$LOAD_CLUSTER'..."
  kind load docker-image "$IMG_TAG" --name "$LOAD_CLUSTER"
  echo "loaded."
fi
