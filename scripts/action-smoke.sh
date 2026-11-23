#!/usr/bin/env bash
# scripts/action-smoke.sh: test of the script behind the GitHub Action.
#
# What this script does
#
#   It runs scripts/github-action.sh the way action.yml does: the inputs in
#   INPUT_* variables, and files for GITHUB_STEP_SUMMARY and GITHUB_OUTPUT.
#   Then it checks the four things that a workflow gets from the action: the
#   exit code, the job summary, the outputs, and the log.
#
#   No runner is here, so the log is checked with a model of the runner. The
#   runner reads each line of the log, and runs the lines that are workflow
#   commands. `outside_stop_blocks` below prints the lines that the runner
#   would look at (the lines that are not between "::stop-commands::<token>"
#   and "::<token>::"), and `expect_clean_log` checks that each of them is a
#   line that the action script makes on purpose. The rule is strict: a line
#   that is not a known command and does not start with "agentmoat: " fails
#   the check. The model is from the source of the runner
#   (ActionCommand.cs, ActionCommandManager.cs in actions/runner).
#
#   Checks:
#     1. The gate: pass, fail, and each --fail-on value.
#     2. Annotations: one for each workload that makes the gate fail, with
#        the file and the line; 10 at most.
#     3. Input that cannot be scanned, and inputs that are wrong: exit 1, and
#        one line in the job summary that says that the gate did not answer.
#     4. A render by the action: Kustomize and Helm, and a render that fails.
#     5. Hostile input: names and a file name with line breaks and workflow
#        commands in them. No command of theirs gets to the runner.
#     6. The download: the version rule, the checksum, an old release.
#
#   The download is tested against a directory (a file:// address) that has
#   the layout of the GitHub releases, made here from the built binary. The
#   real address is the one thing this script cannot test.
#
# Requirements: the binary (run `make build`), jq, curl, and tar. kubectl (or
# kustomize) and helm are optional; the render checks that need them are
# skipped without them.
#
# Exit codes
#
#   0  every check passed
#   1  prerequisite missing
#   2  a check failed

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=scripts/lib/output.sh
source "$REPO_ROOT/scripts/lib/output.sh"

AGENTMOAT_BIN="${AGENTMOAT_BIN:-$REPO_ROOT/bin/agentmoat}"
ACTION_SCRIPT="$REPO_ROOT/scripts/github-action.sh"
FIXTURES="test/testdata/manifests" # relative: the action runs in the root of a repository

if [[ ! -x "$AGENTMOAT_BIN" ]]; then
  echo "ERROR: agentmoat binary not found at $AGENTMOAT_BIN" >&2
  echo "Hint: run 'make build' first." >&2
  exit 1
fi
for tool in jq curl tar; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "ERROR: $tool is required; install it and retry." >&2
    exit 1
  fi
done

WORK_DIR="$(mktemp -d -t agentmoat-action.XXXX)"
trap 'rm -rf "$WORK_DIR"' EXIT
# The physical path: on macOS the temporary directory is behind a symlink,
# and a check below compares paths.
WORK_DIR="$(cd "$WORK_DIR" && pwd -P)"
mkdir -p "$WORK_DIR/runner-temp"

# The canary kubeconfig, as in scripts/offline-smoke.sh: the action reads no
# cluster, so the name of this context must be in no output.
CANARY="canary-must-not-appear"
cat > "$WORK_DIR/kubeconfig" <<EOF
apiVersion: v1
kind: Config
current-context: $CANARY
clusters:
  - name: $CANARY
    cluster:
      server: https://127.0.0.1:1
contexts:
  - name: $CANARY
    context:
      cluster: $CANARY
      user: $CANARY
users:
  - name: $CANARY
    user: {}
EOF

ALL_OUTPUT="$WORK_DIR/all-output.log"
: > "$ALL_OUTPUT"

FAILURES=0
CHECKS=0

