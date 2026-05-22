#!/usr/bin/env bash
# agentmoat: end-to-end smoke test against a real kind cluster.
#
# What this exercises
#
#   1. `agentmoat scan`     against the kind API server (exit 2 path, JSON
#                           output, summary counts, per-workload kinds).
#   2. `agentmoat plan`     over the stored ScanReport (deterministic
#                           ordering, two steps, PlanHash present).
#   3. `agentmoat apply`    in dry-run mode (default), then again with
#                           --dry-run=false (real strategic-merge patch),
#                           then a third time to prove idempotency.
#   4. `agentmoat verify --in-pod-probe`  confirms each patched workload
#                           actually runs on runsc by exec-ing a probe
#                           inside the pod and grepping dmesg / proc for
#                           gVisor markers. Run three times: after real
#                           apply, after idempotent re-apply (still all
#                           ok), and after rollback (expect mismatch=2
#                           and exit 4).
#   5. `agentmoat rollback` to clear the patches and restore the
#                           pre-apply spec.
#   6. `agentmoat explain`  smoke: list mode, known topic, unknown topic
#                           (exit non-zero, stderr lists topics).
#
# Real gVisor execution: the kind worker is built from
# kind/Dockerfile.gvisor-node and ships runsc + the containerd v2 shim.
# The RuntimeClass uses handler=gvisor so `verify --in-pod-probe` will
# actually see the gVisor Sentry signature in the pod's dmesg/proc.
#
# Environment knobs
#
#   CLUSTER_NAME    kind cluster name (default: agentmoat-e2e)
#   NAMESPACE       namespace to deploy workloads into (default: agentmoat-e2e)
#   KEEP_CLUSTER    set to 1 to leave the cluster running on exit (default: 0)
#   AGENTMOAT_BIN   path to the agentmoat binary (default: bin/agentmoat)
#   VERBOSE         set to 1 to print every command before it runs
#
# Exit codes
#
#   0  end-to-end succeeded
#   1  any step failed; an assertion message names which one

set -euo pipefail

# Pipe stdout/stderr through `tee` so the user always sees what the
# subcommands print, even when CI captures the log.
[[ "${VERBOSE:-0}" == "1" ]] && set -x

# -----------------------------------------------------------------------------
# Config
# -----------------------------------------------------------------------------

CLUSTER_NAME="${CLUSTER_NAME:-agentmoat-e2e}"
NAMESPACE="${NAMESPACE:-agentmoat-e2e}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
AGENTMOAT_BIN="${AGENTMOAT_BIN:-$REPO_ROOT/bin/agentmoat}"
MANIFEST_DIR="$REPO_ROOT/test/e2e/manifests"
WORK_DIR="$(mktemp -d -t agentmoat-e2e.XXXX)"

# kubectl talks to the kind context kind has created. We pin the context
# explicitly on every kubectl call so a stray KUBECONFIG never targets
# the wrong cluster.
CTX="kind-$CLUSTER_NAME"
KCTL=(kubectl --context "$CTX")
AGENT=("$AGENTMOAT_BIN" --context "$CTX")

# Toggled by the assertion helpers below; lets cleanup print a summary.
FAIL_COUNT=0

# -----------------------------------------------------------------------------
# Helpers
# -----------------------------------------------------------------------------

log() {
  printf '\n=== %s\n' "$*"
}

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  FAIL_COUNT=$((FAIL_COUNT + 1))
  exit 1
}

# assert_eq: assert the literal equality of two values. Used for JSON
# scalars and short strings; not for free-form text.
assert_eq() {
  local what="$1" expected="$2" actual="$3"
  if [[ "$expected" != "$actual" ]]; then
    fail "$what: expected '$expected', got '$actual'"
  fi
}

