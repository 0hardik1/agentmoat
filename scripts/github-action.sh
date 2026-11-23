#!/usr/bin/env bash
# scripts/github-action.sh: the script behind action.yml (the GitHub Action).
#
# What this script does
#
#   action.yml is a composite action with one step, and that step runs this
#   script. The script:
#
#     1. gets an agentmoat binary (a release download, or the `binary` input),
#     2. renders the input when it is a Kustomize directory or a Helm chart,
#     3. runs `agentmoat scan` on manifests with the --fail-on gate,
#     4. reports in three places:
#          the job summary   the Markdown report; it starts with the gate result
#          annotations       one error for each workload that makes the gate fail
#          the step log      the table, and what the tools printed
#
#   No cluster is read, and no kubeconfig: `scan -f` reads files only.
#
#   The exit code of the script is the exit code of the scan (see
#   docs/exit-codes.md): 0, 1 (the scan did not get an answer), or 2 (the gate
#   failed). The step fails on 1 and on 2.
#
# The rules of this script. Read them before a change.
#
#   1. The step log is a command channel. The runner reads every line that a
#      step prints, on stdout and on stderr, and runs the lines that are
#      workflow commands: a line that starts with "::", or that has "##["
#      in it. A pull request controls the text that the tools print: a
#      workload name can hold a line break, and so can a file name. So:
#
#        - Everything that a tool prints goes to a file first. This script
#          prints it only between "::stop-commands::<token>" and
#          "::<token>::", where the runner runs no command. The token is
#          random for each block.
#        - An annotation is made from the SARIF report by jq, with every
#          value encoded the way the runner decodes it (see ANNOTATIONS_JQ).
#        - Every other line that this script prints is fixed text, or a
#          value that passed a pattern check (a version, a number).
#        - A block that stays open would turn commands off for the later
#          steps of the job, and "::add-mask::" is a command. The EXIT trap
#          closes an open block.
#
#   2. It fails closed, and it always says so in the job summary. A job
#      summary shows no log and no exit code. So every way out that is not
#      exit code 0 writes to the summary first: the report when there is one,
#      and one fixed line when there is none (a render that failed, a
#      download that failed, input that does not parse). That line has no
#      text from a tool in it. A render goes to a file, and its exit code is
#      checked before the scan: a pipe would let the scan pass on output
#      that was cut short.
#
#   3. The exit code is the one of the first scan. The other runs (the table,
#      the SARIF file, the JSON file), jq, and the annotations are extras: a
#      failure in one of them never changes the exit code. One exception, on
#      the safe side: exit code 0 with no report is an error.
#
#   4. The inputs come from the environment (INPUT_*), which action.yml sets
#      with `env:`. An input is never put into the text of a `run:` line.
#
#   5. bash 3.2 runs it (the bash of a macOS runner): no associative array,
#      no mapfile, and no expansion of an empty array with `set -u`.
#
# Environment
#
#   INPUT_PATH            files or directories of manifests, one for each line
#   INPUT_KUSTOMIZE       a directory with a kustomization file; rendered first
#   INPUT_HELM_CHART      a chart directory; rendered first
#   INPUT_HELM_VALUES     values files for the chart, one for each line
#   INPUT_FAIL_ON         sandboxed (default) | incompatible | none
#   INPUT_RUNTIME_CLASS   the sandbox RuntimeClass (default gvisor)
#   INPUT_NAMESPACE       the namespace for objects that name none
#   INPUT_RULES           a rules override file
#   INPUT_SARIF_FILE      where to write a SARIF report (default: nowhere)
#   INPUT_VERSION         the release to download: latest, or a tag (v0.3.0)
#   INPUT_BINARY          an agentmoat binary to use; then nothing is downloaded
#   AGENTMOAT_ACTION_REF  the ref of the action (github.action_ref)
#
#   From the runner: GITHUB_STEP_SUMMARY, GITHUB_OUTPUT, RUNNER_TEMP.
#
#   For tests only: AGENTMOAT_RELEASE_BASE_URL replaces the address of the
#   GitHub releases. scripts/action-smoke.sh points it at a directory.
#
# Tests: scripts/action-smoke.sh (`make offline-smoke`), and
# .github/workflows/action-test.yml, which runs the action on real runners
# when the action changes.

