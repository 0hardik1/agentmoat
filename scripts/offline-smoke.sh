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
#     7. The output formats for a pipeline: Markdown (the gate result is in
#        the report, because a job summary shows no stderr) and SARIF (a
#        result is "error" only when it makes the gate fail).
#     8. `patch`, on a copy of the files: a dry-run writes nothing; a real
#        run adds one line for each selected workload and removes none; the
#        scan of the result finds the class; a second run changes nothing;
#        git reads the diff. Also the forms that are not edited (exit 3),
#        a Helm template (exit 1), and a Kustomize tree (the base is edited,
#        and the render of the overlay has the class).
#     9. `patch --kustomize`, the move of one Kustomize overlay: no source
#        file changes; the files that the run writes are a component that
#        `kubectl kustomize` builds; the overlay that uses it has the class
#        and the other overlay of the same base does not; a second run
#        changes nothing. Also the ways to give the input (the files of the
#        base, the output of the base, a plain directory, the output of a
#        Helm chart), and the two inputs that Kustomize refuses at the build.
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
# Requirements: the binary (run `make build`), and jq. kubectl, git, and
# helm are optional; the checks that render with `kubectl kustomize` or
# `helm template` and the check that applies a diff with `git apply` are
# skipped without them.
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
section "7. output for a pipeline: markdown and sarif"
# -----------------------------------------------------------------------------

# Relative paths from here on: a SARIF location is a path relative to the
# repository root, which is what a pipeline job gives.
cd "$REPO_ROOT"
REL="test/testdata/manifests"

run scan -f "$REL/sandboxed-hostnetwork.yaml" --fail-on sandboxed -o markdown
expect_rc 2 "markdown: the gate still sets the exit code"
expect_text "$OUT" '**Gate failed** (`--fail-on sandboxed`)' "markdown: the gate result is in the report, not only on stderr"
expect_text "$OUT" "Deployment edge/gateway: host-network ($REL/sandboxed-hostnetwork.yaml:4)" "markdown: the failing workload is listed with file and line"
if [[ "$(head -n 1 "$OUT")" == "## agentmoat scan" ]]; then
  pass "markdown: the report starts with its title"
else
  fail "markdown: the report starts with its title" "the first line is '$(head -n 1 "$OUT")'"
fi

run scan -f "$REL/sandboxed-compatible.yaml" --fail-on sandboxed -o markdown
expect_rc 0 "markdown: a passing gate exits 0"
expect_text "$OUT" '**Gate passed**' "markdown: a pass says so"

run scan -f "$E2E_WORKLOADS" --fail-on sandboxed -o markdown
expect_rc 0 "markdown: a gate that checked nothing exits 0"
expect_text "$OUT" '**Gate checked nothing**' "markdown: and the report does not call it a pass"

run scan -f "$REL/kustomize" --fail-on sandboxed -o markdown
expect_rc 1 "markdown: a refused gate exits 1"
expect_text "$OUT" '**Gate could not answer**' "markdown: the report that was printed says that the gate refused"

# SARIF. Two files: a sandboxed Deployment that cannot run in gVisor, and a
# Rollout that cannot run in it and does not ask to.
run scan -f "$REL/sandboxed-hostnetwork.yaml" -f "$REL/rollout-privileged.yaml" --fail-on sandboxed -o sarif
expect_rc 2 "sarif: the gate still sets the exit code"
expect_jq '.version' "2.1.0" "sarif: version 2.1.0"
expect_jq '.runs[0].results | length' "2" "sarif: one result for each finding"
expect_jq '[.runs[0].results[] | "\(.properties.name)=\(.level)"] | sort | join(" ")' "canary=warning gateway=error" \
  "sarif: only the workload that fails the gate has the level error"
expect_jq '.runs[0].results[] | select(.properties.name == "gateway") | .locations[0].physicalLocation | "\(.artifactLocation.uri):\(.region.startLine)"' \
  "$REL/sandboxed-hostnetwork.yaml:4" "sarif: the location is the file, relative, and the line"
expect_jq '[.runs[0].tool.driver.rules[].id] | join(" ")' "host-network privileged" "sarif: the rules are listed, sorted by ID"
expect_jq '[.runs[0].tool.driver.rules[] | select((.shortDescription.text | length) == 0 or (.fullDescription.text | length) == 0 or (.help.text | length) == 0)] | length' "0" \
  "sarif: every rule has the text that code scanning requires"