# Results of the most recent `run_action`.
LOG="$WORK_DIR/log"         # stdout and stderr of the script: the step log
SUMMARY="$WORK_DIR/summary" # the job summary
OUTPUTS="$WORK_DIR/outputs" # the outputs of the step
OUTSIDE="$WORK_DIR/outside" # the lines of the log that the runner looks at
RC=0

# The directory that the action runs in. A check can change it.
RUN_DIR="$REPO_ROOT"

# run_action <NAME=value>...: run the action script with these inputs. The
# environment is empty but for PATH, HOME, and what the runner sets, so an
# input of an earlier run cannot leak into this one.
run_action() {
  RC=0
  : > "$SUMMARY"
  : > "$OUTPUTS"
  (
    cd "$RUN_DIR"
    env -i PATH="$PATH" HOME="$HOME" NO_COLOR=1 \
      KUBECONFIG="$WORK_DIR/kubeconfig" \
      RUNNER_TEMP="$WORK_DIR/runner-temp" \
      GITHUB_STEP_SUMMARY="$SUMMARY" \
      GITHUB_OUTPUT="$OUTPUTS" \
      "$@" \
      bash "$ACTION_SCRIPT"
  ) > "$LOG" 2>&1 || RC=$?
  outside_stop_blocks < "$LOG" > "$OUTSIDE"
  cat "$LOG" "$SUMMARY" "$OUTPUTS" >> "$ALL_OUTPUT"
}

# outside_stop_blocks: the model of the runner. It copies the lines that are
# not in a stop-commands block, and the two lines that open and close each
# block. If the log ends in an open block, it adds a marker line.
#
# The runner closes a block on a line that starts with "::<token>" (after
# white space), or that has "##[<token>]" in it. This model does the same.
outside_stop_blocks() {
  awk '
    {
      if (token != "") {
        if ($0 ~ ("^[ \t]*::" token) || index($0, "##[" token "]") > 0) {
          print
          token = ""
        }
        next
      }
      print
      if ($0 ~ /^[ \t]*::stop-commands::/) {
        token = $0
        sub(/^[ \t]*::stop-commands::/, "", token)
      }
    }
    END {
      if (token != "") {
        print "<<A STOP BLOCK IS OPEN AT THE END OF THE LOG>>"
      }
    }
  '
}

pass() {
  CHECKS=$((CHECKS + 1))
  printf '  %s %s\n' "$(_am_success "$AM_SYMBOL_OK")" "$1"
}

fail() {
  CHECKS=$((CHECKS + 1))
  FAILURES=$((FAILURES + 1))
  printf '  %s %s\n' "$(_am_danger "$AM_SYMBOL_FAIL")" "$1"
  if [[ -n "${2:-}" ]]; then
    printf '      %s\n' "$2"
  fi
  # The evidence, without control characters: a hostile fixture must not
  # write commands into the log of the job that runs this test.
  LC_ALL=C tr -d '\000-\010\013-\037\177' < "$LOG" | sed -e 's/^/      log| /' | head -30
}

# expect_rc <want> <label>
expect_rc() {
  if [[ "$RC" -eq "$1" ]]; then
    pass "$2 (exit $1)"
  else
    fail "$2" "exit code is $RC, want $1"
  fi
}

# expect_text <file> <text> <label>: the file contains the text.
expect_text() {
  if grep -qF -- "$2" "$1"; then
    pass "$3"
  else
    fail "$3" "'$2' is not in $(basename "$1")"
  fi
}

# expect_no_text <file> <text> <label>: the file does not contain the text.
expect_no_text() {
  if grep -qF -- "$2" "$1"; then
    fail "$3" "'$2' is in $(basename "$1")"
  else
    pass "$3"
  fi
}

# expect_output <name> <value> <label>: the step has this output.
expect_output() {
  if grep -qxF -- "$1=$2" "$OUTPUTS"; then
    pass "$3"
  else
    fail "$3" "the outputs have no line '$1=$2'"
  fi
}

# output_value <name>: the value of an output, empty when it is not set.
output_value() {
  sed -n "s/^$1=//p" "$OUTPUTS" | tail -1
}