set -uo pipefail

RELEASES_URL="${AGENTMOAT_RELEASE_BASE_URL:-https://github.com/0hardik1/agentmoat/releases}"

# The runner shows a limited number of annotations for one step. This
# script makes 10 at most, and the last one then says how many are left.
MAX_ANNOTATIONS=10

# The most bytes of tool output that one block of the log shows.
MAX_LOG_BYTES=262144

INPUT_PATH="${INPUT_PATH:-}"
INPUT_KUSTOMIZE="${INPUT_KUSTOMIZE:-}"
INPUT_HELM_CHART="${INPUT_HELM_CHART:-}"
INPUT_HELM_VALUES="${INPUT_HELM_VALUES:-}"
INPUT_FAIL_ON="${INPUT_FAIL_ON:-}"
INPUT_RUNTIME_CLASS="${INPUT_RUNTIME_CLASS:-}"
INPUT_NAMESPACE="${INPUT_NAMESPACE:-}"
INPUT_RULES="${INPUT_RULES:-}"
INPUT_SARIF_FILE="${INPUT_SARIF_FILE:-}"
INPUT_VERSION="${INPUT_VERSION:-}"
INPUT_BINARY="${INPUT_BINARY:-}"
AGENTMOAT_ACTION_REF="${AGENTMOAT_ACTION_REF:-}"

WORK=""        # the directory for the files of this run
BIN=""         # the agentmoat binary
STOP_TOKEN=""  # not empty while a stop-commands block is open
NL=$'\n'
CR=$'\r'

# ----------------------------------------------------------------------------
# Output helpers
# ----------------------------------------------------------------------------

# say <text>: one line in the log. The text is fixed, or has only values that
# passed a pattern check.
say() {
  printf 'agentmoat: %s\n' "$1"
}

# annotate_error <text>: one error annotation with no file. Fixed text only:
# this function does not encode.
annotate_error() {
  printf '::error title=agentmoat::%s\n' "$1"
}

# summary <text>: one line in the job summary. Fixed text only.
summary() {
  [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] || return 0
  printf '%s\n' "$1" >> "$GITHUB_STEP_SUMMARY" 2>/dev/null || true
}

# set_output <name> <value>: one output of the step. The value is one line.
set_output() {
  [[ -n "${GITHUB_OUTPUT:-}" ]] || return 0
  printf '%s=%s\n' "$1" "$2" >> "$GITHUB_OUTPUT" 2>/dev/null || true
}