# The same files under the default gate: both workloads fail it.
run scan -f "$REL/sandboxed-hostnetwork.yaml" -f "$REL/rollout-privileged.yaml" -o sarif
expect_jq '[.runs[0].results[].level] | unique | join(" ")' "error" "sarif: under the default gate, both results are errors"

run scan -f "$REL/sandboxed-compatible.yaml" -o sarif
expect_rc 0 "sarif: a scan with no findings exits 0"
expect_jq '.runs[0] | "\(.results | type) \(.results | length) \(.tool.driver.rules | type)"' "array 0 array" \
  "sarif: no findings is an empty array, not null"

run scan -f - -o sarif < "$REL/sandboxed-hostnetwork.yaml"
expect_text "$ERR" "have no file location" "sarif: results from standard input have no file, and scan says so"
expect_jq '.runs[0].results[0].locations[0] | has("physicalLocation")' "false" "sarif: such a result has a logical location only"

# A format that a command cannot render is refused before any work.
run apply --plan "$WORK_DIR/plan.json" --no-audit -o sarif
expect_rc 1 "apply -o sarif is an error"
expect_text "$ERR" "is not available for ApplyResult output" "the error is about the format, before the plan is read"
expect_empty "$OUT" "nothing is printed on stdout"

run plan --scan "$WORK_DIR/scan.json" -o markdown
expect_rc 0 "plan -o markdown works"
expect_text "$OUT" '`manifest-source`' "markdown: the preview warning is in the plan summary"

# -----------------------------------------------------------------------------
section "8. patch: the move as a change to files"
# -----------------------------------------------------------------------------

# expect_same <file a> <file b> <label>: the two files have the same bytes.
expect_same() {
  if cmp -s "$1" "$2"; then
    pass "$3"
  else
    fail "$3" "$(basename "$1") and $(basename "$2") are different"
  fi
}

# expect_eq <got> <want> <label>
expect_eq() {
  if [[ "$1" == "$2" ]]; then
    pass "$3"
  else
    fail "$3" "got '$1', want '$2'"
  fi
}

# patch edits files, so it works on a copy. The paths are relative to the
# copy, as in a repository: the diff then has a/ and b/ names that git reads.
PATCH_DIR="$WORK_DIR/patch"
mkdir -p "$PATCH_DIR/k8s"
cp "$E2E_WORKLOADS" "$PATCH_DIR/k8s/workloads.yaml"
cp -R "$REPO_ROOT/test/testdata/patch/kustomize" "$PATCH_DIR/kustomize"
cp -R "$REPO_ROOT/test/testdata/patch/forms" "$PATCH_DIR/forms"
cp -R "$FIXTURES/helm-unrendered" "$PATCH_DIR/chart"
pushd "$PATCH_DIR" > /dev/null

# The dry-run is the default: the result has the diff, and no file changes.
run patch -f k8s -o json
expect_rc 0 "patch (dry-run, the default) exits 0"
expect_jq '.kind + " " + (.metadata.dryRun | tostring)' "PatchResult true" "the result is a PatchResult, and says that it is a dry-run"
expect_jq '.spec.summary | "\(.total) \(.selected) \(.patched) \(.excluded)"' "14 8 8 6" \
  "8 of 14 workloads are selected: the compatible ones with no class"
expect_jq '.spec.files | map(.written) | any' "false" "no file is marked as written"
expect_jq '.metadata.source.type' "manifests" "the result says its source is manifests"
expect_same k8s/workloads.yaml "$E2E_WORKLOADS" "the dry-run changed no file"
expect_text "$ERR" "--dry-run=false" "stderr says how to write the files"
jq -r '.spec.files[].diff' "$OUT" > workloads.diff

# The real run. The oracle: the new file is the old file plus one line for
# each selected workload, and nothing else.
run patch -f k8s --dry-run=false -o json
expect_rc 0 "patch --dry-run=false exits 0"
expect_jq '.spec.summary | "\(.patched) \(.notEdited) \(.failed) \(.files)"' "8 0 0 1" "8 workloads are patched, in one file"
expect_jq '.spec.files[0] | "\(.added) \(.written)"' "8 true" "the file has 8 added lines and is written"
ADDED="$(diff "$E2E_WORKLOADS" k8s/workloads.yaml | grep -c '^>' || true)"
REMOVED="$(diff "$E2E_WORKLOADS" k8s/workloads.yaml | grep -c '^<' || true)"
OTHER="$(diff "$E2E_WORKLOADS" k8s/workloads.yaml | grep '^>' | grep -vc '^> *runtimeClassName: gvisor$' || true)"
expect_eq "$ADDED $REMOVED $OTHER" "8 0 0" "the file has 8 new lines, each is 'runtimeClassName: gvisor', and no line is gone"