# wait_runtime_class: poll the Deployment/StatefulSet pod template until
# its runtimeClassName matches the expected value (empty string for
# "absent"). Polls every 2s up to 60s. The strategic-merge patch is
# synchronous on the API server, but kind's etcd may need a heartbeat
# before kubectl sees the change.
wait_runtime_class() {
  local kind="$1" name="$2" expected="$3" deadline=$((SECONDS + 60))
  while [[ $SECONDS -lt $deadline ]]; do
    local actual
    actual=$("${KCTL[@]}" -n "$NAMESPACE" get "$kind" "$name" \
      -o jsonpath='{.spec.template.spec.runtimeClassName}' 2>/dev/null || true)
    if [[ "$actual" == "$expected" ]]; then
      return 0
    fi
    sleep 2
  done
  local final
  final=$("${KCTL[@]}" -n "$NAMESPACE" get "$kind" "$name" \
    -o jsonpath='{.spec.template.spec.runtimeClassName}' 2>/dev/null || echo "<missing>")
  fail "$kind/$name runtimeClassName: expected '$expected', got '$final' (after 60s wait)"
}

cleanup() {
  local code=$?
  if [[ "$KEEP_CLUSTER" != "1" ]]; then
    log "tearing down kind cluster '$CLUSTER_NAME'"
    "$REPO_ROOT/scripts/kind-down.sh" >/dev/null 2>&1 || true
  else
    log "leaving kind cluster '$CLUSTER_NAME' running (KEEP_CLUSTER=1)"
  fi
  rm -rf "$WORK_DIR"
  if [[ $code -eq 0 ]]; then
    printf '\ne2e OK\n'
  else
    printf '\ne2e FAILED (exit %d)\n' "$code"
  fi
}
trap cleanup EXIT

# -----------------------------------------------------------------------------
# 0. Pre-flight: binary + cluster + manifests.
# -----------------------------------------------------------------------------

log "building agentmoat binary (if missing)"
if [[ ! -x "$AGENTMOAT_BIN" ]]; then
  (cd "$REPO_ROOT" && make build)
fi
[[ -x "$AGENTMOAT_BIN" ]] || fail "agentmoat binary not found at $AGENTMOAT_BIN"

log "bringing up kind cluster '$CLUSTER_NAME'"
"$REPO_ROOT/scripts/kind-up.sh"

# Wait for every node (control-plane + gVisor worker) to be Ready. kind's
# own --wait blocks on the control-plane; the gVisor worker can take a
# few extra seconds because the runtime registration is parsed and the
# kubelet does its first pull. Be generous.
"${KCTL[@]}" wait --for=condition=Ready node --all --timeout=180s

log "creating namespace '$NAMESPACE' and applying RuntimeClass + workloads"
"${KCTL[@]}" apply -f "$MANIFEST_DIR/runtimeclass.yaml"
"${KCTL[@]}" create namespace "$NAMESPACE" --dry-run=client -o yaml | "${KCTL[@]}" apply -f -
"${KCTL[@]}" -n "$NAMESPACE" apply -f "$MANIFEST_DIR/workloads.yaml"

log "waiting for workloads to be Ready"
"${KCTL[@]}" -n "$NAMESPACE" wait --for=condition=Available deployment/web --timeout=180s
"${KCTL[@]}" -n "$NAMESPACE" rollout status statefulset/cache --timeout=180s
# host-net's Pod may stay Pending on some kind setups; we tolerate that
# (the scanner only needs to see the spec). A 30s grace gives the pod
# a chance to register before we scan.
"${KCTL[@]}" -n "$NAMESPACE" wait --for=condition=PodScheduled pod/host-net --timeout=30s || true

# -----------------------------------------------------------------------------
# 1. scan: expect exit 2 (host-net is incompatible) and the right counts.
# -----------------------------------------------------------------------------

log "scan: enumerate and classify"
set +e
"${AGENT[@]}" scan --namespace "$NAMESPACE" --output json >"$WORK_DIR/scan.json"
SCAN_EXIT=$?
set -e
assert_eq "scan exit code" 2 "$SCAN_EXIT"

