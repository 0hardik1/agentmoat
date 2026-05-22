## What was checked

The classifier ran all 14 built-in rules against this workload and none
fired. The sweep covers host namespace sharing (`hostNetwork`,
`hostPID`, `hostIPC`), capability requests (`CAP_NET_RAW`, `CAP_BPF`,
`CAP_PERFMON`, `CAP_SYS_ADMIN`, `privileged`), volume sources
(`hostPath`, FUSE CSI drivers, `/dev/kvm`), resource requests
(`nvidia.com/gpu`), workload annotations
(`agentmoat.io/uses-iouring`, `agentmoat.io/needs-raw-socket`), and
image-hint heuristics for known network-throughput-bound and
syscall-heavy workloads. No incompatibility, review, or info finding
was emitted. The full rule catalogue is in
[`compatibility-checklist.md`](../compatibility-checklist.md).

## Expected overhead

"Compatible" means the workload will run under `runsc`; it does not
mean zero performance impact. Plan capacity using the published gVisor
performance numbers for the workload's actual class.

- CPU-bound (TensorFlow training, batch compute, language compilers):
  under 5% overhead. These migrate first and are the safest bet.
- Disk I/O bound (transcoders, log shippers): around 10% overhead from
  the Gofer 9P proxy.
- Network throughput bound (a proxy that the classifier did not
  recognise from the image hint): 20-40% throughput drop. The rule
  list is image-hint based and incomplete; do not assume the absence
  of a finding means the absence of overhead.
- Mixed-workload application tiers (typical microservice business
  logic): 5-15% latency increase, manageable in most deployments.
- The Sentry itself adds ~140Mi RSS and ~250m CPU as resident overhead
  per pod. Account for it on small nodes or tight autoscaler bands.

## Next step

The recommended rollout is: set `runtimeClassName: gvisor` on a single
replica, run `agentmoat plan` to see the migration sequenced against
the rest of the namespace, deploy, and verify with `agentmoat verify
--in-pod-probe` that the runtime actually swapped. Measure p95 and p99
latency against the `runc` baseline before expanding to the remaining
replicas. If the workload survives a representative load test, the
plan's idempotency guarantees re-running `apply` to the whole namespace
is safe.

See also: [agentmoat performance guide](../performance.md)