if command -v git >/dev/null 2>&1; then
  # git makes the new file from the old one and the diff of the dry-run.
  mkdir -p apply/k8s
  cp "$E2E_WORKLOADS" apply/k8s/workloads.yaml
  if (cd apply && git apply ../workloads.diff) >> "$ALL_OUTPUT" 2>&1; then
    expect_same apply/k8s/workloads.yaml k8s/workloads.yaml "git apply of the dry-run diff gives the file that the real run wrote"
  else
    fail "git apply reads the diff of the dry-run" "git apply failed"
  fi
else
  printf '  %s %s\n' "$(_am_muted "-")" "skipped: git is not installed (the git apply check)"
fi

run scan -f k8s --fail-on sandboxed -o json
expect_rc 0 "the scan of the patched file passes the sandboxed gate"
expect_jq '.spec.summary | "\(.total) \(.sandboxed)"' "14 8" "the scan finds the class on the 8 workloads"

cp k8s/workloads.yaml once.yaml
run patch -f k8s --dry-run=false -o json
expect_rc 0 "a second patch exits 0"
expect_jq '.spec.summary.selected' "0" "a second patch selects nothing"
expect_jq '.spec.files | length' "0" "a second patch has no diff"
expect_same k8s/workloads.yaml once.yaml "a second patch changed no file"

run patch -f k8s -o markdown
expect_rc 0 "patch -o markdown works"
expect_text "$OUT" "**Nothing to change.**" "markdown: the first lines say that there is nothing to change"

# Forms that are not edited: reported, with exit 3, and left as they are.
run patch -f forms --dry-run=false -o json
expect_rc 3 "one workload patched and two in a form that is not edited: exit 3"
expect_jq '.spec.summary | "\(.patched) \(.notEdited)"' "1 2" "the block-style Pod is patched; the flow-style and the JSON Pod are not"
expect_jq '[.spec.changes[] | select(.status == "not-edited") | .reason | length > 0] | all' "true" "each one has a reason"
expect_same forms/flow.yaml "$REPO_ROOT/test/testdata/patch/forms/flow.yaml" "the flow-style file is as it was"
expect_same forms/pod.json "$REPO_ROOT/test/testdata/patch/forms/pod.json" "the JSON file is as it was"
expect_text forms/block.yaml "  runtimeClassName: gvisor" "the block-style file has the class"

run patch -f forms/pod.json -f forms/flow.yaml -o json
expect_rc 1 "no selected workload can be patched: exit 1 (a dry-run too)"

# A Helm template is not YAML. patch stops before it writes a file.
run patch -f chart -f kustomize --dry-run=false
expect_rc 1 "a Helm template in the input: exit 1"
expect_empty "$OUT" "no report on stdout"
expect_text "$ERR" "A Helm template cannot be edited" "the error says that a template cannot be edited"
expect_same kustomize/base/web.yaml "$REPO_ROOT/test/testdata/patch/kustomize/base/web.yaml" "no file was written"

run patch -f - < /dev/null
expect_rc 1 "standard input is refused: it has no file to edit"
expect_text "$ERR" "--kustomize <dir>" "the error names the form that reads standard input"

# Kustomize: the base is a file that a person edits, so patch edits it. The
# overlay patch is a partial object and is never a target.
run patch -f kustomize --dry-run=false -o json
expect_rc 0 "patch of a Kustomize tree exits 0"
expect_jq '.spec.changes | map("\(.target.name) \(.status) \(.origin.file)") | join(",")' "web patched kustomize/base/web.yaml" \
  "the Deployment of the base is patched"
expect_jq '.metadata.source.unrendered | length > 0' "true" "the result lists the signs that the input is not rendered"
expect_text "$ERR" "this input is not rendered" "a warning says the input is not rendered"
expect_same kustomize/overlay/replicas.yaml "$REPO_ROOT/test/testdata/patch/kustomize/overlay/replicas.yaml" "the overlay patch is as it was"
BASE_DIFF="$(diff "$REPO_ROOT/test/testdata/patch/kustomize/base/web.yaml" kustomize/base/web.yaml | grep '^[<>]' || true)"
expect_eq "$BASE_DIFF" ">       runtimeClassName: gvisor" "the base has one new line; comments and quotes are as they were"

