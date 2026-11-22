#!/usr/bin/env bash
# agentmoat: check that every gVisor sandbox on a node is confined to a
# device cgroup.
#
# Why this exists:
#   From release 20261116.0, runsc restricts the sandbox's cgroup to an
#   allowlist of host devices ("Confine the sandbox to a device cgroup",
#   google/gvisor PR 16485). The allowlist covers what runsc, the Sentry and
#   the Gofer open on the sandbox's behalf: /dev/null, /dev/ptmx and the
#   pseudo-terminals, overlayfs whiteouts, /dev/cpu_dma_latency, the
#   platform's device (for example /dev/kvm on the KVM platform), the
#   devices listed in the container's OCI spec, and the NVIDIA driver
#   majors when the NVIDIA hook is in use. On cgroup v1 it uses the devices
#   controller; on cgroup v2 a BPF_PROG_TYPE_CGROUP_DEVICE program.
#
#   Before that release the sandbox cgroup allowed every device
#   (`a *:* rwm`), so a process that took over the Sentry could open any
#   host device node it could reach. After it, the host kernel refuses
#   them. This is a second layer under the Sentry, and the reason a sandbox
#   runtime pin is worth keeping current. This script checks that the layer
#   is actually in force on a node, not just that the pinned release has it.
#
# What it does, for every running `runsc-sandbox` process on the node:
#   1. Finds the process's device cgroup (cgroup v1 `devices` hierarchy, or
#      the unified hierarchy on cgroup v2).
#   2. cgroup v1 only: reads devices.list and fails if it still allows all
#      devices (`a *:* rwm`).
#   3. Both versions: moves a short-lived probe shell into that cgroup and
#      opens two devices. /dev/null must open (it is on every sandbox's
#      allowlist). /dev/kmsg must be refused with "Operation not
#      permitted": no ordinary pod declares it, and opening it for reading
#      has no side effect. The probe shell exits right after.
#
#   A sandbox for a privileged pod declares every host device in its OCI
#   spec, so its allowlist legitimately includes /dev/kmsg. agentmoat never
#   migrates privileged workloads (the `privileged` rule is an error), so
#   on a node that only runs agentmoat-migrated pods every sandbox should
#   pass.
#
# Usage:
#   ./scripts/check-device-cgroup.sh                 # on this host (a gVisor node, e.g. over SSM)
#   ./scripts/check-device-cgroup.sh --node <name>   # inside a kind node, through `docker exec`
#   RUNSC=/path/to/runsc ./scripts/check-device-cgroup.sh
#
# Run it as root: it reads other processes' cgroups and writes to
# cgroup.procs. It changes nothing that outlives the probe shell.
#
# Exit codes (script codes, unrelated to the CLI's docs/exit-codes.md):
#   0  every sandbox found is confined
#   1  a sandbox is not confined, or the check itself failed
#   2  skipped: runsc predates the device cgroup, or no sandbox is running

set -euo pipefail