# count_annotations: the number of error annotations that the runner makes.
count_annotations() {
  grep -c '^::error ' "$OUTSIDE" || true
}

# expect_annotations <n> <label>
expect_annotations() {
  local got
  got="$(count_annotations)"
  if [[ "$got" -eq "$1" ]]; then
    pass "$2"
  else
    fail "$2" "the log has $got error annotation(s), want $1"
  fi
}

# expect_clean_log <label>: every line that the runner looks at is a line
# that the action script makes on purpose. See the header.
#
# An annotation line must have no ":" in its properties (the first "::"
# after "::error " is then the end of the properties), which is what the
# encoding gives.
expect_clean_log() {
  local bad
  bad="$(grep -vE \
    -e '^agentmoat: ' \
    -e '^::group::agentmoat: (the report|details)$' \
    -e '^::endgroup::$' \
    -e '^::stop-commands::[0-9a-f]{32}$' \
    -e '^::[0-9a-f]{32}::$' \
    -e '^::error [^:]*::' \
    "$OUTSIDE" || true)"
  if [[ -n "$bad" ]]; then
    fail "$1" "$(printf '%s' "$bad" | wc -l | tr -d ' ') line(s) outside a stop block are not from the action script; the first: $(printf '%s' "$bad" | head -1 | LC_ALL=C tr -d '\000-\037\177' | cut -c1-100)"
    return
  fi
  if grep -qF '##[' "$OUTSIDE"; then
    fail "$1" "a line outside a stop block has '##[' in it"
    return
  fi
  pass "$1"
}

# expect_fixed_failure <text> <label>: the run ended in `die`: exit 1, the
# fixed line in the job summary, one fixed annotation, and no scan report.
expect_fixed_failure() {
  expect_rc 1 "$2"
  expect_text "$SUMMARY" "The scan did not run, so the gate did not answer." "$2: the job summary says that the gate did not answer"
  expect_text "$SUMMARY" "$1" "$2: the job summary says why"
  expect_annotations 1 "$2: one error annotation"
  expect_output "exit-code" "1" "$2: the output exit-code is 1"
  expect_no_text "$SUMMARY" "Gate passed" "$2: the job summary has no pass line"
  expect_clean_log "$2: the log is clean"
}

# section <title>: a heading between groups of checks.
section() {
  printf '\n%s\n' "$(_am_bold "$1")"
}

BIN_INPUT="INPUT_BINARY=$AGENTMOAT_BIN"

# ============================================================================
printf '\n'
_am_bold "agentmoat action smoke"
printf '\n'

# ----------------------------------------------------------------------------
section "1. the gate"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml"
expect_rc 0 "a sandboxed workload that is compatible"
expect_text "$SUMMARY" "✅ **Gate passed** (\`--fail-on sandboxed\`)" "the job summary starts with the gate result (the default is sandboxed)"
expect_output "exit-code" "0" "the output exit-code is 0"
expect_annotations 0 "a pass makes no annotation"
expect_text "$LOG" "agent-runner" "the log has the table"
expect_clean_log "the log is clean"
if cmp -s "$SUMMARY" "$(output_value markdown-file)"; then
  pass "the output markdown-file is the text of the job summary"
else
  fail "the output markdown-file is the text of the job summary"
fi
if [[ "$(jq -r '.kind' "$(output_value json-file)" 2>/dev/null)" == "ScanReport" ]]; then
  pass "the output json-file is a ScanReport"
else
  fail "the output json-file is a ScanReport"
