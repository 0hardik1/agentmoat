#!/usr/bin/env bash
# scripts/lib/gvisor-version.sh: shared helpers for the gVisor release pin.
#
# The pinned gVisor release appears in three files (the Packer template, the
# kind node Dockerfile, and the kind image build script). This library is the
# one place that knows how to read those defaults, validate a tag, and talk
# to the gVisor release bucket and the GitHub tags API. It is sourced by
# scripts/check-gvisor-version.sh and scripts/bump-gvisor-version.sh so the
# two scripts cannot drift apart.
#
# Tag format: gVisor publishes point releases as `yyyymmdd.N` (for example
# 20260817.0). `runsc --version` prints `release-yyyymmdd.N`; the download
# URLs never include the `release-` prefix, so neither do we.
#
# Requirements: bash 3.2+ (macOS ships 3.2, so no associative arrays and
# the `${arr[@]+"${arr[@]}"}` idiom for possibly-empty arrays under set -u),
# curl, grep, sed, sort. No jq: the GitHub tags payload is simple enough to
# grep, and this keeps the local toolchain short.

if [[ -n "${_AGENTMOAT_GVISOR_VERSION_SH:-}" ]]; then
  return 0 2>/dev/null || exit 0
fi
_AGENTMOAT_GVISOR_VERSION_SH=1

# GV_REPO_ROOT is resolved relative to this file so callers do not need to
# compute it themselves.
GV_REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# The three files that carry the pin. Keep this list in sync with
# docs/gvisor-version.md.
GV_PACKER_HCL="$GV_REPO_ROOT/packer/eks-gvisor-al2023.pkr.hcl"
GV_KIND_BUILD="$GV_REPO_ROOT/scripts/build-gvisor-node.sh"
GV_KIND_DOCKERFILE="$GV_REPO_ROOT/kind/Dockerfile.gvisor-node"

# The release bucket. Each release publishes, per arch, exactly these four
# artifacts; the Dockerfile and the Packer template download all of them.
GV_BUCKET="https://storage.googleapis.com/gvisor/releases/release"
GV_ARCHES=(x86_64 aarch64)
GV_ARTIFACTS=(runsc runsc.sha512 containerd-shim-runsc-v1 containerd-shim-runsc-v1.sha512)

# GitHub tags API for google/gvisor. Tags are not returned in a guaranteed
# order, so callers sort numerically; we page a little to be safe.
GV_TAGS_API="https://api.github.com/repos/google/gvisor/tags"
GV_TAGS_PAGES="${GV_TAGS_PAGES:-3}"

# How many of the newest tags gv_latest_published will probe for artifacts
# before giving up. A tag can exist on GitHub for days before its binaries
# land in the bucket (observed 2026-09-04 with release-20260831.0), so the
# newest tag is not always installable.
GV_PROBE_LIMIT="${GV_PROBE_LIMIT:-5}"

# gv_extract_tag prints the first yyyymmdd.N token in its argument, or fails.
gv_extract_tag() {
  if [[ "$1" =~ ([0-9]{8}\.[0-9]+) ]]; then
    printf '%s\n' "${BASH_REMATCH[1]}"
    return 0
  fi
  return 1
}

# gv_read_packer_default reads variable "gvisor_version" { default = "..." }
# from the Packer template. The awk range limits the search to that block so
# a different variable with an 8-digit default can never be mistaken for it.
gv_read_packer_default() {
  local file="${1:-$GV_PACKER_HCL}" line
  line="$(awk '/^variable "gvisor_version"/,/^}/' "$file" | grep -E '^[[:space:]]*default[[:space:]]*=' | head -1)"
  gv_extract_tag "$line" || {
    echo "error: could not parse gvisor_version default from $file" >&2
    return 1
  }
}

# gv_read_kind_script_default reads GVISOR_VERSION="${GVISOR_VERSION:-...}"
# from scripts/build-gvisor-node.sh.
gv_read_kind_script_default() {
  local file="${1:-$GV_KIND_BUILD}" line
  line="$(grep -E '^GVISOR_VERSION="\$\{GVISOR_VERSION:-[0-9]{8}\.[0-9]+\}"' "$file" | head -1)"
  gv_extract_tag "$line" || {
    echo "error: could not parse GVISOR_VERSION default from $file" >&2
    return 1
  }
}

