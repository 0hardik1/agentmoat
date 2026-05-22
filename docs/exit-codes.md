# Exit codes

Every agentmoat command returns a deterministic exit code so CI scripts
and AI agents can branch on it.

| Code | Meaning | Emitted by |
| --- | --- | --- |
| `0` | Success. The command completed and (for mutating commands) the cluster matches the requested state. | All commands |
| `1` | Generic error. Could not load kubeconfig, network failure, unparseable flag, etc. The stderr message names the cause. | All commands |
| `2` | Compatibility issues found. The scan completed but at least one workload was classified as `incompatible`. | `agentmoat scan` |
| `3` | Partial apply. Some workloads were patched, others were not, and the namespace annotation reflects the partial state. Re-run `apply` is safe (idempotent). | `agentmoat apply` (Phase 2) |
| `4` | Verify failed. Pods were patched but the post-migration probe (kubelet runtime field, in-pod `/proc/cmdline` check) did not return gVisor-shaped values. | `agentmoat verify` (Phase 3) |

## Why these exact codes

- `0` and `1` are the universal "success" and "generic error" conventions.
- `2` is reserved (per `man 2`) for misuse in many CLIs, but here we
  repurpose it for "completed successfully but the answer is bad news"
  because that pattern is widely understood (think `grep` returning 1
  on no matches).
- `3` and `4` are application-specific. They never collide with shell
  conventions (126 for "found but not executable", 127 for "not found").

## CI patterns

```bash
# Fail the pipeline only on real errors, not on "found incompatible workloads".
agentmoat scan --output json > scan.json || true
test $? -lt 2 && echo "scan completed (with or without findings)"

# Conversely: fail loudly if anything is incompatible.
if ! agentmoat scan; then
  case $? in
    1) echo "scan errored out, bailing"; exit 1 ;;
    2) echo "incompatible workloads found, blocking merge"; exit 2 ;;
  esac
fi
```
