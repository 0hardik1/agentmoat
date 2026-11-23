# Exit codes

Every agentmoat command returns a deterministic exit code so CI scripts
and AI agents can branch on it.

| Code | Meaning | Emitted by |
| --- | --- | --- |
| `0` | Success. The command completed and (for mutating commands) the cluster matches the requested state. | All commands |
| `1` | Generic error. Could not load kubeconfig, network failure, unparseable flag, etc. For `apply`/`rollback`, also emitted when **every** step failed. They also refuse a plan with no `metadata.planHash` with this code. For `scan -f`, a manifest that does not parse, or `--fail-on sandboxed` on input that is not rendered. The stderr message names the cause. | All commands |
| `2` | Compatibility issues found. At least one workload was classified as `incompatible`. For `scan`, `--fail-on` changes which workloads count (see below). | `agentmoat scan`, `agentmoat explain namespace`, `agentmoat explain workload` |
| `3` | Partial outcome. Some steps succeeded and others failed; idempotent re-run is safe. After a partial `apply`, `rollback` with the same plan undoes the steps that succeeded (see below). | `agentmoat apply`, `agentmoat rollback` |
| `4` | Verify failed. Live pods do not match the plan's expected `runtimeClassName`, the hosting nodes fall outside the RuntimeClass `nodeSelector`, (with `--in-pod-probe`) the in-container probe did not find gVisor markers, and/or (with `--wait`) the pods did not stay Ready before the wait time ended. | `agentmoat verify` |
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
  `metadata.source.unrendered` lists the signs. With `-o markdown`, the first
  line of that report says that the gate could not answer, so a job summary
  does not read as a pass. See [output formats](output-formats.md).

Input with objects but no workloads (config only) is exit `0` with a note.

## Verify exit code 4 in detail

By default, `agentmoat verify` compares each plan step's live pods'
`spec.runtimeClassName` against what the migration plan requested. Exit `4`
means at least one step reported `mismatch` or `error`.

With `--in-pod-probe`, the verifier also execs into a running pod and
checks `dmesg`, `/proc/cmdline`, and `uname` output for a gVisor marker.
A pod whose spec says `gvisor` but whose kernel surface does not look
gVisor-shaped is demoted to `mismatch` even when the API field is correct.

A workload that has no pods now, and wants none, is checked on its pod
template: a Deployment or StatefulSet at `replicas: 0`, a DaemonSet that
selects no node, a Job that is finished. The result then has
`basis: template`. A CronJob is always checked on its template, because a Job
from before the change keeps its old pods until it ends. A workload that
wants pods and has none is an `error`.

### `verify --wait`

Without `--wait`, verify looks at the cluster one time. Directly after
`apply --dry-run=false` that look finds a rollout in progress (old pods with
no class, or no pods), and the result is exit `4` for a migration that is
correct and not finished. A pipeline cannot act on that.

With `--wait <duration>` (for example `--wait 3m`), verify looks again every
5 seconds until every step is ok or the time ends. Two things change:

- **Pods must be Ready, and stay Ready.** A step is ok only when all its pods
  have the class, are Ready, and stayed Ready for 10 seconds with no restart
  and no replaced pod. One look that finds a pod Ready is not sufficient: a
  container with no readiness probe is Ready from its start until it stops,
  so a pod in a crash loop is Ready for a moment in each restart. The settle
  time is never more than the wait time.
- **Exit `4` means more.** After a wait, exit `4` says that the workloads did
  not come up in gVisor in the time that you gave them. That is the signal
  to roll back.

A step that is not ok after the wait is a `mismatch` (or an `error`, when
the workload could not be read or has no pods). Its `message` and its
`readiness` block say why: the pods that are not Ready, and the reason of the
first one (`container app is waiting: CrashLoopBackOff, 4 restart(s)`, or the
message of the scheduler for a pod that has no node). In a rolling update
that does not go on, the old pods stay, and the message names both: the old
pod that does not have the class, and the new pod that does not start.

The report says how the wait ended, in `metadata.wait`:

| Field | Meaning |
| --- | --- |
| `requestedSeconds` | The `--wait` value. |
| `waitedSeconds` | The time from the first look to the last one. |
| `rounds` | The number of looks. |
| `timedOut` | `true` when the time ended with one or more steps not ok. |
| `interrupted` | `true` when the caller canceled the verify: an MCP client that cancels the tool call, or a program that uses the library and cancels its context. The results are those of the last complete look, and they are not a verdict. The CLI never writes this field (see below). |

The wait time is not a deadline on the API calls. A look that started before
the time ended runs to its end, so the report has the reason for each step
and not `context deadline exceeded`. The command can thus run a few seconds
longer than the wait time.

An interrupt (Ctrl-C, or the SIGTERM of a job that the CI system cancels)
ends `agentmoat verify` by the signal, with no report and no exit code of its
own. This is on purpose. A shell goes on with the right side of
`verify --wait 3m || rollback --dry-run=false` when the left side returns an
exit code, and it stops the full list when the left side dies by the signal.
An interrupt must never start a rollback.