# The classifier groups by controller, so bare Pods (host-net plus the
# rule-coverage pods in workloads.yaml) report as Pod; the Deployment and
# StatefulSet report as their controller kinds. The expected workloads
# are: web (Deployment, compatible), cache (StatefulSet, compatible),
# host-net + 6 rule-coverage pods (Pod, incompatible), and 5 rule-coverage
# pods (Pod, review). Total = 14.
SCAN_TOTAL=$(jq -r '.spec.summary.total' "$WORK_DIR/scan.json")
SCAN_COMPAT=$(jq -r '.spec.summary.compatible' "$WORK_DIR/scan.json")
SCAN_REVIEW=$(jq -r '.spec.summary.needsReview' "$WORK_DIR/scan.json")
SCAN_INCOMPAT=$(jq -r '.spec.summary.incompatible' "$WORK_DIR/scan.json")
assert_eq "scan summary.total" 14 "$SCAN_TOTAL"
assert_eq "scan summary.compatible" 2 "$SCAN_COMPAT"
assert_eq "scan summary.needsReview" 5 "$SCAN_REVIEW"
assert_eq "scan summary.incompatible" 7 "$SCAN_INCOMPAT"

# Cross-check that the right workloads landed in the right buckets. `unique`
# collapses the 12 Pod kinds so the assertion stays readable.
KINDS=$(jq -r '.spec.workloads | map(.kind) | unique | sort | join(",")' "$WORK_DIR/scan.json")
assert_eq "scan workload kinds" "Deployment,Pod,StatefulSet" "$KINDS"

INCOMPAT_NAMES=$(jq -r '.spec.workloads | map(select(.compatibility=="incompatible") | .name) | sort | join(",")' "$WORK_DIR/scan.json")
assert_eq "scan incompatible workloads" \
  "ebpf-app,host-ipc-app,host-net,host-pid-app,kvm-app,priv-app,raw-socket-app" \
  "$INCOMPAT_NAMES"
REVIEW_NAMES=$(jq -r '.spec.workloads | map(select(.compatibility=="review") | .name) | sort | join(",")' "$WORK_DIR/scan.json")
assert_eq "scan review workloads" \
  "fuse-app,gpu-app,hostpath-app,iouring-app,perf-app" \
  "$REVIEW_NAMES"

# -----------------------------------------------------------------------------
# 2. plan: deterministic, 2 steps (compatible only), non-empty PlanHash.
# -----------------------------------------------------------------------------
#
# The applier accepts JSON or YAML plans (kind sniff on read), and using
# JSON here lets us assert via jq instead of pulling in a YAML parser.

log "plan: produce MigrationPlan"
"${AGENT[@]}" plan --scan "$WORK_DIR/scan.json" --output json >"$WORK_DIR/plan.json"

PLAN_KIND=$(jq -r '.kind' "$WORK_DIR/plan.json")
assert_eq "plan kind" "MigrationPlan" "$PLAN_KIND"

# PlanSummary.Total is `included + excluded`; the included subset is
# what the applier will actually patch.
PLAN_TOTAL=$(jq -r '.spec.summary.total' "$WORK_DIR/plan.json")
PLAN_INCLUDED=$(jq -r '.spec.summary.included' "$WORK_DIR/plan.json")
PLAN_EXCLUDED=$(jq -r '.spec.summary.excluded' "$WORK_DIR/plan.json")
PLAN_STEP_COUNT=$(jq -r '.spec.steps | length' "$WORK_DIR/plan.json")
PLAN_HASH=$(jq -r '.metadata.planHash' "$WORK_DIR/plan.json")
assert_eq "plan summary.total" 14 "$PLAN_TOTAL"
assert_eq "plan summary.included" 2 "$PLAN_INCLUDED"
assert_eq "plan summary.excluded" 12 "$PLAN_EXCLUDED"
assert_eq "plan spec.steps length" 2 "$PLAN_STEP_COUNT"
[[ -n "$PLAN_HASH" && "$PLAN_HASH" != "null" ]] || fail "plan metadata.planHash is empty"

