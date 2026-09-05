# Exit codes

Every agentmoat command returns a deterministic exit code so CI scripts
and AI agents can branch on it.

| Code | Meaning | Emitted by |
| --- | --- | --- |
| `0` | Success. The command completed and (for mutating commands) the cluster matches the requested state. | All commands |
| `1` | Generic error. Could not load kubeconfig, network failure, unparseable flag, etc. For `apply`/`rollback`, also emitted when **every** step failed. The stderr message names the cause. | All commands |
| `2` | Compatibility issues found. At least one workload was classified as `incompatible`. | `agentmoat scan`, `agentmoat explain namespace`, `agentmoat explain workload` |
| `3` | Partial outcome. Some steps succeeded and others failed; idempotent re-run is safe. | `agentmoat apply`, `agentmoat rollback` |
| `4` | Verify failed. Live pods do not match the plan's expected `runtimeClassName`, the hosting nodes fall outside the RuntimeClass `nodeSelector`, and/or (with `--in-pod-probe`) the in-container probe did not find gVisor markers. | `agentmoat verify` |
| `5` | Cluster not ready. The preflight found an error: no RuntimeClass, a RuntimeClass with no `scheduling.nodeSelector`, no Ready node matching it, untolerated taints on every matching node, or only EKS Auto Mode / Bottlerocket nodes. For `apply`, every step is reported `skipped` and nothing was mutated. For `probe nvproxy`, the probe pod was not created. | `agentmoat preflight`, `agentmoat probe nvproxy`, `agentmoat apply` |

## Verify exit code 4 in detail

By default, `agentmoat verify` compares each plan step's live pods'
`spec.runtimeClassName` against what the migration plan requested. Exit `4`
means at least one step reported `mismatch` or `error`.

With `--in-pod-probe`, the verifier also execs into a running pod and
checks `dmesg`, `/proc/cmdline`, and `uname` output for a gVisor marker.
A pod whose spec says `gvisor` but whose kernel surface does not look
gVisor-shaped is demoted to `mismatch` even when the API field is correct.

## Preflight exit code 5 in detail

`agentmoat preflight` reads one RuntimeClass and the node list and evaluates
whether a pod requesting that RuntimeClass can schedule at all. Exit `5`
means at least one finding has `severity: error`; warnings and info never
change the exit code. The finding IDs are listed in
[`preflight.md`](preflight.md).

`agentmoat apply` runs the same check before its first step, in dry-run too.
When it fails, the ApplyResult carries `metadata.preflight.ready: false`,
every step has `status: skipped`, and `spec.preflightFindings` explains why.
Nothing was mutated, so there is no partial state to roll back.
`--skip-preflight` bypasses the gate. `rollback` never runs it.

`agentmoat probe nvproxy` runs the preflight first and exits `5` for the
same reason: with no Ready node that can host the RuntimeClass there is
nowhere to run the probe pod, and the report says so with
`nvproxy-probe-skipped`. A probe pod that ran but failed (image pull,
timeout, no `runsc` at the path) is a warning finding
(`nvproxy-probe-failed`) and exit `0`, because the preflight itself passed.
GPU findings are never errors, so they never produce exit `5`.

## Why these exact codes

- `0` and `1` are the universal "success" and "generic error" conventions.
- `2` conventionally signals "misuse of shell builtins" in Bash, but many
  CLIs repurpose it. Here it means "completed successfully but the answer
  is bad news", a widely understood pattern (think `grep` returning 1 on
  no matches).
- `3`, `4`, and `5` are application-specific. They never collide with
  shell conventions (126 for "found but not executable", 127 for "not
  found"). `5` is distinct from `3` on purpose: a blocked apply mutated
  nothing, so "idempotent re-run is safe" understates it; there is nothing
  to re-run until the cluster changes.

## CI patterns

```bash
# Fail the pipeline only on real errors, not on "found incompatible workloads".
# Capture the exit code before anything else can overwrite $?.
rc=0
agentmoat scan --output json > scan.json || rc=$?
if [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ]; then
  echo "scan completed (with or without findings)"
else
  echo "scan errored out ($rc)"; exit "$rc"
fi

# Conversely: fail loudly if anything is incompatible. (Don't test $?
# inside an `if ! cmd` branch: the `!` negation rewrites $? to 0.)
rc=0
agentmoat scan || rc=$?
case "$rc" in
  0) echo "all workloads compatible" ;;
  2) echo "incompatible workloads found, blocking merge"; exit 2 ;;
  *) echo "scan errored out, bailing"; exit "$rc" ;;
esac

# After apply, confirm spec and (optionally) runtime inside the pod.
agentmoat verify --plan plan.json --in-pod-probe || test $? -eq 4

# Before planning at all: can this cluster host gVisor pods? Exit 5 is
# "not yet", with the finding ids in the JSON.
agentmoat preflight --output json > preflight.json || test $? -eq 5
jq -r '.spec.findings[] | select(.severity=="error") | .id' preflight.json
```
