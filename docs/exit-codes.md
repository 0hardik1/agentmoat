# Exit codes

Every agentmoat command returns a deterministic exit code so CI scripts
and AI agents can branch on it.

| Code | Meaning | Emitted by |
| --- | --- | --- |
| `0` | Success. The command completed and (for mutating commands) the cluster matches the requested state. | All commands |
| `1` | Generic error. Could not load kubeconfig, network failure, unparseable flag, etc. For `apply`/`rollback`, also emitted when **every** step failed. The stderr message names the cause. | All commands |
| `2` | Compatibility issues found. At least one workload was classified as `incompatible`. | `agentmoat scan`, `agentmoat explain namespace`, `agentmoat explain workload` |
| `3` | Partial outcome. Some steps succeeded and others failed; idempotent re-run is safe. | `agentmoat apply`, `agentmoat rollback` |
| `4` | Verify failed. Live pods do not match the plan's expected `runtimeClassName`, and/or (with `--in-pod-probe`) the in-container probe did not find gVisor markers. | `agentmoat verify` |

## Verify exit code 4 in detail

By default, `agentmoat verify` compares each plan step's live pods'
`spec.runtimeClassName` against what the migration plan requested. Exit `4`
means at least one step reported `mismatch` or `error`.

With `--in-pod-probe`, the verifier also execs into a running pod and
checks `dmesg`, `/proc/cmdline`, and `uname` output for a gVisor marker.
A pod whose spec says `gvisor` but whose kernel surface does not look
gVisor-shaped is demoted to `mismatch` even when the API field is correct.

## Why these exact codes

- `0` and `1` are the universal "success" and "generic error" conventions.
- `2` conventionally signals "misuse of shell builtins" in Bash, but many
  CLIs repurpose it. Here it means "completed successfully but the answer
  is bad news", a widely understood pattern (think `grep` returning 1 on
  no matches).
- `3` and `4` are application-specific. They never collide with shell
  conventions (126 for "found but not executable", 127 for "not found").

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
```