fi
expect_no_text "$OUTPUTS" "sarif-file=" "no SARIF file when the input sarif-file is not set"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/sandboxed-hostnetwork.yaml" "INPUT_SARIF_FILE=$WORK_DIR/out/agentmoat.sarif"
expect_rc 2 "a sandboxed workload that is incompatible"
expect_text "$SUMMARY" "❌ **Gate failed**" "the job summary starts with the gate result"
expect_output "exit-code" "2" "the output exit-code is 2"
expect_annotations 1 "one annotation for the workload that fails the gate"
expect_text "$OUTSIDE" "::error file=$FIXTURES/sandboxed-hostnetwork.yaml,line=4,title=agentmoat%3A host-network::Deployment edge/gateway: Pod uses hostNetwork: true" "the annotation has the file, the line, the rule, and the workload"
expect_output "sarif-file" "$WORK_DIR/out/agentmoat.sarif" "the output sarif-file is the path of the input"
if [[ "$(jq -r '[.runs[0].results[] | select(.level == "error")] | length' "$WORK_DIR/out/agentmoat.sarif" 2>/dev/null)" == "1" ]]; then
  pass "the SARIF file has one result with the level error"
else
  fail "the SARIF file has one result with the level error"
fi
expect_clean_log "the log is clean"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/sandboxed-hostnetwork.yaml" "INPUT_FAIL_ON=none"
expect_rc 0 "fail-on none on the same file"
expect_text "$SUMMARY" "No gate" "the job summary says that there is no gate"
expect_annotations 0 "no gate, no annotation"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/rollout-privileged.yaml"
expect_rc 0 "fail-on sandboxed ignores a workload that does not ask for the sandbox"
expect_text "$SUMMARY" "Gate checked nothing" "the job summary says that the gate checked nothing, not that it passed"
expect_no_text "$SUMMARY" "Gate passed" "no pass line for a gate that checked nothing"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/rollout-privileged.yaml" "INPUT_FAIL_ON=incompatible"
expect_rc 2 "fail-on incompatible fails on it"
expect_annotations 1 "one annotation"
expect_text "$OUTSIDE" "title=agentmoat%3A privileged::" "the annotation names the rule"

# Two paths, with an empty line and a Windows line end between them.
run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml"$'\r\n\n'"  $FIXTURES/sandboxed-hostnetwork.yaml  "
expect_rc 2 "two paths, one for each line"
if [[ "$(jq -r '.spec.summary.total' "$(output_value json-file)" 2>/dev/null)" == "2" ]]; then
  pass "the two files were scanned"
else
  fail "the two files were scanned"
fi

# A sandbox class with another name.
run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/sandboxed-hostnetwork.yaml" "INPUT_RUNTIME_CLASS=kata"
expect_rc 0 "runtime-class sets the class that the gate looks for"
expect_text "$SUMMARY" "Gate checked nothing" "no workload asks for that class"

# ----------------------------------------------------------------------------
section "2. the limit on annotations"

# 12 workloads that fail the gate: 9 annotations, and one that counts the rest.
MANY="$WORK_DIR/many"
mkdir -p "$MANY"
: > "$MANY/many.yaml"
for i in 01 02 03 04 05 06 07 08 09 10 11 12; do
  cat >> "$MANY/many.yaml" <<EOF
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app-$i
  namespace: many
spec:
  selector:
    matchLabels: {app: app-$i}
  template:
    metadata:
      labels: {app: app-$i}
    spec:
      runtimeClassName: gvisor
      hostNetwork: true
      hostPID: true
      containers:
        - name: app
          image: example.test/app:1
EOF
done
RUN_DIR="$WORK_DIR"
run_action "$BIN_INPUT" "INPUT_PATH=many"
RUN_DIR="$REPO_ROOT"
expect_rc 2 "12 workloads that fail the gate"
expect_annotations 10 "10 annotations at most"
expect_text "$OUTSIDE" "::error title=agentmoat::3 more workload(s) make the gate fail. The job summary has the full list." "the last annotation counts the workloads that have none"
expect_text "$OUTSIDE" "more finding(s) on this workload)" "one annotation for a workload with more than one finding"
expect_clean_log "the log is clean"