# new_token: 32 random hex characters, for one stop-commands block.
new_token() {
  local token
  token="$(LC_ALL=C od -An -N16 -tx1 /dev/urandom 2>/dev/null | LC_ALL=C tr -d ' \t\n')"
  [[ ${#token} -eq 32 ]] || return 1
  printf '%s' "$token"
}

# close_stop_block: start to run commands again. Safe to call when no block
# is open. The line break first makes the token start a line, also when the
# last line of the tool output had no line break.
close_stop_block() {
  if [[ -n "$STOP_TOKEN" ]]; then
    printf '\n::%s::\n' "$STOP_TOKEN"
    STOP_TOKEN=""
  fi
}
trap close_stop_block EXIT

# show_untrusted <title> <file>...: print what tools wrote, as a group in the
# log, where the runner runs no command. See rule 1 in the header.
#
# Two more changes make the text safe to read, not only safe to run:
# control characters go (a terminal escape can hide or recolor a line), and
# "##[" gets a space, because the log page draws a line with "##[error]" as
# an error line.
show_untrusted() {
  local title="$1"
  shift
  local file some=0
  for file in "$@"; do
    [[ -s "$file" ]] && some=1
  done
  [[ $some -eq 1 ]] || return 0

  local token
  if ! token="$(new_token)"; then
    say "the output of the tools is not shown: this runner gave no random token"
    return 0
  fi
  printf '::group::%s\n' "$title"
  STOP_TOKEN="$token"
  printf '::stop-commands::%s\n' "$token"
  for file in "$@"; do
    [[ -s "$file" ]] || continue
    LC_ALL=C tr -d '\000-\010\013-\037\177' < "$file" \
      | LC_ALL=C sed -e 's/##\[/# #[/g' \
      | head -c "$MAX_LOG_BYTES"
  done
  close_stop_block
  printf '::endgroup::\n'
}

# die <text> [file...]: the way out for every failure that is not a result
# of the scan. See rule 2 in the header. The text is fixed; the files hold
# what a tool printed, and go to the log only.
die() {
  local text="$1"
  shift
  if [[ $# -gt 0 ]]; then
    show_untrusted "agentmoat: details" "$@"
  fi
  summary "## agentmoat scan"
  summary ""
  summary "❌ **The scan did not run, so the gate did not answer.** $text The log of the step has the details."
  annotate_error "The scan did not run, so the gate did not answer. $text"
  set_output "exit-code" "1"
  exit 1
}

# trim <text>: the text with no white space at its two ends.
trim() {
  local s="$1"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

# one_line <name> <value>: an input that is one value must be one line.
one_line() {
  case "$2" in
    *"$NL"* | *"$CR"*) die "The input \`$1\` must be one line." ;;
  esac
}

# ----------------------------------------------------------------------------
# The binary
# ----------------------------------------------------------------------------

# release_version: which release to download. Prints "latest" or a tag.
#
#   - The `version` input, when it is set.
#   - The tag of the action, when the workflow names a full version
#     (`uses: 0hardik1/agentmoat@v0.3.0`). The script and the binary are
#     then from the same release.
#   - "latest" for every other ref: a branch, a commit SHA, or a moving tag
#     such as v0. A workflow that pins the action to a commit SHA must set
#     `version` to pin the binary too.
release_version() {
  local full='^v[0-9]+\.[0-9]+\.[0-9]+$'
  local any='^v[0-9]+\.[0-9]+\.[0-9]+([-+.][0-9A-Za-z.+-]*)?$'
  local v="$INPUT_VERSION"
  if [[ -z "$v" ]]; then
    if [[ "$AGENTMOAT_ACTION_REF" =~ $full ]]; then
      printf '%s' "$AGENTMOAT_ACTION_REF"
    else
      printf 'latest'
    fi
    return 0
  fi
  if [[ "$v" == "latest" ]]; then
    printf 'latest'
    return 0
  fi
  case "$v" in
    [0-9]*) v="v$v" ;;
  esac
  [[ "$v" =~ $any ]] || return 1
  printf '%s' "$v"
}

# sha256_of <file>: the SHA-256 of the file, in hex.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{ print $1 }'
  else
    return 1
  fi
}

# download_binary: get the release archive, check it against the checksums
# file of the same release, and extract it. Sets BIN.
download_binary() {
  local os arch
  case "$(uname -s)" in
    Linux) os="linux" ;;
    Darwin) os="darwin" ;;
    *) die "There is no agentmoat release for the operating system of this runner (the releases are for Linux and macOS). Build agentmoat in an earlier step and give its path in the input \`binary\`." ;;
  esac
  case "$(uname -m)" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
    *) die "There is no agentmoat release for the processor of this runner (the releases are for amd64 and arm64). Build agentmoat in an earlier step and give its path in the input \`binary\`." ;;
  esac

  local version
  if ! version="$(release_version)"; then
    die "The input \`version\` is not \`latest\` and not a release tag such as \`v0.3.0\`."
  fi

  local asset="agentmoat_${os}_${arch}.tar.gz"
  local base
  if [[ "$version" == "latest" ]]; then
    base="$RELEASES_URL/latest/download"
  else
    base="$RELEASES_URL/download/$version"
  fi
  say "release $version ($os/$arch)"

  local dl="$WORK/download"
  mkdir -p "$dl/extract"
  if ! command -v curl >/dev/null 2>&1; then
    die "This runner has no \`curl\`, so the release cannot be downloaded. Give the path of an agentmoat binary in the input \`binary\`."
  fi
  if ! curl -fsSL --retry 2 --connect-timeout 20 --max-time 300 -o "$dl/$asset" "$base/$asset" 2> "$dl/curl.stderr"; then
    die "The download of the agentmoat release failed. If the input \`version\` names a release, check that it exists." "$dl/curl.stderr"
  fi
  if ! curl -fsSL --retry 2 --connect-timeout 20 --max-time 60 -o "$dl/checksums.txt" "$base/checksums.txt" 2> "$dl/curl.stderr"; then
    die "The download of the checksums file of the agentmoat release failed." "$dl/curl.stderr"
  fi

  # The checksums file has one line for each archive: "<sha256>  <name>".
  local want got hex='^[0-9a-f]{64}$'
  want="$(awk -v name="$asset" '$2 == name || $2 == "*" name { print $1; exit }' "$dl/checksums.txt")"
  if ! [[ "$want" =~ $hex ]]; then
    die "The checksums file of the agentmoat release has no line for the archive of this runner."
  fi
  if ! got="$(sha256_of "$dl/$asset")"; then
    die "This runner has no \`sha256sum\` and no \`shasum\`, so the download cannot be checked. Give the path of an agentmoat binary in the input \`binary\`."
  fi
  if [[ "$got" != "$want" ]]; then
    die "The agentmoat archive does not have the SHA-256 that the checksums file of the release gives. The archive is not used."
  fi

  if ! tar -xzf "$dl/$asset" -C "$dl/extract" 2> "$dl/tar.stderr"; then
    die "The agentmoat archive could not be extracted." "$dl/tar.stderr"
  fi
  if [[ ! -f "$dl/extract/agentmoat" ]]; then
    die "The agentmoat archive has no \`agentmoat\` file in it."
  fi
  chmod +x "$dl/extract/agentmoat" 2>/dev/null || true
  BIN="$dl/extract/agentmoat"
}

