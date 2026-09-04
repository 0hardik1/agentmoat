#!/usr/bin/env bash
# agentmoat: verify the pinned gVisor release.
#
# Checks, in order:
#   1. The three pin defaults (Packer template, kind build script, kind
#      Dockerfile) agree with each other.
#   2. The tag has the yyyymmdd.N shape (no `release-` prefix).
#   3. All eight release artifacts (runsc, containerd-shim-runsc-v1, and both
#      .sha512 files, for x86_64 and aarch64) exist in the release bucket.
#      The Dockerfile and the Packer template download exactly these files.
#   4. With --latest: the pin is the newest *published* gVisor release. A
#      tag can exist on GitHub before its binaries are uploaded, so "latest"
#      means "newest tag whose artifacts are all present".
#
# Usage:
#   ./scripts/check-gvisor-version.sh                 # 1-3, used by CI on every PR
#   ./scripts/check-gvisor-version.sh --latest        # 1-4, used by the weekly drift workflow
#   GVISOR_VERSION=20260817.0 ./scripts/check-gvisor-version.sh   # check a tag other than the pin
#
# Machine-readable output (--latest only), on stdout and appended to
# $GITHUB_OUTPUT when that variable is set:
#   gvisor_pin=<tag>
#   gvisor_latest=<tag>
#   gvisor_behind=true|false
#
# Exit codes (script codes, unrelated to the CLI's docs/exit-codes.md):
#   0  all checks passed (and, with --latest, the pin is current)
#   1  pins diverged, bad tag format, artifact missing, or network error
#   2  pins consistent and artifacts present, but a newer release is published

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/gvisor-version.sh
source "$SCRIPT_DIR/lib/gvisor-version.sh"

CHECK_LATEST="${CHECK_LATEST:-0}"
for arg in "$@"; do
  case "$arg" in
    --latest) CHECK_LATEST=1 ;;
    -h|--help)
      sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "error: unknown argument '$arg' (try --help)" >&2
      exit 1
      ;;
  esac
done

PACKER_VERSION="$(gv_read_packer_default)"
KIND_SCRIPT_VERSION="$(gv_read_kind_script_default)"
KIND_DOCKER_VERSION="$(gv_read_dockerfile_default)"

if [[ "$PACKER_VERSION" != "$KIND_SCRIPT_VERSION" || "$PACKER_VERSION" != "$KIND_DOCKER_VERSION" ]]; then
  echo "error: gVisor version defaults diverged:" >&2
  echo "  packer/eks-gvisor-al2023.pkr.hcl: $PACKER_VERSION" >&2
  echo "  scripts/build-gvisor-node.sh:     $KIND_SCRIPT_VERSION" >&2
  echo "  kind/Dockerfile.gvisor-node:      $KIND_DOCKER_VERSION" >&2
  echo "fix: ./scripts/bump-gvisor-version.sh <tag> rewrites all three." >&2
  exit 1
fi

GVISOR_VERSION="${GVISOR_VERSION:-$PACKER_VERSION}"
gv_validate_tag "$GVISOR_VERSION"

echo "checking gVisor release $GVISOR_VERSION (packer/kind default: $PACKER_VERSION)..."
if ! GV_VERBOSE=1 gv_artifacts_exist "$GVISOR_VERSION"; then
  echo "error: release $GVISOR_VERSION is not fully published under $GV_BUCKET/$GVISOR_VERSION/" >&2
  exit 1
fi
echo "ok  all ${#GV_ARTIFACTS[@]} artifacts present for ${GV_ARCHES[*]}"

if [[ "$CHECK_LATEST" != "1" ]]; then
  exit 0
fi

echo "looking up the newest published gVisor release..."
LATEST="$(gv_latest_published)"
BEHIND=false
if gv_tag_newer "$LATEST" "$GVISOR_VERSION"; then
  BEHIND=true
fi

OUT="gvisor_pin=$GVISOR_VERSION
gvisor_latest=$LATEST
gvisor_behind=$BEHIND"
printf '%s\n' "$OUT"
if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
  printf '%s\n' "$OUT" >>"$GITHUB_OUTPUT"
fi

if [[ "$BEHIND" == "true" ]]; then
  echo "drift: pinned $GVISOR_VERSION is behind published $LATEST (see docs/gvisor-version.md)" >&2
  exit 2
fi
echo "ok  pin $GVISOR_VERSION is the newest published release"