# ----------------------------------------------------------------------------
section "3. input that cannot be scanned, and inputs that are wrong"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/helm-unrendered"
expect_rc 1 "a Helm template that is not rendered"
expect_text "$SUMMARY" "agentmoat could not scan the input, so the gate did not answer." "the job summary has a line, although the scan printed no report"
expect_no_text "$SUMMARY" "Gate passed" "the job summary has no pass line"
expect_annotations 1 "one error annotation"
expect_text "$OUTSIDE" "use the input 'kustomize' or 'helm-chart' of the action" "the annotation names the inputs that render"
expect_text "$LOG" "helm template" "the log has the reason from agentmoat"
expect_output "exit-code" "1" "the output exit-code is 1"
expect_clean_log "the log is clean"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/kustomize"
expect_rc 1 "a Kustomize directory in the input path, with fail-on sandboxed"
expect_text "$SUMMARY" "Gate could not answer" "the job summary has the report, and it says that the gate could not answer"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml" "INPUT_FAIL_ON=everything"
expect_rc 1 "a fail-on value that does not exist"
expect_text "$SUMMARY" "the gate did not answer" "the job summary says that the gate did not answer"

run_action "$BIN_INPUT" "INPUT_PATH=does/not/exist"
expect_rc 1 "a path that does not exist"
expect_text "$SUMMARY" "the gate did not answer" "the job summary says that the gate did not answer"

run_action "$BIN_INPUT"
expect_fixed_failure "set one of the inputs" "no input to scan"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES" "INPUT_KUSTOMIZE=$FIXTURES/kustomize/overlay"
expect_fixed_failure "Set only one of them" "two inputs to scan"

run_action "$BIN_INPUT" "INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml" "INPUT_FAIL_ON=none"$'\n'"sandboxed"
expect_fixed_failure "must be one line" "an input with two lines that takes one value"

run_action "INPUT_BINARY=$WORK_DIR/no-such-binary" "INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml"
expect_fixed_failure "does not name a file that can run" "a binary input that names no file"

# A release from before `scan -f`: its help has no --fail-on.
cat > "$WORK_DIR/old-agentmoat" <<'EOF'
#!/bin/sh
echo "Usage: agentmoat scan [flags]"
echo "      --rules string   path to classifier rules"
EOF
chmod +x "$WORK_DIR/old-agentmoat"
run_action "INPUT_BINARY=$WORK_DIR/old-agentmoat" "INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml"
expect_fixed_failure "cannot scan manifests" "a release with no file scan"

# A binary that prints no report and ends with exit code 0. That is not a
# pass: the job summary would be empty, and an empty summary on a green step
# reads as "nothing to say". The action makes it an error.
cat > "$WORK_DIR/silent-agentmoat" <<'EOF'
#!/bin/sh
case "$*" in
  *--help*) echo "  -f, --filename stringArray"; echo "      --fail-on string" ;;
esac
exit 0
EOF
chmod +x "$WORK_DIR/silent-agentmoat"
run_action "INPUT_BINARY=$WORK_DIR/silent-agentmoat" "INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml"
expect_rc 1 "exit code 0 with no report is an error"
expect_text "$SUMMARY" "agentmoat could not scan the input, so the gate did not answer." "the job summary says that the gate did not answer"
expect_output "exit-code" "1" "the output exit-code is 1"

# ----------------------------------------------------------------------------
section "4. a render by the action"

if command -v kubectl >/dev/null 2>&1 || command -v kustomize >/dev/null 2>&1; then
  # The fixture: the base has a Deployment with host networking, and the
  # overlay adds runtimeClassName: gvisor to it. Only the render shows that
  # a sandboxed workload cannot run, so exit 2 here proves the render.
  run_action "$BIN_INPUT" "INPUT_KUSTOMIZE=$FIXTURES/kustomize/overlay"
  expect_rc 2 "the input kustomize: the overlay is rendered, then scanned"
  expect_text "$SUMMARY" "Gate failed" "the gate looked at the workload that the overlay puts in the sandbox"
  if [[ "$(jq -r '.spec.workloads[0].runtimeClassName' "$(output_value json-file)" 2>/dev/null)" == "gvisor" ]]; then
    pass "the scan read the class that the overlay adds"
  else
    fail "the scan read the class that the overlay adds"
  fi
  expect_annotations 1 "one annotation"
  expect_no_text "$OUTSIDE" "::error file=" "an annotation for a rendered object has no file"
  expect_clean_log "the log is clean"

  run_action "$BIN_INPUT" "INPUT_KUSTOMIZE=$FIXTURES/sandboxed-compatible.yaml"
  expect_fixed_failure "The Kustomize render failed" "a Kustomize render that fails"
  expect_text "$OUTSIDE" "::group::agentmoat: details" "the log has what the render printed, as a group"
  expect_no_text "$LOG" "agentmoat: the report" "no scan ran after the render failed"