# resolve_binary: sets BIN, and checks that the binary has the file scan.
resolve_binary() {
  if [[ -n "$INPUT_BINARY" ]]; then
    one_line "binary" "$INPUT_BINARY"
    if [[ ! -f "$INPUT_BINARY" || ! -x "$INPUT_BINARY" ]]; then
      die "The input \`binary\` does not name a file that can run."
    fi
    case "$INPUT_BINARY" in
      /*) BIN="$INPUT_BINARY" ;;
      *) BIN="$PWD/$INPUT_BINARY" ;;
    esac
    say "the binary is the one that the input 'binary' names"
  else
    download_binary
  fi

  # An old release has no `scan -f` and no --fail-on. Its error would be
  # "unknown flag", in the log only. Say what is wrong in plain words.
  if ! "$BIN" scan --help > "$WORK/help.txt" 2> "$WORK/help.stderr"; then
    die "The agentmoat binary did not run." "$WORK/help.stderr"
  fi
  if ! grep -q -- '--fail-on' "$WORK/help.txt" || ! grep -q -- '--filename' "$WORK/help.txt"; then
    die "This agentmoat release cannot scan manifests: it has no \`scan --filename\` or no \`--fail-on\`. Use a newer release (the input \`version\`)."
  fi
}

# ----------------------------------------------------------------------------
# The input: files, or a render
# ----------------------------------------------------------------------------

SCAN_ARGS=(scan)        # the arguments of every scan run, with no --output
SCAN_STDIN="/dev/null"  # the file that the scan reads as standard input
ANNOTATE_FILES="true"   # false for a render: a rendered object has no file in the repository

# add_paths: one --filename for each line of the `path` input.
add_paths() {
  local line count=0
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(trim "$line")"
    [[ -n "$line" ]] || continue
    # The "=" form: a value that starts with "-" is then not read as a flag.
    SCAN_ARGS+=("--filename=$line")
    count=$((count + 1))
  done <<< "$INPUT_PATH"
  [[ $count -gt 0 ]]
}

# render_kustomize <dir>: `kubectl kustomize`, or `kustomize build`, to a file.
render_kustomize() {
  local dir="$1" out="$WORK/rendered.yaml" err="$WORK/render.stderr"
  one_line "kustomize" "$dir"
  case "$dir" in
    -*) dir="./$dir" ;;
  esac
  local rc=0
  if command -v kubectl >/dev/null 2>&1; then
    # A local render: `kubectl kustomize` contacts no cluster.
    kubectl kustomize "$dir" > "$out" 2> "$err" || rc=$?
  elif command -v kustomize >/dev/null 2>&1; then
    kustomize build "$dir" > "$out" 2> "$err" || rc=$?
  else
    die "The input \`kustomize\` needs \`kubectl\` or \`kustomize\` on the runner, and it has none of them."
  fi
  if [[ $rc -ne 0 ]]; then
    die "The Kustomize render failed, so there is nothing to scan." "$err"
  fi
  # A warning of the render is not an error. Keep it for the log.
  cat "$err" >> "$WORK/tools.stderr" 2>/dev/null || true
}

# render_helm <chart>: `helm template` to a file. The release name is the
# default of `helm template` ("release-name").
render_helm() {
  local chart="$1" out="$WORK/rendered.yaml" err="$WORK/render.stderr"
  one_line "helm-chart" "$chart"
  case "$chart" in
    -*) chart="./$chart" ;;
  esac
  if ! command -v helm >/dev/null 2>&1; then
    die "The input \`helm-chart\` needs \`helm\` on the runner, and it is not there."
  fi
  local args=(template "$chart")
  if [[ -n "$INPUT_NAMESPACE" ]]; then
    args+=("--namespace=$INPUT_NAMESPACE")
  fi
  local line
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="$(trim "$line")"
    [[ -n "$line" ]] || continue
    args+=("--values=$line")
  done <<< "$INPUT_HELM_VALUES"
  if ! helm "${args[@]}" > "$out" 2> "$err"; then
    die "The Helm render failed, so there is nothing to scan." "$err"
  fi
  cat "$err" >> "$WORK/tools.stderr" 2>/dev/null || true
}

# prepare_input: exactly one of the three sources, and the scan arguments.
prepare_input() {
  local sources=0
  [[ -n "$(trim "$INPUT_PATH")" ]] && sources=$((sources + 1))
  [[ -n "$(trim "$INPUT_KUSTOMIZE")" ]] && sources=$((sources + 1))
  [[ -n "$(trim "$INPUT_HELM_CHART")" ]] && sources=$((sources + 1))
  if [[ $sources -eq 0 ]]; then
    die "The action has nothing to scan: set one of the inputs \`path\`, \`kustomize\`, and \`helm-chart\`."
  fi
  if [[ $sources -gt 1 ]]; then
    die "The inputs \`path\`, \`kustomize\`, and \`helm-chart\` are three ways to name the input. Set only one of them, and use the action one time for each."
  fi

  one_line "fail-on" "$INPUT_FAIL_ON"
  one_line "runtime-class" "$INPUT_RUNTIME_CLASS"
  one_line "namespace" "$INPUT_NAMESPACE"
  one_line "rules" "$INPUT_RULES"
  one_line "sarif-file" "$INPUT_SARIF_FILE"

  if [[ -n "$(trim "$INPUT_PATH")" ]]; then
    add_paths || die "The input \`path\` has no path in it."
  elif [[ -n "$(trim "$INPUT_KUSTOMIZE")" ]]; then
    render_kustomize "$(trim "$INPUT_KUSTOMIZE")"
    SCAN_ARGS+=("--filename=-")
    SCAN_STDIN="$WORK/rendered.yaml"
    ANNOTATE_FILES="false"
  else
    render_helm "$(trim "$INPUT_HELM_CHART")"
    SCAN_ARGS+=("--filename=-")
    SCAN_STDIN="$WORK/rendered.yaml"
    ANNOTATE_FILES="false"
  fi

  SCAN_ARGS+=("--fail-on=${INPUT_FAIL_ON:-sandboxed}")
  SCAN_ARGS+=("--runtime-class=${INPUT_RUNTIME_CLASS:-gvisor}")
  if [[ -n "$INPUT_NAMESPACE" ]]; then
    SCAN_ARGS+=("--namespace=$INPUT_NAMESPACE")
  fi
  if [[ -n "$INPUT_RULES" ]]; then
    SCAN_ARGS+=("--rules=$INPUT_RULES")
  fi
}

# run_scan <format> <stdout file> <stderr file>: one run of the scan. The
# return value is the exit code of agentmoat.
run_scan() {
  "$BIN" "${SCAN_ARGS[@]}" --no-color "--output=$1" < "$SCAN_STDIN" > "$2" 2> "$3"
}

# ----------------------------------------------------------------------------
# Annotations
# ----------------------------------------------------------------------------

# ANNOTATIONS_JQ makes the annotation lines from the SARIF report.
#
# Why the SARIF report: a result there has the level "error" only when its
# workload makes the gate fail. agentmoat decides that, with the same code as
# the exit code, so an annotation cannot say something else than the gate.
#
# One line for each workload, not for each finding: the number of
# annotations is limited, and three findings on one workload are one thing
# to correct.
#
# The encoding is the one of the runner (the "escapeData" and
# "escapeProperty" functions of actions/toolkit): "%", CR, and LF in every
# value, and also ":" and "," in a property. After it, a value cannot end
# its line, and a property cannot end the list of properties. Other control
# characters, and the Unicode line separators, become a space.
#
# The file of a result is a URI with percent-encoding. It is decoded here
# for the ASCII range. A path with other characters gets no file: the
# annotation is then in the list of the run, and not on a line of the diff.
ANNOTATIONS_JQ='
def enc($prop):
  explode
  | map(
      if . == 37 then "%25"
      elif . == 13 then "%0D"
      elif . == 10 then "%0A"
      elif . < 32 or . == 127 or . == 133 or . == 8232 or . == 8233 then " "
      elif $prop and . == 58 then "%3A"
      elif $prop and . == 44 then "%2C"
      else [.] | implode
      end)
  | join("");

def hexval:
  ascii_downcase | explode
  | map(if . >= 97 then . - 87 else . - 48 end)
  | .[0] * 16 + .[1];

def repo_file:
  (.locations[0].physicalLocation // null) as $p
  | if $files != "true" or $p == null then null
    elif ($p.artifactLocation.uriBaseId // "") != "%SRCROOT%" then null
    else
      ($p.artifactLocation.uri // "") as $u
      | if $u == "" or ($u | test("%[89A-Fa-f][0-9A-Fa-f]")) or ($u | test("^\\.\\.(/|$)")) then null
        else $u | gsub("%(?<h>[0-7][0-9A-Fa-f])"; (.h | hexval | [.] | implode))
        end
    end;

[.runs[0].results[]? | select(.level == "error")]
| group_by([(.locations[0].logicalLocations[0].fullyQualifiedName // ""), (repo_file // "")])
| map({
    file: (.[0] | repo_file),
    line: (.[0].locations[0].physicalLocation.region.startLine // null),
    rules: (map(.ruleId // "") | unique | map(select(. != ""))),
    text: (.[0].message.text // ""),
    more: (length - 1)
  })
| length as $n
| (if $n > $max then .[0:($max - 1)] else . end) as $shown
| ($shown[]
    | "::error "
      + (if .file != null
         then "file=" + (.file | enc(true))
              + (if (.line | type) == "number" then ",line=" + (.line | floor | tostring) else "" end)
              + ","
         else "" end)
      + "title=" + (("agentmoat: " + (.rules | join(", ")))[0:120] | enc(true))
      + "::"
      + ((.text[0:600]
          + (if .more > 0 then " (and " + (.more | tostring) + " more finding(s) on this workload)" else "" end))
         | enc(false))),
  (if $n > $max
   then "::error title=agentmoat::" + (($n - $max + 1) | tostring)
        + " more workload(s) make the gate fail. The job summary has the full list."
   else empty end)
'

# print_annotations <sarif file>: prints the annotation lines. Returns 0 when
# it printed one or more.
print_annotations() {
  local sarif="$1" lines
  command -v jq >/dev/null 2>&1 || return 1
  [[ -s "$sarif" ]] || return 1
  lines="$(jq -r --arg files "$ANNOTATE_FILES" --argjson max "$MAX_ANNOTATIONS" "$ANNOTATIONS_JQ" "$sarif" 2>/dev/null)" || return 1
  # One more check that costs nothing: only a line that is an error
  # annotation gets to the log.
  lines="$(printf '%s\n' "$lines" | grep '^::error ' || true)"
  [[ -n "$lines" ]] || return 1
  printf '%s\n' "$lines"
}

# ----------------------------------------------------------------------------
# Main
# ----------------------------------------------------------------------------

main() {
  WORK="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/agentmoat.XXXXXX")" || {
    WORK=""
    die "The runner gave no directory for temporary files."
  }
  : > "$WORK/tools.stderr"

  resolve_binary
  prepare_input

  # 1. The scan that decides. Its exit code is the exit code of the step.
  local rc=0
  run_scan markdown "$WORK/report.md" "$WORK/scan.stderr" || rc=$?
  cat "$WORK/scan.stderr" >> "$WORK/tools.stderr" 2>/dev/null || true

  # 2. The other forms of the same report. Each is an extra (rule 3).
  run_scan table "$WORK/report.txt" /dev/null || true
  run_scan sarif "$WORK/report.sarif" /dev/null || true
  run_scan json "$WORK/report.json" /dev/null || true

  # 3. The log: the table, then what the tools printed on stderr.
  show_untrusted "agentmoat: the report" "$WORK/report.txt" "$WORK/tools.stderr"

  # 4. The job summary (rule 2).
  if [[ -s "$WORK/report.md" ]]; then
    if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
      cat "$WORK/report.md" >> "$GITHUB_STEP_SUMMARY" 2>/dev/null || true
    fi
    set_output "markdown-file" "$WORK/report.md"
  else
    if [[ $rc -eq 0 ]]; then
      # Exit code 0 and no report: something is wrong, and "pass" is not
      # a safe answer to that.
      rc=1
    fi
    summary "## agentmoat scan"
    summary ""
    summary "❌ **agentmoat could not scan the input, so the gate did not answer.** The log of the step has the reason."
  fi

  # 5. Annotations, and one fixed line that says how the step ended.
  case "$rc" in
    0)
      say "exit code 0. The job summary has the report."
      ;;
    2)
      if ! print_annotations "$WORK/report.sarif"; then
        annotate_error "The agentmoat gate failed. The job summary has the report."
      fi
      say "the gate failed (exit code 2). The job summary has the report."
      ;;
    *)
      if [[ "$ANNOTATE_FILES" == "true" ]]; then
        # The usual cause with the `path` input: files that are not
        # rendered. Name the inputs that render.
        annotate_error "agentmoat could not scan the input, so the gate did not answer. The log of this step has the reason. If the input is a Kustomize directory or a Helm chart, use the input 'kustomize' or 'helm-chart' of the action: it then renders before the scan."
      else
        annotate_error "agentmoat could not scan the rendered input, so the gate did not answer. The log of this step has the reason."
      fi
      say "the scan did not get an answer (exit code $((rc)))."
      ;;
  esac

  # 6. The files for later steps.
  if [[ -s "$WORK/report.json" ]]; then
    set_output "json-file" "$WORK/report.json"
  fi
  if [[ -n "$INPUT_SARIF_FILE" ]]; then
    if [[ -s "$WORK/report.sarif" ]]; then
      local dir
      dir="$(dirname "$INPUT_SARIF_FILE")"
      if mkdir -p "$dir" 2>/dev/null && cp "$WORK/report.sarif" "$INPUT_SARIF_FILE" 2>/dev/null; then
        set_output "sarif-file" "$INPUT_SARIF_FILE"
      else
        say "the SARIF file was not written: the path of the input 'sarif-file' cannot be written"
      fi
    else
      say "the SARIF file was not written: the scan gave no report"
    fi
  fi

  set_output "exit-code" "$((rc))"
  exit "$rc"
}

main "$@"