# Plan determinism: re-running the planner on the same scan must produce
# the same PlanHash. Cheap insurance against accidental clock-dependent
# fields creeping into the plan body.
"${AGENT[@]}" plan --scan "$WORK_DIR/scan.json" --output json >"$WORK_DIR/plan2.json"
PLAN_HASH_2=$(jq -r '.metadata.planHash' "$WORK_DIR/plan2.json")
assert_eq "plan determinism (re-run hash)" "$PLAN_HASH" "$PLAN_HASH_2"

# -----------------------------------------------------------------------------
# 3. apply --dry-run: no mutation; Deployment template runtimeClassName empty.
# -----------------------------------------------------------------------------

log "apply (dry-run): preview patches only"
"${AGENT[@]}" apply --plan "$WORK_DIR/plan.json" --output json --no-audit >"$WORK_DIR/apply-dry.json"

DRY_RUN_FLAG=$(jq -r '.metadata.dryRun' "$WORK_DIR/apply-dry.json")
DRY_APPLIED=$(jq -r '.spec.summary.applied' "$WORK_DIR/apply-dry.json")
DRY_FAILED=$(jq -r '.spec.summary.failed' "$WORK_DIR/apply-dry.json")
assert_eq "apply dry-run metadata.dryRun" "true" "$DRY_RUN_FLAG"
assert_eq "apply dry-run summary.applied" 2 "$DRY_APPLIED"
assert_eq "apply dry-run summary.failed" 0 "$DRY_FAILED"

# Spec must still be unmutated.
WEB_RT=$("${KCTL[@]}" -n "$NAMESPACE" get deployment web -o jsonpath='{.spec.template.spec.runtimeClassName}')
CACHE_RT=$("${KCTL[@]}" -n "$NAMESPACE" get statefulset cache -o jsonpath='{.spec.template.spec.runtimeClassName}')
assert_eq "Deployment/web runtimeClassName after dry-run" "" "$WEB_RT"
assert_eq "StatefulSet/cache runtimeClassName after dry-run" "" "$CACHE_RT"

# -----------------------------------------------------------------------------
# 4. apply (real): patches land; pods carry runtimeClassName=gvisor.
# -----------------------------------------------------------------------------

log "apply: send real strategic-merge patches"
"${AGENT[@]}" apply --plan "$WORK_DIR/plan.json" --dry-run=false --output json --no-audit \
  >"$WORK_DIR/apply.json"

REAL_DRY_RUN=$(jq -r '.metadata.dryRun' "$WORK_DIR/apply.json")
REAL_APPLIED=$(jq -r '.spec.summary.applied' "$WORK_DIR/apply.json")
REAL_ALREADY=$(jq -r '.spec.summary.alreadyApplied' "$WORK_DIR/apply.json")
REAL_FAILED=$(jq -r '.spec.summary.failed' "$WORK_DIR/apply.json")
assert_eq "apply metadata.dryRun" "false" "$REAL_DRY_RUN"
assert_eq "apply summary.applied" 2 "$REAL_APPLIED"
assert_eq "apply summary.alreadyApplied" 0 "$REAL_ALREADY"
assert_eq "apply summary.failed" 0 "$REAL_FAILED"

wait_runtime_class deployment web "gvisor"
wait_runtime_class statefulset cache "gvisor"

# Namespace stamp: applier writes the plan hash to the namespace.
NS_HASH=$("${KCTL[@]}" get namespace "$NAMESPACE" \
  -o jsonpath='{.metadata.annotations.agentmoat\.io/plan-hash}')
assert_eq "namespace plan-hash annotation" "$PLAN_HASH" "$NS_HASH"