A workload with no pods now (see above) does not use the wait time. With
`--in-pod-probe`, the probe runs one time for each step, when its pods have
settled. Progress goes to stderr (`verify: waiting: 3 of 8 step(s) ok, 2m10s
left`); `-q` removes it.

## `rollback` after each way that `apply` can end

A pipeline runs `apply`, and when a later step fails it runs `rollback` with
the same plan. `rollback` must then do the correct thing for every way that
the `apply` ended, also when it ended in the middle.

### The record on the namespace

`apply` records itself on each namespace that it changes, with two
annotations. The value of each is the hash of the plan
(`metadata.planHash`).

| Annotation | Written | Meaning |
| --- | --- | --- |
| `agentmoat.io/apply-started` | Before the first patch in the namespace. | An apply of this plan started here. |
| `agentmoat.io/plan-hash` | When every step in the namespace is done. `apply-started` is removed in the same write. | This plan is applied here. A second `apply` reports `already-applied`. |

`apply` writes `apply-started` first and patches after. If it cannot write the
annotation (the identity has no `patch` on namespaces), it patches no
workload in that namespace, and each step there fails with `the workload was
not changed`. So a workload is never changed with no record of it in the
cluster.

A plan with no `metadata.planHash` cannot be recorded. `apply` and `rollback`
refuse it with exit `1`, in a dry-run too. `agentmoat plan` always sets the
hash.

### What `rollback` does

`rollback` acts on a namespace only when one of the two annotations has the
hash of its plan:

| `apply` ended with | The cluster then | `rollback` with the same plan |
| --- | --- | --- |
| `0` | Every step is applied. Each namespace has `plan-hash`. | Un-patches every step and removes the annotation. |
| `3` (some steps failed) | A namespace with a step that failed has `apply-started` and no `plan-hash`. Some of its workloads have the RuntimeClass. | In such a namespace: reads each workload of the plan and un-patches the ones that have the RuntimeClass of the plan. Then removes the annotation. |
| Stopped in the middle (the process was killed, the CI system canceled the job) | The same as for `3`, for each namespace that the apply got to. | The same as for `3`. |
| `1` because every step failed | `apply-started` on each namespace where a patch was tried. No workload got the RuntimeClass from this apply. | The same as for `3`. |
| `5` (the preflight gate), or `1` before the first write | Nothing changed. No annotation. | Changes nothing. Every step is `already-applied`, exit `0`. |

The limits of this, in plain words:

- With no annotation of the plan, `rollback` reads no workload and patches
  none. A RuntimeClass that another tool put on a workload stays. `rollback`
  prints a note on stderr for such a namespace, because the result
  (`already-applied`, exit `0`) is also the result for a plan whose record
  another plan replaced (see below).
- `rollback` removes the RuntimeClass of the plan from the workloads of the
  plan. It has no record of each workload, so it cannot tell a workload that
  this apply changed from one that had the same RuntimeClass before the
  apply. The two lose it.
- After an apply that did not finish, a workload of the plan that has
  another RuntimeClass, or none, is left as it is: the apply did not get to
  it.

After a partial `apply` you can also run the same `apply` again: the
namespace has no `plan-hash`, so every step is done again, and a complete run
leaves `plan-hash` only.

### One plan for a namespace at a time

The two annotations hold the hash of one plan. The newest `apply` has the
namespace:

- When an `apply` starts in a namespace that has the annotation of another
  plan, it replaces it (the same write sets `apply-started` and removes the
  old `plan-hash`), and prints a note on stderr with the hash that it
  replaced.
- A `rollback` with a plan that does not have the namespace changes nothing
  there. When the annotations have the hash of another plan, each step is
  `skipped` with a message that names that hash. When no annotation is
  left, each step is `already-applied` and stderr has the note.
- To roll back a plan that lost the namespace: `apply` it again (for
  workloads that have the RuntimeClass, the patch changes nothing and starts
  no rollout), and then `rollback` it.

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

# In a pipeline: apply, wait for the workloads to come up in gVisor, and roll
# back when they do not. Exit 4 from `verify --wait` is "they did not come up
# in 3 minutes"; exit 1 is "verify could not run". In the two cases the
# cluster is not in a known good state, so roll back and fail the job.
# Rollback is idempotent.
agentmoat apply --plan plan.json --dry-run=false
rc=0
agentmoat verify --plan plan.json --wait 3m -o markdown >> "$GITHUB_STEP_SUMMARY" || rc=$?
if [ "$rc" -ne 0 ]; then
  agentmoat rollback --plan plan.json --dry-run=false
  exit "$rc"
fi

# Before planning at all: can this cluster host gVisor pods? Exit 5 is
# "not yet", with the finding ids in the JSON.
agentmoat preflight --output json > preflight.json || test $? -eq 5
jq -r '.spec.findings[] | select(.severity=="error") | .id' preflight.json
```