if command -v kubectl >/dev/null 2>&1; then
  # `kubectl kustomize` is a local render. It contacts no cluster.
  kubectl kustomize kustomize/overlay > rendered.yaml 2>> "$ALL_OUTPUT"
  run scan -f rendered.yaml --fail-on sandboxed -o json
  expect_rc 0 "rendered: the overlay of the patched base passes the gate"
  expect_jq '.spec.workloads | map("\(.name) \(.namespace) \(.runtimeClassName)") | join(",")' "prod-web shop gvisor" \
    "rendered: the Deployment of the overlay has the class"
else
  printf '  %s %s\n' "$(_am_muted "-")" "skipped: kubectl is not installed (the render of the patched Kustomize tree)"
fi

popd > /dev/null

# -----------------------------------------------------------------------------
section "9. patch --kustomize: one overlay first"
# -----------------------------------------------------------------------------

# The tree: a base with five workloads (four can run in the sandbox, in four
# forms), and two overlays of it, staging and prod. The base gives each
# object a name prefix and a namespace, so the names that an overlay gets
# are not the names in the files. Only staging must move.
COMPONENT_FIXTURE="$REPO_ROOT/test/testdata/patch/component"
KZ_DIR="$WORK_DIR/component"
mkdir -p "$KZ_DIR"
cp -R "$COMPONENT_FIXTURE" "$KZ_DIR/tree"
pushd "$KZ_DIR" > /dev/null

# use_component <kustomization directory> <path>: what the user does by hand
# after a run: add the directory to `components:`.
use_component() {
  printf 'components:\n  - %s\n' "$2" >> "$1/kustomization.yaml"
}

# rendered_with_class <kustomization directory>: the names of the workloads
# that have the class in the build of the directory, sorted, joined with ",".
rendered_with_class() {
  kubectl kustomize "$1" 2>> "$ALL_OUTPUT" > "$WORK_DIR/rendered.yaml" || { echo "<the build failed>"; return 0; }
  "$AGENTMOAT_BIN" scan -f "$WORK_DIR/rendered.yaml" --fail-on none -o json -q 2>> "$ALL_OUTPUT" \
    | jq -r '[.spec.workloads[] | select(.runtimeClassName == "gvisor") | .name] | sort | join(",")'
}

# expect_build_fails <kustomization directory> <text> <label>: `kubectl
# kustomize` stops, and its message has the text.
expect_build_fails() {
  local msg
  if msg="$(kubectl kustomize "$1" 2>&1 > /dev/null)"; then
    fail "$3" "the build passed"
  elif [[ "$msg" == *"$2"* ]]; then
    pass "$3"
  else
    fail "$3" "the build failed, and its message does not have '$2': ${msg:0:300}"
  fi
}

# The dry-run is the default here too. It makes no directory.
run patch -f tree/base --kustomize tree/staging/gvisor -o json
expect_rc 0 "patch --kustomize (dry-run, the default) exits 0"
expect_jq '.metadata | "\(.dryRun) \(.kustomizeDir)"' "true tree/staging/gvisor" "the result says that it is a dry-run, and names the component directory"
expect_jq '.spec.summary | "\(.total) \(.selected) \(.patched) \(.notEdited) \(.excluded) \(.files)"' "5 4 4 0 1 5" \
  "4 of 5 workloads get a patch file (also the flow-style Pod); 5 files: 4 patches and the list"
expect_jq '.spec.files | map(.created and (.written | not) and (.diff | startswith("--- /dev/null\n"))) | all' "true" \
  "each file is new and not written, and its diff is from /dev/null"
expect_text "$ERR" 'to `components:` in the kustomization that must move' "stderr says what to do with the directory"
if [[ -e tree/staging/gvisor ]]; then
  fail "the dry-run made no directory" "tree/staging/gvisor is there"
else
  pass "the dry-run made no directory"
fi
jq -r '.spec.files[].diff' "$OUT" > component.diff

