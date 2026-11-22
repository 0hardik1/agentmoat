#!/usr/bin/env bash
# scripts/offline-smoke.sh: smoke test for the commands that need no cluster.
#
# What this script does
#
#   It runs the built ./bin/agentmoat against manifest files and checks the
#   exit code and the output of each run. Go unit tests cover the same logic
#   in-process; this script covers what they cannot: the real binary, real
#   pipes, and real exit codes, which is what a CI job uses.
#
#   Checks:
#     1. Inventory: `scan -f test/e2e/manifests/workloads.yaml` gives the
#        totals in that file's header (14 workloads: 8 compatible, 3 review,
#        3 incompatible). `make e2e` applies the same file to a real cluster
#        and expects the same totals from the cluster scan.
#     2. The --fail-on gate: incompatible (default), sandboxed, none.
#     3. Kinds with a pod template that are not one of the six (Argo Rollout).
#     4. Input that cannot be read: exit 1 and no report on stdout.
#     5. Input that is not rendered (a Kustomize base and overlay):
#        --fail-on sandboxed refuses it; the rendered output is checked.
#     6. A plan made from manifests is a preview: apply refuses it.
#
# The canary
#
#   Every run has KUBECONFIG set to a file whose context is named
#   "canary-must-not-appear" and whose server is a closed port. A command
#   that needs no cluster must not read it. If any output of any run in this
#   script contains that context name, the script fails. (/dev/null as the
#   kubeconfig would not do: its context name is empty, so a leak into a
#   report would not show.)
#
# Requirements: the binary (run `make build`), and jq. kubectl is optional;
# the one check that renders with `kubectl kustomize` is skipped without it.
#
# Exit codes
#
#   0  every check passed
#   1  prerequisite missing (binary, jq)
#   2  a check failed

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
# shellcheck source=scripts/lib/output.sh
source "$REPO_ROOT/scripts/lib/output.sh"

AGENTMOAT_BIN="${AGENTMOAT_BIN:-$REPO_ROOT/bin/agentmoat}"
FIXTURES="$REPO_ROOT/test/testdata/manifests"
E2E_WORKLOADS="$REPO_ROOT/test/e2e/manifests/workloads.yaml"

if [[ ! -x "$AGENTMOAT_BIN" ]]; then
  echo "ERROR: agentmoat binary not found at $AGENTMOAT_BIN" >&2
  echo "Hint: run 'make build' first." >&2
  exit 1
fi
if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq is required for assertions; install it and retry." >&2
  exit 1
fi

WORK_DIR="$(mktemp -d -t agentmoat-offline.XXXX)"
trap 'rm -rf "$WORK_DIR"' EXIT

# The canary kubeconfig. See the header. The whole context name is the
# marker; a shorter word would match ordinary workload names.
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
export KUBECONFIG="$WORK_DIR/kubeconfig"
export NO_COLOR=1

# Every byte any run prints is appended here, for the canary check at the end.
ALL_OUTPUT="$WORK_DIR/all-output.log"
: > "$ALL_OUTPUT"

FAILURES=0
CHECKS=0

# Results of the most recent `run`.
OUT="$WORK_DIR/stdout"
ERR="$WORK_DIR/stderr"
RC=0

# run <args...>: run agentmoat with the given arguments. Standard input is
# whatever the caller piped in. Captures stdout, stderr, and the exit code.
run() {
  RC=0
  "$AGENTMOAT_BIN" "$@" > "$OUT" 2> "$ERR" || RC=$?
  cat "$OUT" "$ERR" >> "$ALL_OUTPUT"
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
  # Show what the command printed, indented, so a CI log has the evidence.
  sed 's/^/      stderr: /' "$ERR" | head -20
}

# expect_rc <want> <label>
expect_rc() {
  if [[ "$RC" -eq "$1" ]]; then
    pass "$2 (exit $1)"
  else
    fail "$2" "exit code is $RC, want $1"
  fi
}

# expect_jq <filter> <want> <label>: the filter's output on stdout is <want>.
expect_jq() {
  local got
  got="$(jq -r "$1" "$OUT" 2>/dev/null || echo '<stdout is not JSON>')"
  if [[ "$got" == "$2" ]]; then
    pass "$3"
  else
    fail "$3" "jq '$1' gave '$got', want '$2'"
  fi
}

# expect_text <file> <text> <label>: the file contains the text.
expect_text() {
  if grep -qF -- "$2" "$1"; then
    pass "$3"
  else
    fail "$3" "the text '$2' is not in $(basename "$1")"
  fi
}

