#!/usr/bin/env bash
# agentmoat: run `agentmoat probe seccomp` for real against a cluster and
# check what it reports.
#
# What it checks, with the three Deployments in
# test/testdata/seccomp-probe/workloads.yaml (all runtimeClassName gvisor):
#   1. The dry run (the default) creates no pod and reports
#      seccomp-probe-dry-run.
#   2. --dry-run=false runs one trial per workload on the audit RuntimeClass
#      and reads the Sentry's debug logs back:
#        * seccomp-blocked (RuntimeDefault, calls unshare -U) has unshare
#          in its blocked list, with action "denied", and the
#          seccomp-blocked-syscalls warning names it
#        * seccomp-clean (RuntimeDefault, only sleeps) loaded a filter and
#          has no blocked syscall
#        * seccomp-unconfined loaded one filter fewer than seccomp-clean
#          (its app container has none; the pause container has one), has
#          no blocked syscall, and the seccomp-unconfined finding names it
#        * every trial read a runsc release of 20261123.1 or newer
#        * no trial or reader pod is left behind
#
# Needs: a built ./bin/agentmoat (make build), kubectl, jq, and a cluster
# with the gvisor and gvisor-seccomp-audit RuntimeClasses whose nodes
# register both handlers: the kind cluster from `make kind-up` with
# deploy/runtimeclass.yaml and deploy/runtimeclass-seccomp-audit.yaml
# applied. CI runs it there (the kind-seccomp job in .github/workflows/ci.yml).
#
# Usage:
#   ./scripts/seccomp-probe-smoke.sh
#   KUBE_CONTEXT=kind-agentmoat-e2e KEEP=1 ./scripts/seccomp-probe-smoke.sh
#
# Exit codes: 0 all checks passed, 1 a check failed.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AGENTMOAT="${AGENTMOAT:-$REPO_ROOT/bin/agentmoat}"
KUBECTL="${KUBECTL:-kubectl}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
NAMESPACE="${NAMESPACE:-agentmoat-seccomp-probe-smoke}"
RUN_FOR="${RUN_FOR:-20s}"
KEEP="${KEEP:-0}"

KCTL=("$KUBECTL")
AGENT=("$AGENTMOAT")
if [[ -n "$KUBE_CONTEXT" ]]; then
  KCTL+=(--context "$KUBE_CONTEXT")
  AGENT+=(--context "$KUBE_CONTEXT")
fi

WORK_DIR="$(mktemp -d)"
FAILS=0

fail() { printf 'FAIL: %s\n' "$*" >&2; FAILS=$((FAILS + 1)); }
pass() { printf 'ok:   %s\n' "$*"; }
check_eq() {
  local what="$1" want="$2" got="$3"
  if [[ "$want" == "$got" ]]; then pass "$what"; else fail "$what: expected '$want', got '$got'"; fi
}

probe_pods() {
  "${KCTL[@]}" get pods -A -l 'app.kubernetes.io/component in (seccomp-trial,seccomp-reader)' -o name 2>/dev/null | wc -l | tr -d ' '
}

cleanup() {
  local code=$?
  if [[ "$KEEP" != "1" ]]; then
    "${KCTL[@]}" delete namespace "$NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
  if (( code != 0 )) && [[ -s "$WORK_DIR/probe.json" ]]; then
    echo "--- probe report (JSON) ---" >&2
    jq . "$WORK_DIR/probe.json" >&2 || cat "$WORK_DIR/probe.json" >&2
  fi
  rm -rf "$WORK_DIR"
  exit "$code"
}
trap cleanup EXIT

[[ -x "$AGENTMOAT" ]] || { echo "error: $AGENTMOAT not found; run make build" >&2; exit 1; }
command -v jq >/dev/null 2>&1 || { echo "error: jq not found" >&2; exit 1; }
for rc in gvisor gvisor-seccomp-audit; do
  "${KCTL[@]}" get runtimeclass "$rc" >/dev/null 2>&1 ||
    { echo "error: RuntimeClass $rc not found (apply deploy/runtimeclass.yaml and deploy/runtimeclass-seccomp-audit.yaml)" >&2; exit 1; }
done

echo "deploying the three test workloads into namespace $NAMESPACE"
"${KCTL[@]}" create namespace "$NAMESPACE" --dry-run=client -o yaml | "${KCTL[@]}" apply -f - >/dev/null
"${KCTL[@]}" -n "$NAMESPACE" apply -f "$REPO_ROOT/test/testdata/seccomp-probe/workloads.yaml" >/dev/null
for d in seccomp-blocked seccomp-clean seccomp-unconfined; do
  "${KCTL[@]}" -n "$NAMESPACE" rollout status "deployment/$d" --timeout=180s >/dev/null
