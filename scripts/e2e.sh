#!/usr/bin/env bash
# agentmoat: end-to-end smoke test against a real kind cluster.
#
# What this exercises
#
#   1. `agentmoat scan`    against the kind API server (exit 2 path, JSON
#                          output, summary counts, per-workload kinds).
#   2. `agentmoat plan`    over the stored ScanReport (deterministic
#                          ordering, two steps, PlanHash present).
#   3. `agentmoat apply`   in dry-run mode (default), then again with
#                          --dry-run=false (real strategic-merge patch),
#                          then a third time to prove idempotency.
#   4. `agentmoat rollback` to clear the patches and restore the
#                          pre-apply spec.
#
# What this DOES exercise that earlier revisions did not
#
#   - Real gVisor execution (runsc). The kind worker is built from
#     kind/Dockerfile.gvisor-node and ships runsc + the containerd v2
#     shim. The RuntimeClass uses handler=gvisor. After apply we exec
#     into a patched pod and assert dmesg / /proc/version show gVisor
#     markers, confirming the workload really moved to runsc and not
#     just to a runc that's renamed "gvisor".
#
# What this DOES NOT exercise
#
#   - `agentmoat verify` / `agentmoat explain`. Phase 3 work; pkg/verifier
#     and pkg/explainer are not implemented yet. The e2e probes the
#     runtime directly via `kubectl exec` instead of going through the
#     not-yet-built verifier.
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

# The classifier groups by controller, so the host-net Pod is reported
# as a Pod (no owner). Deployment and StatefulSet show as their
# controller kinds.
SCAN_TOTAL=$(jq -r '.spec.summary.total' "$WORK_DIR/scan.json")
SCAN_COMPAT=$(jq -r '.spec.summary.compatible' "$WORK_DIR/scan.json")
SCAN_INCOMPAT=$(jq -r '.spec.summary.incompatible' "$WORK_DIR/scan.json")
assert_eq "scan summary.total" 3 "$SCAN_TOTAL"
assert_eq "scan summary.compatible" 2 "$SCAN_COMPAT"
assert_eq "scan summary.incompatible" 1 "$SCAN_INCOMPAT"

# Cross-check that the right workloads landed in the right buckets.
KINDS=$(jq -r '.spec.workloads | map(.kind) | sort | join(",")' "$WORK_DIR/scan.json")
assert_eq "scan workload kinds" "Deployment,Pod,StatefulSet" "$KINDS"

INCOMPAT_NAME=$(jq -r '.spec.workloads[] | select(.compatibility=="incompatible") | .name' "$WORK_DIR/scan.json")
assert_eq "scan incompatible workload" "host-net" "$INCOMPAT_NAME"

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
assert_eq "plan summary.total" 3 "$PLAN_TOTAL"
assert_eq "plan summary.included" 2 "$PLAN_INCLUDED"
assert_eq "plan summary.excluded" 1 "$PLAN_EXCLUDED"
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
# 4b. gVisor probe: confirm patched pods are really running under runsc.
# -----------------------------------------------------------------------------
#
# The verifier package (Phase 3) is not built yet, so we probe the
# runtime directly. Two checks per workload:
#
#   1. Scheduling: the pod's .spec.nodeName must be the kind worker, the
#      only node labelled `runtime=gvisor`. If RuntimeClass admission or
#      the nodeSelector silently broke, the pod would land on the
#      control-plane (or stay Pending) and this catches that.
#
#   2. Runtime signature: gVisor's Sentry emulates a Linux kernel and
#      announces itself in dmesg (and sometimes /proc/version, depending
#      on the release). A real runc execution shows the host kernel
#      banner there. We grep case-insensitively for "gvisor"; one hit
#      in either source is enough.
#
# We probe both web (Deployment) and cache (StatefulSet) because they
# took different patch paths (strategic-merge on .spec.template.spec vs
# the StatefulSet equivalent) and we want to catch a regression in
# either path.

probe_gvisor() {
  local kind="$1" name="$2" pod_label="$3"
  local pod
  pod=$("${KCTL[@]}" -n "$NAMESPACE" get pod -l "$pod_label" \
    --field-selector=status.phase=Running \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [[ -z "$pod" ]]; then
    fail "gVisor probe: no Running pod found for $kind/$name (selector $pod_label)"
  fi

  local node
  node=$("${KCTL[@]}" -n "$NAMESPACE" get pod "$pod" \
    -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
  case "$node" in
    *worker*) ;;
    *) fail "gVisor probe: $kind/$name pod '$pod' landed on '$node', expected a *worker* node" ;;
  esac

  # Combine sources so a release that drops one marker still passes.
  # `dmesg` may fail with EPERM if a future kindest base tightens caps;
  # we tolerate that and let /proc/version carry the check.
  local probe_out
  probe_out=$("${KCTL[@]}" -n "$NAMESPACE" exec "$pod" -- sh -c '
    echo "=== dmesg (head) ==="
    dmesg 2>&1 | head -40 || true
    echo "=== /proc/version ==="
    cat /proc/version 2>&1 || true
    echo "=== uname -a ==="
    uname -a 2>&1 || true
  ' 2>&1 || true)

  printf '%s\n' "$probe_out"

  if ! printf '%s' "$probe_out" | grep -qi 'gvisor'; then
    fail "gVisor probe: $kind/$name pod '$pod' on node '$node' shows no gVisor markers (see output above)"
  fi
  printf 'gVisor probe: %s/%s on node %s confirmed running under runsc.\n' \
    "$kind" "$name" "$node"
}

log "gVisor probe: confirm web pod is really running under runsc"
probe_gvisor Deployment web "app=web"

log "gVisor probe: confirm cache pod is really running under runsc"
probe_gvisor StatefulSet cache "app=cache"

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

NS_HASH_AFTER=$("${KCTL[@]}" get namespace "$NAMESPACE" \
  -o jsonpath='{.metadata.annotations.agentmoat\.io/plan-hash}')
assert_eq "namespace plan-hash after rollback" "" "$NS_HASH_AFTER"

# -----------------------------------------------------------------------------
# Done. Cleanup runs from the EXIT trap.
# -----------------------------------------------------------------------------

log "all e2e assertions passed"