NODE=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --node)
      [[ $# -ge 2 ]] || { echo "error: --node needs a value" >&2; exit 1; }
      NODE="$2"
      shift 2
      ;;
    -h|--help)
      sed -n '2,52p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "error: unknown argument '$1' (try --help)" >&2
      exit 1
      ;;
  esac
done

# The check runs as a POSIX sh program on the node, so it works the same on
# an EKS node (Amazon Linux 2023) and inside a kind node (Debian), neither
# of which is guaranteed to ship bash extras, pgrep -f, or getfattr. It reads
# only /proc and /sys/fs/cgroup.
#
# MIN_DATE is the date part of the first release with the device cgroup.
# Keep it in sync with docs/gvisor-version.md.
read -r -d '' NODE_CHECK <<'SH' || true
set -u
RUNSC="${RUNSC:-/usr/local/bin/runsc}"
MIN_DATE=20261116

if [ ! -x "$RUNSC" ]; then
  echo "error: runsc not found at $RUNSC (set RUNSC)" >&2
  exit 1
fi
version="$("$RUNSC" --version 2>/dev/null | sed -n 's/^runsc version release-\([0-9]\{8\}\.[0-9]*\)$/\1/p' | head -1)"
if [ -z "$version" ]; then
  echo "error: could not read a release-yyyymmdd.N version from $RUNSC --version" >&2
  exit 1
fi
if [ "${version%%.*}" -lt "$MIN_DATE" ]; then
  echo "skip: runsc release-$version predates the sandbox device cgroup (release-$MIN_DATE.0)"
  exit 2
fi

# Sandbox processes re-exec runsc with argv[0] set to "runsc-sandbox".
pids=""
for f in /proc/[0-9]*/cmdline; do
  arg0="$(tr '\000' '\n' <"$f" 2>/dev/null | head -1)" || continue
  if [ "$arg0" = "runsc-sandbox" ]; then
    p="${f#/proc/}"
    pids="$pids ${p%/cmdline}"
  fi
done
if [ -z "$pids" ]; then
  echo "skip: no runsc-sandbox process is running on this node"
  exit 2
fi

failed=0
checked=0
for pid in $pids; do
  [ -r "/proc/$pid/cgroup" ] || continue
  devices_line="$(grep -E '^[0-9]+:([^:]*,)?devices(,[^:]*)?:' "/proc/$pid/cgroup" | head -1)"
  if [ -n "$devices_line" ]; then
    mode=v1
    cg="/sys/fs/cgroup/devices${devices_line##*:}"
  else
    mode=v2
    cg="/sys/fs/cgroup$(grep -E '^0::' "/proc/$pid/cgroup" | head -1 | cut -d: -f3-)"
  fi
  if [ ! -w "$cg/cgroup.procs" ]; then
    echo "error: sandbox $pid: cannot write $cg/cgroup.procs (run as root)" >&2
    failed=1
    continue
  fi
  checked=$((checked + 1))

  if [ "$mode" = v1 ] && grep -q '^a ' "$cg/devices.list"; then
    echo "FAIL sandbox $pid ($mode $cg): devices.list allows every device:"
    sed 's/^/  /' "$cg/devices.list"
    failed=1
    continue
  fi

  # The probe runs in a child shell that joins the sandbox cgroup and
  # exits, so the checking shell itself stays where it was.
  result="$(sh -c '
    echo $$ >"$1/cgroup.procs" || { echo "join-failed"; exit 0; }
    if ( exec 3</dev/null ) 2>/dev/null; then echo "null=ok"; else echo "null=denied"; fi
    err="$( ( exec 3</dev/kmsg ) 2>&1 )" && echo "kmsg=opened" || echo "kmsg=denied:$err"
  ' probe "$cg" 2>&1)"

  case "$result" in
    *join-failed*)
      echo "error: sandbox $pid: could not move the probe into $cg" >&2
      failed=1
      ;;
    *null=denied*)
      echo "FAIL sandbox $pid ($mode $cg): /dev/null is denied, so the allowlist is wrong"
      failed=1
      ;;
    *kmsg=opened*)
      echo "FAIL sandbox $pid ($mode $cg): /dev/kmsg opened, so the device cgroup is not enforced"
      failed=1
      ;;
    *"kmsg=denied"*"not permitted"*)
      echo "ok   sandbox $pid ($mode $cg): /dev/null allowed, /dev/kmsg refused"
      ;;
    *)
      echo "error: sandbox $pid: unexpected probe result: $result" >&2
      failed=1
      ;;
  esac
done

if [ "$checked" -eq 0 ] && [ "$failed" -eq 0 ]; then
  echo "skip: no sandbox cgroup could be read"
  exit 2
fi
if [ "$failed" -ne 0 ]; then
  exit 1
fi
echo "ok   $checked sandbox(es) confined to a device cgroup (runsc release-$version)"
SH

if [[ -n "$NODE" ]]; then
  docker exec -i -e "RUNSC=${RUNSC:-/usr/local/bin/runsc}" "$NODE" sh -s <<<"$NODE_CHECK"
else
  RUNSC="${RUNSC:-/usr/local/bin/runsc}" sh -s <<<"$NODE_CHECK"
fi