else
  echo "  (kubectl and kustomize are not installed: the Kustomize checks are skipped)"
fi

if command -v helm >/dev/null 2>&1; then
  CHART="$WORK_DIR/charts/web"
  mkdir -p "$CHART/templates"
  cat > "$CHART/Chart.yaml" <<'EOF'
apiVersion: v2
name: web
version: 0.1.0
EOF
  cat > "$CHART/values.yaml" <<'EOF'
hostNetwork: false
EOF
  cat > "$CHART/values-host.yaml" <<'EOF'
hostNetwork: true
EOF
  cat > "$CHART/templates/deployment.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .Release.Name }}-web
  namespace: {{ .Release.Namespace }}
spec:
  selector:
    matchLabels: {app: web}
  template:
    metadata:
      labels: {app: web}
    spec:
      runtimeClassName: gvisor
      hostNetwork: {{ .Values.hostNetwork }}
      containers:
        - name: web
          image: example.test/web:1
EOF
  RUN_DIR="$WORK_DIR"
  run_action "$BIN_INPUT" "INPUT_HELM_CHART=charts/web" "INPUT_NAMESPACE=shop"
  expect_rc 0 "the input helm-chart: the chart is rendered, then scanned"
  expect_text "$SUMMARY" "Gate passed" "the gate looked at the workload of the chart"
  if [[ "$(jq -r '.spec.workloads[0].namespace' "$(output_value json-file)" 2>/dev/null)" == "shop" ]]; then
    pass "the input namespace is the namespace of the render"
  else
    fail "the input namespace is the namespace of the render"
  fi

  run_action "$BIN_INPUT" "INPUT_HELM_CHART=charts/web" "INPUT_HELM_VALUES=charts/web/values-host.yaml"
  expect_rc 2 "the input helm-values changes the render, and the gate fails on it"
  expect_annotations 1 "one annotation"
  expect_no_text "$OUTSIDE" "::error file=" "an annotation for a rendered object has no file"
  expect_text "$OUTSIDE" "::error title=agentmoat%3A host-network::Deployment" "the annotation names the rule and the workload"
  expect_clean_log "the log is clean"

  printf '{{ .Values.nothing | required "PWNED-required" }}\n{{ if }}\n' > "$CHART/templates/broken.yaml"
  run_action "$BIN_INPUT" "INPUT_HELM_CHART=charts/web"
  expect_fixed_failure "The Helm render failed" "a Helm render that fails"
  expect_no_text "$LOG" "agentmoat: the report" "no scan ran after the render failed"
  RUN_DIR="$REPO_ROOT"
else
  echo "  (helm is not installed: the Helm checks are skipped)"
fi

# ----------------------------------------------------------------------------
section "5. hostile input: no command of a manifest gets to the runner"

# The names hold line breaks, and after each a workflow command: the two
# forms that the runner reads ("::name::" at the start of a line, and
# "##[name]" at any place), a command that would turn the commands back on,
# a terminal escape, and "%0A", which the runner decodes to a line break in
# the text of an annotation. The file name holds a line break, a comma, and
# a colon, which are the characters with a meaning in an annotation.
HOSTILE="$WORK_DIR/hostile"
mkdir -p "$HOSTILE/k8s"
HOSTILE_FILE="$HOSTILE/k8s/a,b:c"$'\n'"::error::PWNED-file.yaml"
cat > "$HOSTILE_FILE" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: "evil\n::error title=PWNED::PWNED-name\n##[error]PWNED-v1\n::stop-commands::guess\n::guess::\n::add-mask::PWNED-mask\e[2K pct%0Aname"
  namespace: "ns\r\n::warning::PWNED-namespace\r::notice::PWNED-cr"