run patch -f tree/base --kustomize tree/staging/gvisor --dry-run=false -o json
expect_rc 0 "patch --kustomize --dry-run=false exits 0"
expect_jq '.spec.files | map(.written) | all' "true" "the 5 files are written"
expect_jq '.spec.changes | map(.patchFile | sub("tree/staging/gvisor/"; "")) | sort | join(" ")' \
  "cronjob.batch_batch_nightly.yaml deployment.apps_web.yaml pod_once.yaml rollout.argoproj.io_canary.yaml" \
  "each change names its patch file: the kind, the namespace when the object has one, and the name"
expect_eq "$(find tree/staging/gvisor -type f | wc -l | tr -d ' ')" "5" "the directory has the 5 files and no other file"
expect_text tree/staging/gvisor/cronjob.batch_batch_nightly.yaml "  namespace: batch" "the patch of the CronJob has the namespace of the file"
if grep -q "namespace" tree/staging/gvisor/deployment.apps_web.yaml; then
  fail "the patch of the Deployment names no namespace" "it has one"
else
  pass "the patch of the Deployment names no namespace: the file has none"
fi
if diff -r "$COMPONENT_FIXTURE/base" tree/base >> "$ALL_OUTPUT" 2>&1; then
  pass "no source file changed: the base is as it was"
else
  fail "no source file changed" "tree/base is different from the fixture"
fi

if command -v git >/dev/null 2>&1; then
  # The diff of the dry-run is what a reviewer approves. git makes the same
  # files from it.
  mkdir -p apply
  if (cd apply && git apply ../component.diff) >> "$ALL_OUTPUT" 2>&1 \
    && diff -r apply/tree/staging/gvisor tree/staging/gvisor >> "$ALL_OUTPUT" 2>&1; then
    pass "git apply of the dry-run diff gives the files that the real run wrote"
  else
    fail "git apply of the dry-run diff gives the files that the real run wrote" "git apply failed, or the files are different"
  fi
else
  printf '  %s %s\n' "$(_am_muted "-")" "skipped: git is not installed (the git apply check)"
fi

cp -R tree/staging/gvisor once
run patch -f tree/base --kustomize tree/staging/gvisor --dry-run=false -o json
expect_rc 0 "a second run exits 0"
expect_jq '.spec.summary | "\(.selected) \(.excluded) \(.files)"' "0 5 0" "a second run selects nothing: the component has the patches"
if diff -r once tree/staging/gvisor >> "$ALL_OUTPUT" 2>&1; then
  pass "a second run changed no file"
else
  fail "a second run changed no file" "the directory is different"
fi

run patch -f tree/base --kustomize tree/prod/gvisor -o markdown
expect_rc 0 "patch --kustomize -o markdown works"
expect_text "$OUT" '**Dry run.** No file was changed. 5 file(s) would be written in the Kustomize component `tree/prod/gvisor`' \
  "markdown: the first lines say what would be written, and where"
expect_text "$OUT" "**To use the component:**" "markdown: the report says what to do with the directory"

# A directory of a person is refused, and nothing is written into it.
run patch -f tree/base --kustomize tree/staging --dry-run=false
expect_rc 1 "the directory of another kustomization is refused"
expect_empty "$OUT" "no report on stdout"
expect_text "$ERR" "does not start with the line" "the error says why: the kustomization.yaml there is not one of agentmoat"
expect_same tree/staging/kustomization.yaml "$COMPONENT_FIXTURE/staging/kustomization.yaml" "the kustomization of the overlay is as it was"