# gv_read_dockerfile_default reads ARG GVISOR_VERSION=... from the kind
# node Dockerfile.
gv_read_dockerfile_default() {
  local file="${1:-$GV_KIND_DOCKERFILE}" line
  line="$(grep -E '^ARG GVISOR_VERSION=[0-9]{8}\.[0-9]+' "$file" | head -1)"
  gv_extract_tag "$line" || {
    echo "error: could not parse ARG GVISOR_VERSION from $file" >&2
    return 1
  }
}

# gv_validate_tag fails with a clear message when the tag carries the
# `release-` prefix or does not match yyyymmdd.N.
gv_validate_tag() {
  local tag="$1"
  if [[ "$tag" == release-* ]]; then
    echo "error: gVisor tag must not include the runsc --version 'release-' prefix (got '$tag')" >&2
    return 1
  fi
  if [[ ! "$tag" =~ ^[0-9]{8}\.[0-9]+$ ]]; then
    echo "error: gVisor tag must match yyyymmdd.N (got '$tag')" >&2
    return 1
  fi
}

# gv_artifact_url prints the bucket URL for <tag> <arch> <file>.
gv_artifact_url() {
  printf '%s/%s/%s/%s\n' "$GV_BUCKET" "$1" "$2" "$3"
}

# gv_artifacts_exist HEADs all eight artifacts for a tag. Prints one line per
# URL when GV_VERBOSE=1. Returns 1 as soon as one is missing.
gv_artifacts_exist() {
  local tag="$1" arch file url
  for arch in "${GV_ARCHES[@]}"; do
    for file in "${GV_ARTIFACTS[@]}"; do
      url="$(gv_artifact_url "$tag" "$arch" "$file")"
      if curl --fail --silent --show-error --location --output /dev/null --head "$url" 2>/dev/null; then
        [[ "${GV_VERBOSE:-0}" == "1" ]] && echo "ok  $url"
      else
        [[ "${GV_VERBOSE:-0}" == "1" ]] && echo "missing  $url" >&2
        return 1
      fi
    done
  done
  return 0
}

# gv_tag_newer returns 0 when tag $1 is strictly newer than tag $2. Compares
# the date part, then the point-release part, numerically.
gv_tag_newer() {
  local a_date="${1%%.*}" a_rc="${1##*.}" b_date="${2%%.*}" b_rc="${2##*.}"
  if (( 10#$a_date != 10#$b_date )); then
    (( 10#$a_date > 10#$b_date ))
  else
    (( 10#$a_rc > 10#$b_rc ))
  fi
}

# gv_fetch_tags prints every release-yyyymmdd.N tag from the GitHub API,
# newest first, one per line (without the release- prefix). Sends a bearer
# token when GITHUB_TOKEN is set: the anonymous limit (60 requests per hour
# per IP) is routinely exhausted on shared CI runners.
gv_fetch_tags() {
  local page body auth=()
  if [[ -n "${GITHUB_TOKEN:-}" ]]; then
    auth=(-H "Authorization: Bearer $GITHUB_TOKEN")
  fi
  for ((page = 1; page <= GV_TAGS_PAGES; page++)); do
    body="$(curl --fail --silent --show-error --location \
      -H "Accept: application/vnd.github+json" ${auth[@]+"${auth[@]}"} \
      "${GV_TAGS_API}?per_page=100&page=${page}")" || {
      echo "error: GitHub tags API request failed (page $page)" >&2
      return 1
    }
    # An empty page ends pagination.
    if ! grep -q '"name"' <<<"$body"; then
      break
    fi
    grep -oE '"name":[[:space:]]*"release-[0-9]{8}\.[0-9]+"' <<<"$body" \
      | grep -oE '[0-9]{8}\.[0-9]+'
  done | sort -t. -k1,1nr -k2,2nr | uniq
}

# gv_latest_published prints the newest tag whose artifacts are all present
# in the release bucket. Probes at most GV_PROBE_LIMIT tags. Fails when the
# tag list is empty or none of the probed tags is fully published.
gv_latest_published() {
  local tags tag probed=0
  tags="$(gv_fetch_tags)" || return 1
  if [[ -z "$tags" ]]; then
    echo "error: GitHub tags API returned no release tags" >&2
    return 1
  fi
  while IFS= read -r tag; do
    (( probed++ ))
    if gv_artifacts_exist "$tag"; then
      printf '%s\n' "$tag"
      return 0
    fi
    echo "note: release-$tag is tagged but its artifacts are not in the bucket yet; skipping" >&2
    if (( probed >= GV_PROBE_LIMIT )); then
      break
    fi
  done <<<"$tags"
  echo "error: none of the newest $probed tags has all artifacts published" >&2
  return 1
}
