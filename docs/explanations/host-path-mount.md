## Why this warrants review

The pod mounts a `hostPath` volume. Under gVisor the workload cannot
open host files directly: every `open`, `read`, `write`, `stat`,
`getdents`, and friends is brokered through the Gofer process over the
9P protocol. Gofer is the only component on the host that touches the
underlying filesystem on behalf of the sandbox. This works well for
read-mostly bind mounts (config, certificates, ConfigMap projections)
and is the path that most workloads end up exercising.

It is the edge cases that warrant review. Some mount semantics are not
fully preserved across the Gofer boundary: filesystem notifications,
advisory locks, mount propagation, and a few extended attributes have
historically been the rough edges. The set of supported semantics has
improved release-to-release, so the answer is "test, do not assume".

## What might break

- `inotify` and `fanotify` may miss events or fire late. Tools that
  tail rotated logs (filebeat, fluent-bit) sometimes need a
  configuration tweak.
- `fcntl(F_SETLK)` advisory locks across the host/sandbox boundary
  behave inconsistently. Databases that use lockfiles to coordinate
  across pods on the same host can break.
- `mount(2)` propagation flags (`shared`, `slave`, `private`) are not
  fully honoured. Workloads that re-mount inside the container may see
  surprising visibility.
- Throughput on sequential read/write drops noticeably under Gofer's 9P
  proxy (commonly 20-40% versus direct host access).
- `O_DIRECT` opens may be downgraded or refused.

## How to validate

1. Run `agentmoat verify --in-pod-probe` on the workload. The probe
   exercises a short read/write and reports the actual Gofer-mediated
   behaviour seen from inside the sandbox.
2. Mirror the workload to a staging cluster with the same hostPath
   mounts and run for at least one log-rotation cycle.
3. If the workload uses file locks, write a small probe that takes
   `F_SETLK` from two concurrent pods on the same node and verify the
   expected exclusion behaviour.
4. Benchmark the I/O path: `fio` with the workload's actual block size
   gives a comparable number against the `runc` baseline.
5. Watch the kubelet's events for the pod: Gofer issues surface as
   pod-level events in many gVisor builds.

See also: [gVisor filesystem guide](https://gvisor.dev/docs/user_guide/filesystem/)
