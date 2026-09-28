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
#   ./scripts/bump-gvisor-version.sh 20260921.0
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

# Files in which every occurrence of the literal is rewritten. Any other
# occurrence in the tree is reported (not rewritten) so a new mention is
# noticed and added here.
FILES=(
  "$GV_PACKER_HCL"
  "$GV_KIND_BUILD"
  "$GV_KIND_DOCKERFILE"
  "$SCRIPT_DIR/check-gvisor-version.sh"
  "$SCRIPT_DIR/bump-gvisor-version.sh"
)

# docs/gvisor-version.md states the pin, but it also records history, such as
# the tags a past drift run skipped. A rewrite of every occurrence changes that
# history to name the new tag (the first automated bump did this). In this
# file, only the lines that match GV_DOC_PIN_LINES are rewritten. This comment
# names no tag on purpose: this script is in FILES and rewrites itself.
GV_DOC="$GV_REPO_ROOT/docs/gvisor-version.md"
GV_DOC_PIN_LINES='^(The current pin is |gVisor tags look like )'

# rewrite FILE [LINE_REGEX]: replace OLD with NEW in FILE. With LINE_REGEX,
# only the lines that match it change. Prints the file when it changed.
rewrite() {
  local f="$1" only="${2:-}" before after
  [[ -f "$f" ]] || return 0
  before="$(grep -c -F "$OLD" "$f" || true)"
  [[ "$before" != "0" ]] || return 0
  # perl -pi works identically on GNU (Linux CI) and BSD (macOS) systems;
  # sed -i differs between the two. The values go in through the environment
  # so that no shell quoting reaches the Perl code. \Q...\E quotes the dot in
  # the tag.
  OLD="$OLD" NEW="$NEW" ONLY="$only" \
    perl -pi -e 's/\Q$ENV{OLD}\E/$ENV{NEW}/g if $ENV{ONLY} eq "" || /$ENV{ONLY}/' "$f"
  after="$(grep -c -F "$OLD" "$f" || true)"
  if [[ "$after" != "$before" ]]; then
    echo "  rewrote ${f#"$GV_REPO_ROOT"/}"
  fi
}

echo "bumping gVisor pin $OLD -> $NEW"
for f in "${FILES[@]}"; do
  rewrite "$f"
done
rewrite "$GV_DOC" "$GV_DOC_PIN_LINES"

# Anything left over is either a pin this script does not know about, or an
# intentional mention of the old tag (history in the docs, the release a rule
# was tested against). Only the first kind belongs in FILES.
if leftover="$(cd "$GV_REPO_ROOT" && git grep -n -F "$OLD" -- . ':!STRATEGY.md' 2>/dev/null)" && [[ -n "$leftover" ]]; then
  echo "warning: '$OLD' still appears in the files below. If a line is a pin, add its file to FILES in $0; history and test fixtures can stay:" >&2
  printf '%s\n' "$leftover" >&2
fi

exec "$SCRIPT_DIR/check-gvisor-version.sh"