if command -v kubectl >/dev/null 2>&1; then
  # `kubectl kustomize` is a local build. It contacts no cluster.

  # Input 1: the files of the base. The names in the patches are the names
  # in the files, and Kustomize finds the objects although the base gave
  # them a prefix and another namespace.
  expect_eq "$(rendered_with_class tree/staging)" "" "before the overlay uses the component, its build has no class"
  use_component tree/staging gvisor
  expect_eq "$(rendered_with_class tree/staging)" "staging-b-canary,staging-b-nightly,staging-b-once,staging-b-web" \
    "staging: the four workloads have the class; the DaemonSet with host networking has none"
  expect_eq "$(rendered_with_class tree/prod)" "" "prod uses the same base and has no class: only one overlay moved"
  kubectl kustomize tree/staging > staging.yaml 2>> "$ALL_OUTPUT"
  run scan -f staging.yaml --fail-on sandboxed -o json
  expect_rc 0 "staging, rendered: the gate passes"
  expect_jq '.spec.summary | "\(.total) \(.sandboxed)"' "5 4" "staging, rendered: 4 of 5 workloads are in the sandbox"
  expect_text staging.yaml "  replicas: 3" "staging, rendered: the patch of the overlay itself still works"

  # Input 2: the output of the base, on standard input. The names in the
  # patches are then the names that the overlay reads.
  kubectl kustomize tree/base > base-rendered.yaml 2>> "$ALL_OUTPUT"
  run patch -f - --kustomize tree/prod/gvisor --dry-run=false -o json < base-rendered.yaml
  expect_rc 0 "patch -f - --kustomize reads the output of the base from standard input"
  expect_jq '.spec.summary | "\(.patched) \(.files)"' "4 5" "4 workloads get a patch file"
  expect_text tree/prod/gvisor/deployment.apps_team_b-web.yaml "  name: b-web" "the patch has the name that the base gives the object"
  use_component tree/prod gvisor
  expect_eq "$(rendered_with_class tree/prod)" "prod-b-canary,prod-b-nightly,prod-b-once,prod-b-web" \
    "prod: with patches made from the output of the base, the four workloads have the class"

  # Input 3: a directory with a kustomization and no overlay. It uses the
  # component itself.
  cp -R "$REPO_ROOT/test/testdata/patch/kustomize/base" plain
  run patch -f plain --kustomize plain/gvisor --dry-run=false -o json
  expect_rc 0 "a plain kustomization: exit 0"
  use_component plain gvisor
  expect_eq "$(rendered_with_class plain)" "web" "a plain kustomization that uses the component has the class"
  expect_same plain/web.yaml "$REPO_ROOT/test/testdata/patch/kustomize/base/web.yaml" "its source file is as it was"

  # What Kustomize refuses. patch cannot know which kustomization will use
  # the component, so it cannot check these two. The build stops with a
  # message that names the patch: the error is not silent.
  #
  # (a) The input has more than the kustomization reads.
  cp -R "$COMPONENT_FIXTURE" more
  run patch -f more/base -f more/other --kustomize more/staging/gvisor --dry-run=false -o json
  expect_rc 0 "input with one object more than the overlay reads: patch exits 0"
  use_component more/staging gvisor
  expect_build_fails more/staging "stray" "the build of that overlay stops, and names the object that it does not read"
  # (b) The input is the output of the kustomization that uses the component.
  cp -R "$COMPONENT_FIXTURE" same
  kubectl kustomize same/staging > same-rendered.yaml 2>> "$ALL_OUTPUT"
  run patch -f same-rendered.yaml --kustomize same/staging/gvisor --dry-run=false -o json
  expect_rc 0 "input that is the output of the same overlay: patch exits 0"
  use_component same/staging gvisor
  expect_build_fails same/staging "staging-b-" "the build of that overlay stops: it reads the objects before it gives them its prefix"

  if command -v helm >/dev/null 2>&1; then
    # A Helm chart: a template cannot be edited, and its output can be the
    # input. The patches name the rendered objects, so they are for a
    # kustomization that reads the rendered output. `helm template` is a
    # local render. It contacts no cluster.
    mkdir -p charts/web/templates deploy
    printf 'apiVersion: v2\nname: web\nversion: 0.1.0\n' > charts/web/Chart.yaml
    cat > charts/web/templates/deployment.yaml <<'EOF'
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
      containers:
        - name: web
          image: example.test/web:1
EOF
    helm template shop charts/web --namespace team > deploy/rendered.yaml 2>> "$ALL_OUTPUT"
    run patch -f - --kustomize deploy/gvisor --dry-run=false -o json < deploy/rendered.yaml
    expect_rc 0 "Helm: patch -f - --kustomize reads the output of helm template"
    expect_jq '.spec.changes | map("\(.status) \(.patchFile) \(.origin.file)") | join(",")' \
      "patched deploy/gvisor/deployment.apps_team_shop-web.yaml web/templates/deployment.yaml" \
      "Helm: the patch names the rendered object, and the change names the template that made it"
    printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - rendered.yaml\n' > deploy/kustomization.yaml
    use_component deploy gvisor
    expect_eq "$(rendered_with_class deploy)" "shop-web" "Helm: a kustomization that reads the rendered output and uses the component has the class"
  else
    printf '  %s %s\n' "$(_am_muted "-")" "skipped: helm is not installed (the component for the output of a chart)"
  fi
else
  printf '  %s %s\n' "$(_am_muted "-")" "skipped: kubectl is not installed (the builds of the component)"
fi

popd > /dev/null

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
