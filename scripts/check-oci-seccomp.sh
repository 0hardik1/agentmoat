#!/usr/bin/env bash
# agentmoat: check that a RuntimeDefault pod on gVisor is really filtered by
# its seccomp profile.
#
# Why this exists:
#   From release 20261123.1, runsc's --oci-seccomp flag defaults to true:
#   runsc loads the container's OCI seccomp filter inside the sandbox, so
#   the Sentry enforces it on the application's syscalls. Before that
#   release runsc skipped the filter unless the flag was set, and logged
#   "Seccomp spec is being ignored because oci-seccomp is disabled". A pod
#   with seccompProfile RuntimeDefault then ran with no filter at all, even
#   though its spec said otherwise. A pinned release says what the node
#   should do; this script checks what it does.
#
# What it does:
#   It starts two short pods with the gVisor RuntimeClass, both running the
#   same busybox shell:
#     * oci-seccomp-runtimedefault  seccompProfile RuntimeDefault
#     * oci-seccomp-unconfined      seccompProfile Unconfined (the control)
#   Each pod reports three things:
#     * SANDBOX  whether /proc/cmdline names gVisor, so the pod really ran
#                under runsc and not runc
#     * SECCOMP  the Seccomp field of /proc/self/status (2 = a filter is
#                installed, 0 = none)
#     * UNSHARE  the result of `unshare -U true`. containerd's default
#                profile allows unshare(2) only with CAP_SYS_ADMIN, which a
#                default container lacks, so the filter makes it fail with
#                "Operation not permitted". gVisor itself allows it, which
#                the Unconfined control shows.
#   The check passes when the RuntimeDefault pod has SECCOMP=2 and its
#   unshare is refused, and the control has SECCOMP=0 and its unshare works.
#   The control is what makes a pass mean something: if gVisor refused
#   unshare on its own, the RuntimeDefault result would prove nothing, and
#   the script says so instead of passing.
#
#   Checked on runsc release-20261123.1 with containerd's default profile:
#   RuntimeDefault gives SECCOMP=2 and EPERM; the same pod with
#   --oci-seccomp=false (the old default) gives SECCOMP=0 and unshare works.
#
# Where it runs:
#   Any cluster your kubeconfig reaches: the kind cluster from `make kind-up`
#   (CI runs it there) or an EKS cluster with the gVisor node group from
#   docs/eks-deployment.md. The pods get the RuntimeClass's nodeSelector and
#   tolerations from the RuntimeClass admission controller, so they land on
#   the gVisor nodes. To cover every gVisor node, run it once per node group.
#
# Usage:
#   ./scripts/check-oci-seccomp.sh
#   ./scripts/check-oci-seccomp.sh --context my-eks --runtime-class gvisor
#   ./scripts/check-oci-seccomp.sh --namespace scratch --keep
#
# Options (each also read from the environment variable in brackets):
#   --context NAME        kubeconfig context [KUBE_CONTEXT] (default: current)
#   --runtime-class NAME  RuntimeClass to test [RUNTIME_CLASS] (default: gvisor)
#   --namespace NAME      namespace for the two pods [NAMESPACE]
#                         (default: agentmoat-oci-seccomp-check). A namespace
#                         the script creates is deleted at the end; one that
#                         already existed is left alone. The Unconfined
#                         control is refused where Pod Security admission
#                         enforces baseline or restricted; pick a namespace
#                         that allows it.
#   --image REF           image with busybox `sh`, `awk` and `unshare`
#                         [IMAGE] (default: busybox:1.36.1)
#   --timeout SECONDS     how long to wait for each pod [TIMEOUT] (default: 180)
#   --keep                leave the pods (and namespace) for inspection [KEEP=1]
#
# Needs kubectl. It creates two pods (and maybe a namespace) and deletes them.
#
# Exit codes (script codes, unrelated to the CLI's docs/exit-codes.md):
#   0  RuntimeDefault is enforced inside the sandbox
#   1  RuntimeDefault is NOT enforced: the pod ran with no filter
#   2  inconclusive: the check could not run, a pod did not run on gVisor,
#      the image has no unshare, or the control was refused too

set -euo pipefail