done

# ---- 1. dry run ------------------------------------------------------------
echo "probe seccomp (dry run)"
code=0
"${AGENT[@]}" probe seccomp -n "$NAMESPACE" -o json >"$WORK_DIR/dry.json" 2>"$WORK_DIR/dry.err" || code=$?
check_eq "dry run exit code" 0 "$code"
check_eq "dry run metadata.seccompProbe.dryRun" true "$(jq -r '.metadata.seccompProbe.dryRun' "$WORK_DIR/dry.json")"
check_eq "dry run has the seccomp-probe-dry-run finding" true \
  "$(jq -r '[.spec.findings[].id] | index("seccomp-probe-dry-run") != null' "$WORK_DIR/dry.json")"
check_eq "dry run workloads" 3 "$(jq -r '.spec.facts.seccomp.workloads | length' "$WORK_DIR/dry.json")"
check_eq "dry run created no pod" 0 "$(probe_pods)"

# ---- 2. real run -----------------------------------------------------------
echo "probe seccomp --dry-run=false --run-for $RUN_FOR"
code=0
"${AGENT[@]}" probe seccomp -n "$NAMESPACE" --dry-run=false --run-for "$RUN_FOR" -o json \
  >"$WORK_DIR/probe.json" 2>"$WORK_DIR/probe.err" || code=$?
cat "$WORK_DIR/probe.err" >&2
check_eq "probe exit code" 0 "$code"
P="$WORK_DIR/probe.json"
wl() { jq -r --arg n "$1" ".spec.facts.seccomp.workloads[] | select(.target.name == \$n) | $2" "$P"; }

for n in seccomp-blocked seccomp-clean seccomp-unconfined; do
  check_eq "$n trial status" ran "$(wl "$n" '.status')"
  v=$(wl "$n" '.runscVersion')
  if [[ "$v" =~ ^release-([0-9]{8})\.([0-9]+)$ ]] &&
     { (( 10#${BASH_REMATCH[1]} > 20261123 )) || { (( 10#${BASH_REMATCH[1]} == 20261123 )) && (( BASH_REMATCH[2] >= 1 )); }; }; then
    pass "$n runsc $v is 20261123.1 or newer"
  else
    fail "$n runscVersion '$v' is not release-20261123.1 or newer"
  fi
done

check_eq "seccomp-blocked: unshare denied" true \
  "$(wl seccomp-blocked '[.blocked[]? | select(.name == "unshare" and .action == "denied")] | length > 0')"
check_eq "seccomp-blocked: a filter was loaded" true "$(wl seccomp-blocked '.filtersLoaded > 0')"
check_eq "seccomp-clean: a filter was loaded" true "$(wl seccomp-clean '.filtersLoaded > 0')"
check_eq "seccomp-clean: nothing blocked" 0 "$(wl seccomp-clean '.blocked // [] | length')"
# filtersLoaded counts every container of the sandbox, the pause container
# included, and containerd gives the pause container a filter of its own. So
# the Unconfined workload loads one filter fewer than the clean one: its app
# container adds none.
check_eq "seccomp-unconfined: its app container loaded no filter" \
  "$(( $(wl seccomp-clean '.filtersLoaded') - 1 ))" "$(wl seccomp-unconfined '.filtersLoaded')"
check_eq "seccomp-unconfined: nothing blocked" 0 "$(wl seccomp-unconfined '.blocked // [] | length')"

check_eq "seccomp-blocked-syscalls names seccomp-blocked" true \
  "$(jq -r '[.spec.findings[] | select(.id == "seccomp-blocked-syscalls" and (.message | contains("seccomp-blocked")))] | length == 1' "$P")"
check_eq "seccomp-unconfined finding names seccomp-unconfined" true \
  "$(jq -r '[.spec.findings[] | select(.id == "seccomp-unconfined" and (.message | contains("seccomp-unconfined")))] | length == 1' "$P")"
check_eq "no seccomp-filter-not-loaded finding" 0 \
  "$(jq -r '[.spec.findings[] | select(.id == "seccomp-filter-not-loaded")] | length' "$P")"
check_eq "no trial or reader pod left behind" 0 "$(probe_pods)"

if (( FAILS > 0 )); then
  echo "$FAILS check(s) failed" >&2
  exit 1
fi
echo "PASS: probe seccomp reported the blocked syscall, the clean workload and the Unconfined one"