# Wait for the rolled deployment to be Ready again so the next apply runs
# against a steady-state spec.
"${KCTL[@]}" -n "$NAMESPACE" rollout status deployment/web --timeout=180s
"${KCTL[@]}" -n "$NAMESPACE" rollout status statefulset/cache --timeout=180s

# -----------------------------------------------------------------------------
# 4b. verify (post-apply, with --in-pod-probe): expect ok=2, exit 0.
# -----------------------------------------------------------------------------
#
# Single source of truth for "did the migration land": the verifier reads
# every plan step, fetches the live pods, checks .spec.runtimeClassName,
# and (with --in-pod-probe) execs a small script to confirm gVisor's
# Sentry markers are present (dmesg banner, /proc/version, uname). Two
# steps in this plan, so summary.ok must be 2.
#
# We use --in-pod-probe here (vs the cheaper field-only path used in the
# post-rollback case below) because the kind worker is gVisor-real and
# we want the e2e to catch a regression where a future containerd patch
# accidentally falls back to runc despite handler=gvisor surviving.

log "verify (post-apply, --in-pod-probe): expect ok=2, exit 0"
set +e
"${AGENT[@]}" verify --plan "$WORK_DIR/plan.json" --in-pod-probe \
  --output json >"$WORK_DIR/verify-after-apply.json"
VERIFY_EXIT=$?
set -e
assert_eq "verify exit code (post-apply)" 0 "$VERIFY_EXIT"
assert_eq "verify summary.ok (post-apply)" 2 \
  "$(jq -r '.spec.summary.ok' "$WORK_DIR/verify-after-apply.json")"
assert_eq "verify summary.mismatch (post-apply)" 0 \
  "$(jq -r '.spec.summary.mismatch' "$WORK_DIR/verify-after-apply.json")"
assert_eq "verify summary.error (post-apply)" 0 \
  "$(jq -r '.spec.summary.error' "$WORK_DIR/verify-after-apply.json")"
STATUSES=$(jq -r '.spec.results | map(.status) | sort | unique | join(",")' \
  "$WORK_DIR/verify-after-apply.json")
assert_eq "verify result statuses (post-apply)" "ok" "$STATUSES"
ACTUALS=$(jq -r '.spec.results | map(.actual) | sort | unique | join(",")' \
  "$WORK_DIR/verify-after-apply.json")
assert_eq "verify result actuals (post-apply)" "gvisor" "$ACTUALS"
# The probe should have detected gVisor markers in every pod it visited.
PROBE_DETECTED_COUNT=$(jq -r \
  '[.spec.results[].probe | select(.!=null) | select(.detected==true)] | length' \
  "$WORK_DIR/verify-after-apply.json")
assert_eq "verify probe detected count (post-apply)" 2 "$PROBE_DETECTED_COUNT"

# -----------------------------------------------------------------------------
# 5. idempotent re-apply: every step already-applied.
# -----------------------------------------------------------------------------

log "apply (re-run): expect already-applied for every step"
"${AGENT[@]}" apply --plan "$WORK_DIR/plan.json" --dry-run=false --output json --no-audit \
  >"$WORK_DIR/apply2.json"

RE_APPLIED=$(jq -r '.spec.summary.applied' "$WORK_DIR/apply2.json")
RE_ALREADY=$(jq -r '.spec.summary.alreadyApplied' "$WORK_DIR/apply2.json")
RE_FAILED=$(jq -r '.spec.summary.failed' "$WORK_DIR/apply2.json")
assert_eq "re-apply summary.applied" 0 "$RE_APPLIED"
assert_eq "re-apply summary.alreadyApplied" 2 "$RE_ALREADY"
assert_eq "re-apply summary.failed" 0 "$RE_FAILED"

# -----------------------------------------------------------------------------
# 5b. verify (post-idempotent-apply): still all-ok.
# -----------------------------------------------------------------------------
#
# Same cluster state as 4b, just re-running the probe to confirm the
# verifier is stateless and reads live cluster state (not the applier's
# in-memory result).