KUBECTL="${KUBECTL:-kubectl}"
KUBE_CONTEXT="${KUBE_CONTEXT:-}"
RUNTIME_CLASS="${RUNTIME_CLASS:-gvisor}"
NAMESPACE="${NAMESPACE:-agentmoat-oci-seccomp-check}"
IMAGE="${IMAGE:-busybox:1.36.1}"
TIMEOUT="${TIMEOUT:-180}"
KEEP="${KEEP:-0}"

usage() { sed -n '/^# Usage:/,/^# Needs kubectl/p' "$0" | sed 's/^# \{0,1\}//'; }

while [[ $# -gt 0 ]]; do
  case "$1" in
    --context) KUBE_CONTEXT="$2"; shift 2 ;;
    --runtime-class) RUNTIME_CLASS="$2"; shift 2 ;;
    --namespace) NAMESPACE="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --timeout) TIMEOUT="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "error: unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

KCTL=("$KUBECTL")
if [[ -n "$KUBE_CONTEXT" ]]; then
  KCTL+=(--context "$KUBE_CONTEXT")
fi

POD_FILTERED=oci-seccomp-runtimedefault
POD_CONTROL=oci-seccomp-unconfined
CREATED_NS=0

inconclusive() { printf 'INCONCLUSIVE: %s\n' "$*" >&2; exit 2; }

cleanup() {
  local code=$?
  if [[ "$KEEP" == "1" ]]; then
    echo "leaving pods in namespace $NAMESPACE (--keep)"
  else
    "${KCTL[@]}" -n "$NAMESPACE" delete pod "$POD_FILTERED" "$POD_CONTROL" \
      --ignore-not-found --wait=false >/dev/null 2>&1 || true
    if [[ "$CREATED_NS" == "1" ]]; then
      "${KCTL[@]}" delete namespace "$NAMESPACE" --wait=false >/dev/null 2>&1 || true
    fi
  fi
  exit "$code"
}
trap cleanup EXIT

command -v "$KUBECTL" >/dev/null 2>&1 || inconclusive "kubectl not found"
"${KCTL[@]}" get runtimeclass "$RUNTIME_CLASS" >/dev/null 2>&1 ||
  inconclusive "RuntimeClass '$RUNTIME_CLASS' not found (kubectl apply -f deploy/runtimeclass.yaml)"

if ! "${KCTL[@]}" get namespace "$NAMESPACE" >/dev/null 2>&1; then
  "${KCTL[@]}" create namespace "$NAMESPACE" >/dev/null
  CREATED_NS=1
fi

# The shell each pod runs. It always exits 0 and reports through stdout, so
# the pod's phase says only whether it ran. busybox's own applet is called
# (`busybox unshare`), so a PATH without the applet link still works, and
# `busybox --list` tells a missing applet from a refused syscall.
read -r -d '' CHECK <<'EOF' || true
case "$(cat /proc/cmdline 2>/dev/null)" in
  *[Gg][Vv][Ii][Ss][Oo][Rr]*) echo "SANDBOX=gvisor" ;;
  *) echo "SANDBOX=other" ;;
esac
echo "SECCOMP=$(awk '/^Seccomp:/ {print $2}' /proc/self/status)"
if ! busybox --list 2>/dev/null | grep -qx unshare; then
  echo "UNSHARE=missing"
elif out=$(busybox unshare -U true 2>&1); then
  echo "UNSHARE=ok"
else
  echo "UNSHARE=fail $out"
fi
echo "END"
EOF

# pod_manifest NAME PROFILE: a one-shot pod with the RuntimeClass and the
# given seccompProfile type. No capabilities are added; the container keeps
# the runtime's default set, which is what RuntimeDefault is written for.
pod_manifest() {
  local name="$1" profile="$2"
  # The check script goes in as one JSON string, so no YAML quoting rules
  # apply to it.
  local args
  args=$(printf '%s' "$CHECK" | awk 'BEGIN{ORS=""; print "\""} {gsub(/\\/,"\\\\"); gsub(/"/,"\\\""); print $0 "\\n"} END{print "\""}')
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $name
  namespace: $NAMESPACE
  labels:
    app.kubernetes.io/name: agentmoat
    app.kubernetes.io/component: oci-seccomp-check
spec:
  runtimeClassName: $RUNTIME_CLASS
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  automountServiceAccountToken: false
  securityContext:
    seccompProfile:
      type: $profile
  containers:
    - name: check
      image: $IMAGE
      command: ["sh", "-c", $args]
EOF
}