spec:
  selector:
    matchLabels: {app: evil}
  template:
    metadata:
      labels: {app: evil}
    spec:
      runtimeClassName: gvisor
      hostNetwork: true
      containers:
        - name: "c\n::set-output name=x::PWNED-container"
          image: "example.test/evil:1\n::error::PWNED-image"
EOF
RUN_DIR="$HOSTILE"
run_action "$BIN_INPUT" "INPUT_PATH=k8s"
RUN_DIR="$REPO_ROOT"
expect_rc 2 "a manifest with workflow commands in its names fails the gate as any other"
if grep -q "PWNED" "$LOG"; then
  pass "the hostile text is in the log (so the check below looks at something)"
else
  fail "the hostile text is in the log (so the check below looks at something)"
fi
expect_clean_log "every line that the runner looks at is from the action script"
expect_annotations 1 "one annotation: the real one"
expect_text "$OUTSIDE" "::error file=k8s/a%2Cb%3Ac%0A%3A%3Aerror%3A%3APWNED-file.yaml,line=1,title=agentmoat%3A host-network::" "the file name is encoded in the annotation: comma, colon, and line break"
# The name is in the message of the real annotation, as text on its line.
# (The SARIF report has no line break in a message; the annotation code
# encodes one all the same.) The "%" of the name is encoded, so the runner
# gives back "%0A" as three characters and not as a line break.
if grep -qE '^::error file=k8s/[^:]*,line=1,title=agentmoat%3A host-network::Deployment ns .*PWNED-name.*pct%250Aname' "$OUTSIDE"; then
  pass "the hostile name is text in the message of the real annotation, with its percent sign encoded"
else
  fail "the hostile name is text in the message of the real annotation, with its percent sign encoded"
fi
if grep -q $'\033' "$LOG"; then
  fail "the log has no terminal escape" "an ESC character is in the log"
else
  pass "the log has no terminal escape"
fi
if LC_ALL=C grep -q '##\[' "$LOG"; then
  fail "no '##[' is in the log" "the log page draws such a line as a command"
else
  pass "no '##[' is in the log"
fi

# ----------------------------------------------------------------------------
section "6. the download"

case "$(uname -s)" in
  Linux) OS="linux" ;;
  Darwin) OS="darwin" ;;
  *) OS="" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) ARCH="amd64" ;;
  aarch64 | arm64) ARCH="arm64" ;;
  *) ARCH="" ;;
esac

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{ print $1 }'
  else
    shasum -a 256 "$1" | awk '{ print $1 }'
  fi
}

if [[ -z "$OS" || -z "$ARCH" ]]; then
  echo "  (no release exists for this machine: the download checks are skipped)"