log "verify (post-idempotent-apply): still all-ok"
"${AGENT[@]}" verify --plan "$WORK_DIR/plan.json" --in-pod-probe \
  --output json >"$WORK_DIR/verify-after-reapply.json"
assert_eq "verify summary.ok (post-reapply)" 2 \
  "$(jq -r '.spec.summary.ok' "$WORK_DIR/verify-after-reapply.json")"
assert_eq "verify summary.mismatch (post-reapply)" 0 \
  "$(jq -r '.spec.summary.mismatch' "$WORK_DIR/verify-after-reapply.json")"

# -----------------------------------------------------------------------------
# 6. rollback: runtimeClassName cleared; namespace annotation removed.
# -----------------------------------------------------------------------------

log "rollback: clear runtimeClassName + plan-hash"
"${AGENT[@]}" rollback --plan "$WORK_DIR/plan.json" --dry-run=false --output json --no-audit \
  >"$WORK_DIR/rollback.json"

RB_APPLIED=$(jq -r '.spec.summary.applied' "$WORK_DIR/rollback.json")
RB_FAILED=$(jq -r '.spec.summary.failed' "$WORK_DIR/rollback.json")
assert_eq "rollback summary.applied" 2 "$RB_APPLIED"
assert_eq "rollback summary.failed" 0 "$RB_FAILED"

wait_runtime_class deployment web ""
wait_runtime_class statefulset cache ""

# Wait for the rollback to actually roll new pods. The applier patches the
# template synchronously, but the controller still needs to terminate the
# gVisor pods and start fresh runc pods. Without this wait the post-
# rollback verify can see either zero alive pods (transient) or still see
# the gVisor-tagged pods (DeletionTimestamp not yet set) and produce the
# wrong verdict.
"${KCTL[@]}" -n "$NAMESPACE" rollout status deployment/web --timeout=180s
"${KCTL[@]}" -n "$NAMESPACE" rollout status statefulset/cache --timeout=180s

NS_HASH_AFTER=$("${KCTL[@]}" get namespace "$NAMESPACE" \
  -o jsonpath='{.metadata.annotations.agentmoat\.io/plan-hash}')
assert_eq "namespace plan-hash after rollback" "" "$NS_HASH_AFTER"

# -----------------------------------------------------------------------------
# 6b. verify (post-rollback): expect mismatch=2 and exit 4.
# -----------------------------------------------------------------------------
#
# The same plan is now stale: the pods exist but no longer carry
# runtimeClassName=gvisor. The verifier should report mismatch on every
# step and exit 4 (per docs/exit-codes.md). We deliberately omit
# --in-pod-probe here: the workloads are back on runc, so the field
# check alone is enough to drive the mismatch, and skipping the exec
# saves a few seconds in CI.

log "verify (post-rollback): expect mismatch=2 and exit 4"
set +e
"${AGENT[@]}" verify --plan "$WORK_DIR/plan.json" \
  --output json >"$WORK_DIR/verify-after-rollback.json"
VERIFY_RB_EXIT=$?
set -e
assert_eq "verify exit code (post-rollback)" 4 "$VERIFY_RB_EXIT"
assert_eq "verify summary.ok (post-rollback)" 0 \
  "$(jq -r '.spec.summary.ok' "$WORK_DIR/verify-after-rollback.json")"
assert_eq "verify summary.mismatch (post-rollback)" 2 \
  "$(jq -r '.spec.summary.mismatch' "$WORK_DIR/verify-after-rollback.json")"
STATUSES_RB=$(jq -r '.spec.results | map(.status) | sort | unique | join(",")' \
  "$WORK_DIR/verify-after-rollback.json")
assert_eq "verify result statuses (post-rollback)" "mismatch" "$STATUSES_RB"

