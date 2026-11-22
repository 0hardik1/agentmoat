# Exit codes

Every agentmoat command returns a deterministic exit code so CI scripts
and AI agents can branch on it.

| Code | Meaning | Emitted by |
| --- | --- | --- |
| `0` | Success. The command completed and (for mutating commands) the cluster matches the requested state. | All commands |
| `1` | Generic error. Could not load kubeconfig, network failure, unparseable flag, etc. For `apply`/`rollback`, also emitted when **every** step failed. For `scan -f`, a manifest that does not parse, or `--fail-on sandboxed` on input that is not rendered. The stderr message names the cause. | All commands |
| `2` | Compatibility issues found. At least one workload was classified as `incompatible`. For `scan`, `--fail-on` changes which workloads count (see below). | `agentmoat scan`, `agentmoat explain namespace`, `agentmoat explain workload` |
| `3` | Partial outcome. Some steps succeeded and others failed; idempotent re-run is safe. | `agentmoat apply`, `agentmoat rollback` |
| `4` | Verify failed. Live pods do not match the plan's expected `runtimeClassName`, the hosting nodes fall outside the RuntimeClass `nodeSelector`, and/or (with `--in-pod-probe`) the in-container probe did not find gVisor markers. | `agentmoat verify` |
| `5` | Cluster not ready. The preflight found an error: no RuntimeClass, a RuntimeClass with no `scheduling.nodeSelector`, no Ready node matching it, untolerated taints on every matching node, or only EKS Auto Mode / Bottlerocket nodes. For `apply`, every step is reported `skipped` and nothing was mutated. For `probe nvproxy`, the probe pod was not created. | `agentmoat preflight`, `agentmoat probe nvproxy`, `agentmoat apply` |

## Scan exit code 2 and `--fail-on`

`agentmoat scan` exits `2` when its gate fails. `--fail-on` selects the gate.
The default is the behavior scan always had.

| `--fail-on` | Exit `2` when | Use |
| --- | --- | --- |
| `incompatible` (default) | A workload is `incompatible`. | A terminal, an inventory, existing scripts. |
| `sandboxed` | A workload that asks for the gVisor RuntimeClass (`runtimeClassName` equal to `--runtime-class`, default `gvisor`) is `incompatible`. | A pull-request check. It does not fail on a CNI DaemonSet that will never move. It stops a change that breaks a sandboxed workload, and a change that puts an incompatible workload in the sandbox. |
| `none` | Never. | A job that only publishes the report. |

With `sandboxed`, scan prints the reason on stderr: the workloads that failed
(with file and line for a manifest scan), or a note when no workload asks for
the RuntimeClass. That note matters. If your sandbox RuntimeClass has another
name, the gate has nothing to check until you pass `--runtime-class`.

`review` never fails a gate. Only `incompatible` does.

### Manifest scans (`scan -f`) and exit code 1

`scan -f` reads manifests and no cluster. Three results are errors (exit `1`,
nothing useful on stdout), because exit `0` must mean "this input was read
and nothing in it failed the gate":

- A document does not parse, or a workload has no name. All problems are
  listed together, with file and line. Text that contains `{{ }}` gets a hint
  to run `helm template` first.
- The input has no Kubernetes object.
- `--fail-on sandboxed` and the input is not rendered: a kustomization file
  is in it, or a partial object (a workload kind with no containers, which is
  the shape of an overlay patch). A file scan reads each object as written
  and applies no overlay. An overlay can add `runtimeClassName`, so the
  answer would not describe what gets deployed. Scan the rendered objects:
  `kustomize build <dir> | agentmoat scan -f - --fail-on sandboxed`. In this
  case the report is still printed (it is correct about the files), and
  `metadata.source.unrendered` lists the signs.

Input with objects but no workloads (config only) is exit `0` with a note.

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

# A pull-request check with no cluster: fail only when a change breaks a
# workload that asks for the gVisor RuntimeClass. Exit 1 is "could not read
# the input", and must fail the job too.
helm template ./chart | agentmoat scan -f - --fail-on sandboxed

# After apply, confirm spec and (optionally) runtime inside the pod.
agentmoat verify --plan plan.json --in-pod-probe || test $? -eq 4

# Before planning at all: can this cluster host gVisor pods? Exit 5 is
# "not yet", with the finding ids in the JSON.
agentmoat preflight --output json > preflight.json || test $? -eq 5
jq -r '.spec.findings[] | select(.severity=="error") | .id' preflight.json
```