else
  # A directory with the layout of the GitHub releases:
  #   <base>/latest/download/<asset>     and   <base>/download/<tag>/<asset>
  ASSET="agentmoat_${OS}_${ARCH}.tar.gz"
  RELEASES="$WORK_DIR/releases"
  mkdir -p "$RELEASES/latest/download" "$RELEASES/download/v9.9.9" "$RELEASES/download/v6.6.6" "$WORK_DIR/pack"
  cp "$AGENTMOAT_BIN" "$WORK_DIR/pack/agentmoat"
  echo "license" > "$WORK_DIR/pack/LICENSE"
  tar -czf "$RELEASES/latest/download/$ASSET" -C "$WORK_DIR/pack" agentmoat LICENSE
  cp "$RELEASES/latest/download/$ASSET" "$RELEASES/download/v9.9.9/$ASSET"
  cp "$RELEASES/latest/download/$ASSET" "$RELEASES/download/v6.6.6/$ASSET"
  SUM="$(sha256_of "$RELEASES/latest/download/$ASSET")"
  # A real checksums file has a line for each archive of the release.
  for d in "$RELEASES/latest/download" "$RELEASES/download/v9.9.9"; do
    printf '%s  %s\n%s  %s\n' \
      "0000000000000000000000000000000000000000000000000000000000000000" "agentmoat-mcp_${OS}_${ARCH}.tar.gz" \
      "$SUM" "$ASSET" > "$d/checksums.txt"
  done
  # v6.6.6: the archive is not the one that the checksums file describes.
  printf '%s  %s\n' "1111111111111111111111111111111111111111111111111111111111111111" "$ASSET" > "$RELEASES/download/v6.6.6/checksums.txt"
  BASE="AGENTMOAT_RELEASE_BASE_URL=file://$RELEASES"
  ONE="INPUT_PATH=$FIXTURES/sandboxed-compatible.yaml"

  run_action "$BASE" "$ONE"
  expect_rc 0 "no version and no ref: the action downloads a release and runs it"
  expect_text "$OUTSIDE" "agentmoat: release latest ($OS/$ARCH)" "the release is the latest one"
  expect_text "$SUMMARY" "Gate passed" "the downloaded binary made the report"

  run_action "$BASE" "$ONE" "AGENTMOAT_ACTION_REF=v9.9.9"
  expect_rc 0 "the action is used at a full version tag"
  expect_text "$OUTSIDE" "agentmoat: release v9.9.9 ($OS/$ARCH)" "the release is the one of the action"

  for ref in v0 main 3f2c1d0a9b8e7f6a5d4c3b2a1908f7e6d5c4b3a2 v9.9; do
    run_action "$BASE" "$ONE" "AGENTMOAT_ACTION_REF=$ref"
    if [[ "$RC" -eq 0 ]] && grep -qF "agentmoat: release latest " "$OUTSIDE"; then
      pass "the ref $ref is not a full version: the release is the latest one"
    else
      fail "the ref $ref is not a full version: the release is the latest one"
    fi
  done

  run_action "$BASE" "$ONE" "AGENTMOAT_ACTION_REF=v1.2.3" "INPUT_VERSION=9.9.9"
  expect_rc 0 "the input version comes before the ref of the action"
  expect_text "$OUTSIDE" "agentmoat: release v9.9.9 ($OS/$ARCH)" "a version with no v gets one"

  run_action "$BASE" "$ONE" "INPUT_VERSION=latest" "AGENTMOAT_ACTION_REF=v9.9.9"
  expect_rc 0 "the input version can be latest"
  expect_text "$OUTSIDE" "agentmoat: release latest ($OS/$ARCH)" "the release is the latest one"

  run_action "$BASE" "$ONE" "INPUT_VERSION=v1.2.3"
  expect_fixed_failure "The download of the agentmoat release failed" "a release that does not exist"

  run_action "$BASE" "$ONE" "INPUT_VERSION=../../latest/download"
  expect_fixed_failure "is not \`latest\` and not a release tag" "a version that is not a tag"

  run_action "$BASE" "$ONE" "INPUT_VERSION=v6.6.6"
  expect_fixed_failure "does not have the SHA-256" "an archive that does not match the checksums file"
  expect_no_text "$LOG" "agentmoat: the report" "the archive was not run"
fi

# ----------------------------------------------------------------------------
section "7. no cluster"

if grep -qF "$CANARY" "$ALL_OUTPUT"; then
  CHECKS=$((CHECKS + 1))
  FAILURES=$((FAILURES + 1))
  printf '  %s %s\n' "$(_am_danger "$AM_SYMBOL_FAIL")" "the kubeconfig was read: its context name is in an output"
else
  pass "no output names the context of the kubeconfig: the action read no cluster"
fi

echo
if [[ "$FAILURES" -gt 0 ]]; then
  printf '%s\n' "$(_am_danger "$AM_SYMBOL_FAIL action smoke failed: $FAILURES of $CHECKS checks")"
  exit 2
fi
printf '%s\n' "$(_am_success "$AM_SYMBOL_OK action smoke passed ($CHECKS checks)")"