# -----------------------------------------------------------------------------
# 7. explain smoke: list mode, valid topic, unknown topic.
# -----------------------------------------------------------------------------
#
# Cluster-independent (the explainer reads embedded docs, never the API
# server). Cheap, so we run it at the end alongside the cluster checks.

log "explain (no topic): expect topic list on stdout"
EXPLAIN_LIST=$("${AGENT[@]}" explain)
if ! printf '%s' "$EXPLAIN_LIST" | grep -q 'runtimeclass'; then
  fail "explain (list) stdout missing 'runtimeclass':\n$EXPLAIN_LIST"
fi

log "explain runtimeclass: expect markdown content"
EXPLAIN_RTC=$("${AGENT[@]}" explain runtimeclass)
if [[ "${EXPLAIN_RTC:0:2}" != "# " ]]; then
  fail "explain runtimeclass should start with '# ': got '${EXPLAIN_RTC:0:40}'"
fi
if (( ${#EXPLAIN_RTC} <= 200 )); then
  fail "explain runtimeclass content too short (${#EXPLAIN_RTC} bytes)"
fi

log "explain bogus-topic: expect non-zero exit, valid topics in stderr"
set +e
EXPLAIN_BOGUS=$("${AGENT[@]}" explain bogus-topic 2>&1)
EXPLAIN_BOGUS_EXIT=$?
set -e
if (( EXPLAIN_BOGUS_EXIT == 0 )); then
  fail "explain bogus-topic should exit non-zero (got 0); output: $EXPLAIN_BOGUS"
fi
for want in runtimeclass gvisor threat-model performance compatibility; do
  if ! printf '%s' "$EXPLAIN_BOGUS" | grep -q "$want"; then
    fail "explain bogus-topic stderr missing topic '$want':\n$EXPLAIN_BOGUS"
  fi
done

# -----------------------------------------------------------------------------
# 8. explain namespace / workload: deep per-workload explanation.
# -----------------------------------------------------------------------------
#
# Like scan, these subcommands hit the live API server (read-only). We
# run them post-rollback so the cluster state is back to the pre-apply
# baseline (host-net still incompatible, web + cache compatible), and we
# assert on structured JSON so the assertions are stable.

log "explain namespace: expect exit 2 (host-net is incompatible) and structured findings"
set +e
"${AGENT[@]}" explain namespace "$NAMESPACE" --output json \
  >"$WORK_DIR/explain-ns.json"
EXPLAIN_NS_EXIT=$?
set -e
assert_eq "explain namespace exit code" 2 "$EXPLAIN_NS_EXIT"

EXPLAIN_KIND=$(jq -r '.kind' "$WORK_DIR/explain-ns.json")
assert_eq "explain namespace kind" "ExplainDocument" "$EXPLAIN_KIND"

NS_NAME=$(jq -r '.spec.namespace.name' "$WORK_DIR/explain-ns.json")
assert_eq "explain namespace name" "$NAMESPACE" "$NS_NAME"

NS_TOTAL=$(jq -r '.spec.namespace.summary.total' "$WORK_DIR/explain-ns.json")
NS_COMPAT=$(jq -r '.spec.namespace.summary.compatible' "$WORK_DIR/explain-ns.json")
NS_REVIEW=$(jq -r '.spec.namespace.summary.needsReview' "$WORK_DIR/explain-ns.json")
NS_INCOMPAT=$(jq -r '.spec.namespace.summary.incompatible' "$WORK_DIR/explain-ns.json")
# The namespace carries one workload per classifier rule plus the original
# web / cache / host-net trio (see test/e2e/manifests/workloads.yaml). Keep
# the bucket counts in sync with that fixture: 2 compatible (web, cache),
# 5 review-required, 7 incompatible.
assert_eq "explain namespace summary.total" 14 "$NS_TOTAL"
assert_eq "explain namespace summary.compatible" 2 "$NS_COMPAT"
assert_eq "explain namespace summary.needsReview" 5 "$NS_REVIEW"
assert_eq "explain namespace summary.incompatible" 7 "$NS_INCOMPAT"

# Every workload in the namespace must appear in the deep document.
NS_NAMES=$(jq -r '.spec.namespace.workloads | map(.name) | sort | join(",")' \
  "$WORK_DIR/explain-ns.json")
assert_eq "explain namespace workload names" \
  "cache,ebpf-app,fuse-app,gpu-app,host-ipc-app,host-net,host-pid-app,hostpath-app,iouring-app,kvm-app,perf-app,priv-app,raw-socket-app,web" \
  "$NS_NAMES"

# host-net is incompatible because of host-network; assert the rule fired
# with the expected evidence in the structured output.
HN_RULES=$(jq -r '.spec.namespace.workloads[] | select(.name=="host-net") | .findings | map(.ruleId) | sort | join(",")' \
  "$WORK_DIR/explain-ns.json")
if ! printf '%s' "$HN_RULES" | grep -q 'host-network'; then
  fail "explain namespace host-net findings missing 'host-network': got '$HN_RULES'"
fi

HN_EVIDENCE=$(jq -r '.spec.namespace.workloads[] | select(.name=="host-net") | .findings[] | select(.ruleId=="host-network") | .evidence.hostNamespaces | join(",")' \
  "$WORK_DIR/explain-ns.json")
assert_eq "explain namespace host-net evidence.hostNamespaces" "hostNetwork" "$HN_EVIDENCE"

# Prose must be embedded (non-empty whyMarkdown for the firing rule).
HN_PROSE_LEN=$(jq -r '.spec.namespace.workloads[] | select(.name=="host-net") | .findings[] | select(.ruleId=="host-network") | .whyMarkdown | length' \
  "$WORK_DIR/explain-ns.json")
if (( HN_PROSE_LEN < 100 )); then
  fail "explain namespace host-net whyMarkdown too short ($HN_PROSE_LEN bytes; expected embedded prose)"
fi

# Compatible workloads must carry the full Checked roster so JSON
# consumers can see what was inspected even when nothing fired.
WEB_CHECKED=$(jq -r '.spec.namespace.workloads[] | select(.name=="web") | .checked | length' \
  "$WORK_DIR/explain-ns.json")
if (( WEB_CHECKED < 10 )); then
  fail "explain namespace web checked too short ($WEB_CHECKED entries; expected the full rule sweep)"
fi

log "explain workload: drill into host-net specifically"
set +e
"${AGENT[@]}" explain workload "$NAMESPACE/host-net" --output json \
  >"$WORK_DIR/explain-wl.json"
EXPLAIN_WL_EXIT=$?
set -e
assert_eq "explain workload exit code" 2 "$EXPLAIN_WL_EXIT"

WL_COUNT=$(jq -r '.spec.namespace.workloads | length' "$WORK_DIR/explain-wl.json")
assert_eq "explain workload result count" 1 "$WL_COUNT"
WL_NAME=$(jq -r '.spec.namespace.workloads[0].name' "$WORK_DIR/explain-wl.json")
assert_eq "explain workload name" "host-net" "$WL_NAME"

# Bad workload reference must fail with a clear error.
log "explain workload (bad ref): expect non-zero exit, helpful error"
set +e
EXPLAIN_BAD=$("${AGENT[@]}" explain workload bogus-ref 2>&1)
EXPLAIN_BAD_EXIT=$?
set -e
if (( EXPLAIN_BAD_EXIT == 0 )); then
  fail "explain workload bogus-ref should exit non-zero (got 0); output: $EXPLAIN_BAD"
fi
if ! printf '%s' "$EXPLAIN_BAD" | grep -q '<namespace>/<name>'; then
  fail "explain workload bogus-ref error missing format hint:\n$EXPLAIN_BAD"
fi

# -----------------------------------------------------------------------------
# Done. Cleanup runs from the EXIT trap.
# -----------------------------------------------------------------------------

log "all e2e assertions passed"