for pod in "$POD_FILTERED" "$POD_CONTROL"; do
  "${KCTL[@]}" -n "$NAMESPACE" delete pod "$pod" --ignore-not-found --wait=true >/dev/null 2>&1 || true
done

echo "starting $POD_FILTERED (RuntimeDefault) and $POD_CONTROL (Unconfined) with RuntimeClass $RUNTIME_CLASS in namespace $NAMESPACE"
pod_manifest "$POD_FILTERED" RuntimeDefault | "${KCTL[@]}" apply -f - >/dev/null ||
  inconclusive "could not create pod $POD_FILTERED"
if ! pod_manifest "$POD_CONTROL" Unconfined | "${KCTL[@]}" apply -f - >/dev/null; then
  inconclusive "could not create the Unconfined control pod; Pod Security admission may refuse Unconfined in namespace $NAMESPACE (use --namespace)"
fi

# wait_done POD: poll until the pod has finished (Succeeded or Failed).
wait_done() {
  local pod="$1" deadline=$((SECONDS + TIMEOUT)) phase=""
  while (( SECONDS < deadline )); do
    phase=$("${KCTL[@]}" -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "$phase" in
      Succeeded|Failed) return 0 ;;
    esac
    sleep 2
  done
  "${KCTL[@]}" -n "$NAMESPACE" describe pod "$pod" >&2 || true
  inconclusive "pod $pod did not finish within ${TIMEOUT}s (phase: ${phase:-unknown})"
}

# field POD KEY: the value after KEY= in the pod's log.
field() {
  printf '%s\n' "$2" | awk -v k="$1=" 'index($0, k) == 1 { print substr($0, length(k) + 1); exit }'
}

report() {
  local pod="$1"
  wait_done "$pod"
  local log node
  log=$("${KCTL[@]}" -n "$NAMESPACE" logs "$pod" 2>/dev/null || true)
  node=$("${KCTL[@]}" -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.spec.nodeName}' 2>/dev/null || true)
  if ! printf '%s\n' "$log" | grep -qx END; then
    printf '%s\n' "$log" >&2
    inconclusive "pod $pod did not finish its check (no END line in its log)"
  fi
  printf '%s\n' "$log" | sed "s/^/  $pod: /" | grep -v ': END$'
  printf '  %s: node=%s\n' "$pod" "${node:-unknown}"
  LOG_OUT="$log"
}

report "$POD_FILTERED"; FILTERED_LOG="$LOG_OUT"
report "$POD_CONTROL"; CONTROL_LOG="$LOG_OUT"

for pair in "$POD_FILTERED:$FILTERED_LOG" "$POD_CONTROL:$CONTROL_LOG"; do
  pod="${pair%%:*}" log="${pair#*:}"
  [[ "$(field SANDBOX "$log")" == "gvisor" ]] ||
    inconclusive "pod $pod did not run under gVisor (/proc/cmdline has no gVisor marker); check RuntimeClass $RUNTIME_CLASS"
  [[ "$(field UNSHARE "$log")" != "missing" ]] ||
    inconclusive "image $IMAGE has no busybox unshare applet; pass --image with one that has it"
done

c_seccomp=$(field SECCOMP "$CONTROL_LOG")
c_unshare=$(field UNSHARE "$CONTROL_LOG")
if [[ "$c_unshare" != "ok" ]]; then
  inconclusive "the Unconfined control could not unshare either ($c_unshare, Seccomp $c_seccomp), so a refusal in the RuntimeDefault pod would not show the filter"
fi
if [[ "$c_seccomp" != "0" ]]; then
  inconclusive "the Unconfined control reports Seccomp $c_seccomp (expected 0); something else installs a filter"
fi

f_seccomp=$(field SECCOMP "$FILTERED_LOG")
f_unshare=$(field UNSHARE "$FILTERED_LOG")
if [[ "$f_seccomp" == "2" && "$f_unshare" == fail* && "$f_unshare" == *"Operation not permitted"* ]]; then
  echo "PASS: RuntimeDefault is enforced inside the gVisor sandbox (Seccomp 2, unshare refused with EPERM; the Unconfined control can unshare)"
  exit 0
fi

echo "FAIL: the RuntimeDefault pod was not filtered (Seccomp ${f_seccomp:-unknown}, unshare: ${f_unshare:-unknown})." >&2
echo "      runsc on that node is probably older than release-20261123.1, or runs with --oci-seccomp=false." >&2
exit 1