# expect_empty <file> <label>
expect_empty() {
  if [[ ! -s "$1" ]]; then
    pass "$2"
  else
    fail "$2" "$(basename "$1") is not empty: $(head -c 200 "$1")"
  fi
}

section() {
  printf '\n%s\n' "$(_am_bold "$1")"
}

# -----------------------------------------------------------------------------
section "1. inventory: the e2e manifests, with no cluster"
# -----------------------------------------------------------------------------

run scan -f "$E2E_WORKLOADS" -n agentmoat-e2e -o json
expect_rc 2 "scan -f exits 2: three workloads are incompatible"
expect_jq '.spec.summary | "\(.total) \(.compatible) \(.needsReview) \(.incompatible)"' "14 8 3 3" \
  "summary is 14 total, 8 compatible, 3 review, 3 incompatible"
expect_jq '.metadata.source.type' "manifests" "the report says its source is manifests"
expect_jq '.metadata | has("cluster") or has("clusterFacts")' "false" "the report names no cluster and has no cluster facts"
expect_jq '[.spec.workloads[] | select(.namespace != "agentmoat-e2e")] | length' "0" "-n filled in the namespace of every workload"
expect_jq '[.spec.workloads[] | select(.origin.line > 0)] | length' "14" "every workload has an origin line"

run scan -f "$E2E_WORKLOADS"
expect_rc 2 "the table form exits 2 too"
expect_text "$OUT" "RUNTIME" "the table has a RUNTIME column"
expect_text "$OUT" "source: manifests" "the table says the source is manifests"

# -----------------------------------------------------------------------------
section "2. the gate: --fail-on"
# -----------------------------------------------------------------------------

run scan -f "$E2E_WORKLOADS" --fail-on sandboxed -o json
expect_rc 0 "sandboxed: incompatible workloads that do not ask for gvisor do not fail"
expect_text "$ERR" 'no workload asks for RuntimeClass "gvisor"' "sandboxed: the gate says it checked nothing"

run scan -f "$E2E_WORKLOADS" --fail-on none -o json
expect_rc 0 "none: never fails"

run scan -f "$FIXTURES/sandboxed-hostnetwork.yaml" --fail-on sandboxed -o json
expect_rc 2 "sandboxed: a workload that asks for gvisor and uses host networking fails"
expect_text "$ERR" "Deployment edge/gateway: host-network" "sandboxed: the failure names the workload and the rule"
expect_jq '.spec.summary.sandboxed' "1" "summary.sandboxed counts it"
expect_jq '.spec.workloads[0].runtimeClassName' "gvisor" "the workload carries its runtimeClassName"

run scan -f "$FIXTURES/sandboxed-compatible.yaml" --fail-on sandboxed -o json
expect_rc 0 "sandboxed: a compatible sandboxed workload passes"
expect_text "$ERR" "gate passed: 1 workload(s)" "sandboxed: the gate says what it checked"
expect_jq '.metadata.source.skipped.Service' "1" "the Service is counted as not classified"

run scan -f "$FIXTURES/sandboxed-hostnetwork.yaml" --fail-on sandboxed --runtime-class kata -o json
expect_rc 0 "sandboxed: under another --runtime-class the workload is not sandboxed"

run scan -f "$FIXTURES/sandboxed-hostnetwork.yaml" --fail-on bogus
expect_rc 1 "an unknown --fail-on value is an error"

# -----------------------------------------------------------------------------
section "3. kinds with a pod template"
# -----------------------------------------------------------------------------

run scan -f "$FIXTURES/rollout-privileged.yaml" -o json
expect_rc 2 "an Argo Rollout with a privileged container is incompatible"
expect_jq '.spec.workloads[0] | "\(.kind) \(.compatibility)"' "Rollout.argoproj.io incompatible" \
  "its kind is Rollout.argoproj.io"
expect_jq '[.spec.workloads[0].reasons[].ruleId] | index("privileged") != null' "true" "the privileged rule fired"

# -----------------------------------------------------------------------------
section "4. input that cannot be read"
# -----------------------------------------------------------------------------

run scan -f "$FIXTURES/helm-unrendered" -o json
expect_rc 1 "an unrendered Helm template is an error"
expect_empty "$OUT" "nothing is printed on stdout"
expect_text "$ERR" "helm template" "the error points to helm template"

