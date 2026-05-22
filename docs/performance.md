# gVisor performance trade-offs

When agentmoat recommends gVisor for a workload, the cost is paid in
syscall latency and (for some workloads) throughput. This page summarises
where gVisor is cheap, where it is expensive, and what you can do to
measure the impact on your own workload before migrating.

## Overhead by workload class

The classifier's `Overhead` field is sourced from gVisor's published
[performance guide](https://gvisor.dev/docs/architecture_guide/performance/).
The numbers below are typical, not worst case.

| Workload class | Typical overhead |
| --- | --- |
| CPU-bound (TensorFlow, batch compute) | <5% |
| Disk I/O bound (transcoding) | ~10% |
| Network throughput (high-bandwidth) | 20-40% |
| Syscall-heavy (Redis, in-memory caches) | 5-10x latency in pathological cases |
| Static file serving (Apache, nginx) | 40-60% |

The planner uses these numbers (via the `RiskScore` field on each
`PlanStep`) to order the migration lowest-risk first: CPU-bound batch jobs
move early, latency-sensitive caches move last.

## Why the costs exist

The overhead comes from two design choices that also give gVisor its
security guarantees.

**Sentry intercepts every syscall.** Sentry is a user-space application
kernel written in Go that re-implements roughly 70% of the Linux syscall
surface. Each syscall the workload issues is trapped (via `systrap`,
which uses seccomp `SIGSYS`) and handed to Sentry to execute. The
trap-and-emulate path is fast for CPU-bound work that rarely syscalls,
but it dominates wall time for syscall-heavy workloads like Redis. See
[gVisor 101](gvisor-101.md) for the Sentry architecture in detail.

**Gofer proxies every filesystem operation.** The sandbox cannot open
host files directly; the Gofer process handles every `open`, `read`,
`write`, `stat`, and friends. This is what makes disk I/O bound
workloads (transcoders, log shippers) measurably slower under gVisor.

**Sentry's user-space netstack is the main throughput tax.** For network
throughput bound workloads (nginx, envoy, haproxy, traefik), the
classifier emits an info-severity finding so operators see the cost up
front. `scheduling.overhead.podFixed` on the RuntimeClass tells the
Kubernetes scheduler about Sentry's resident overhead (~140Mi memory,
~250m CPU by default) so capacity planning stays honest.

## Measuring impact on your own workload

The numbers above are a starting point, not a verdict. To know what the
overhead is for your workload:

1. Roll one replica into a staging cluster running on gVisor nodes.
2. Compare p50, p95, and p99 latency before and after, ideally with a
   replay of production traffic shape.
3. Watch CPU and memory: Sentry's resident overhead adds to the pod's
   total footprint and may push you across an autoscaler threshold.
4. Run `agentmoat verify --in-pod-probe` to confirm the runtime swap
   actually happened. A workload still on `runc` is not a meaningful
   benchmark of gVisor.

If the overhead is unacceptable, the classifier's `review` verdict gives
you a clean signal to keep the workload on `runc`: the migration plan
will already have excluded it by default.

## Related

- [gVisor 101](gvisor-101.md)
- [RuntimeClass 101](runtimeclass-101.md)
- [Compatibility checklist](compatibility-checklist.md)
