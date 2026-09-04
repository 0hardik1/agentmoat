#!/usr/bin/env bash
# agentmoat: rewrite the pinned gVisor release everywhere it appears.
#
# The pin lives in three load-bearing files (Packer template, kind build
# script, kind Dockerfile) plus a few documentation mentions. Editing them by
# hand invites drift; this script rewrites the old literal in a fixed list of
# files and then runs scripts/check-gvisor-version.sh so a typo or an
# unpublished tag fails before anything is committed.
#
# Usage:
#   ./scripts/bump-gvisor-version.sh 20260817.0
#
# Used by .github/workflows/gvisor-drift.yml and by humans doing a manual
# bump (see docs/gvisor-version.md). After bumping, rebuild the kind image
# (`make kind-down && make e2e`) and consider a Packer rebuild.
#
# Exit codes:
#   0  files rewritten (or already at the requested tag) and checks passed
#   1  bad tag, unparseable pin, or the post-bump check failed

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib/gvisor-version.sh
source "$SCRIPT_DIR/lib/gvisor-version.sh"

if [[ $# -ne 1 || "$1" == "-h" || "$1" == "--help" ]]; then
  sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
  exit 1
fi

NEW="$1"
gv_validate_tag "$NEW"
OLD="$(gv_read_packer_default)"

if [[ "$OLD" == "$NEW" ]]; then
  echo "pin already at $NEW; nothing to do."
  exec "$SCRIPT_DIR/check-gvisor-version.sh"
fi

# Files in which the literal is rewritten. Any other occurrence in the tree is
# reported (not rewritten) so a new mention is noticed and added here.
FILES=(
  "$GV_PACKER_HCL"
  "$GV_KIND_BUILD"
  "$GV_KIND_DOCKERFILE"
  "$SCRIPT_DIR/check-gvisor-version.sh"
  "$SCRIPT_DIR/bump-gvisor-version.sh"
  "$GV_REPO_ROOT/docs/gvisor-version.md"
)

echo "bumping gVisor pin $OLD -> $NEW"
for f in "${FILES[@]}"; do
  [[ -f "$f" ]] || continue
  if grep -q -F "$OLD" "$f"; then
    # perl -pi works identically on GNU (Linux CI) and BSD (macOS) systems;
    # sed -i differs between the two. \Q...\E quotes the dot in the tag.
    perl -pi -e "s/\Q$OLD\E/$NEW/g" "$f"
    echo "  rewrote ${f#"$GV_REPO_ROOT"/}"
  fi
done

# Anything left over is a mention this script does not know about.
if leftover="$(cd "$GV_REPO_ROOT" && git grep -n -F "$OLD" -- . ':!STRATEGY.md' 2>/dev/null)" && [[ -n "$leftover" ]]; then
  echo "warning: '$OLD' still appears outside the known files; add them to FILES in $0:" >&2
  printf '%s\n' "$leftover" >&2
fi

exec "$SCRIPT_DIR/check-gvisor-version.sh"