# A redirect, not a pipe: the right side of a pipe is a subshell, and the
# exit code that `run` stores would not come back to this shell.
run scan -f - -o json < /dev/null
expect_rc 1 "empty standard input is an error"
expect_text "$ERR" "standard input" "the error names standard input"

run scan -f "$WORK_DIR/does-not-exist"
expect_rc 1 "a path that does not exist is an error"

# Standard input works when there is something on it.
run scan -f - -o json < "$FIXTURES/sandboxed-hostnetwork.yaml"
expect_rc 2 "manifests on standard input are scanned"
expect_jq '.spec.workloads[0].origin.file' "<stdin>" "the origin is <stdin>"

# -----------------------------------------------------------------------------
section "5. input that is not rendered (Kustomize)"
# -----------------------------------------------------------------------------

run scan -f "$FIXTURES/kustomize" -o json
expect_rc 2 "the default gate reads the base, which is incompatible"
expect_jq '.spec.summary | "\(.total) \(.sandboxed)"' "1 0" "one workload (the base); the patch is not a sandboxed workload"
expect_jq '.metadata.source.unrendered | length' "3" "two kustomization files and one partial object are listed"
expect_text "$ERR" "this input is not rendered" "a warning says the input is not rendered"

run scan -f "$FIXTURES/kustomize" --fail-on sandboxed -o json
expect_rc 1 "sandboxed: the gate refuses input that is not rendered"
expect_text "$ERR" "kustomize build" "the refusal says how to render"
expect_jq '.kind' "ScanReport" "the report is still printed"

if command -v kubectl >/dev/null 2>&1; then
  # `kubectl kustomize` is a local render. It contacts no cluster.
  kubectl kustomize "$FIXTURES/kustomize/overlay" > "$WORK_DIR/rendered.yaml" 2>> "$ALL_OUTPUT"
  run scan -f - --fail-on sandboxed -o json < "$WORK_DIR/rendered.yaml"
  expect_rc 2 "rendered: the sandboxed Deployment with host networking fails the gate"
  expect_jq '.spec.workloads[0] | "\(.runtimeClassName) \(.compatibility)"' "gvisor incompatible" \
    "rendered: the workload is sandboxed and incompatible"
  expect_jq '.metadata.source | has("unrendered")' "false" "rendered: no unrendered signs"
else
  printf '  %s %s\n' "$(_am_muted "-")" "skipped: kubectl is not installed (the rendered Kustomize check)"
fi

# -----------------------------------------------------------------------------
section "6. a plan made from manifests is a preview"
# -----------------------------------------------------------------------------

run scan -f "$E2E_WORKLOADS" -n agentmoat-e2e --fail-on none -o json
cp "$OUT" "$WORK_DIR/scan.json"

run plan --scan "$WORK_DIR/scan.json" -o json
expect_rc 0 "plan --scan accepts a report made from manifests"
expect_jq '.metadata.source.type' "manifests" "the plan is marked with its source"
expect_jq '.spec.warnings[0].id' "manifest-source" "the first warning says it is a preview"
cp "$OUT" "$WORK_DIR/plan.json"

run apply --plan "$WORK_DIR/plan.json" --no-audit
expect_rc 1 "apply refuses the plan (dry-run too)"
expect_text "$ERR" "it is a preview" "the refusal says why"

run rollback --plan "$WORK_DIR/plan.json" --no-audit
expect_rc 1 "rollback refuses the plan"

run verify --plan "$WORK_DIR/plan.json"
expect_rc 1 "verify refuses the plan"

# -----------------------------------------------------------------------------
section "the canary"
# -----------------------------------------------------------------------------

CHECKS=$((CHECKS + 1))
if grep -q "$CANARY" "$ALL_OUTPUT"; then
  FAILURES=$((FAILURES + 1))
  printf '  %s %s\n' "$(_am_danger "$AM_SYMBOL_FAIL")" "a command read the kubeconfig: its context name is in the output"
  grep -n "$CANARY" "$ALL_OUTPUT" | head -5 | sed 's/^/      /'
else
  printf '  %s %s\n' "$(_am_success "$AM_SYMBOL_OK")" "no command read the kubeconfig"
fi

printf '\n'
if [[ "$FAILURES" -gt 0 ]]; then
  printf '%s %d of %d checks failed\n' "$(_am_danger "$AM_SYMBOL_FAIL")" "$FAILURES" "$CHECKS"
  exit 2
fi
printf '%s offline smoke passed (%d checks)\n' "$(_am_success "$AM_SYMBOL_OK")" "$CHECKS"
