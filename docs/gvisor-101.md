# gVisor 101

gVisor is a user-space application kernel that runs as a sandbox between
your containerised workload and the host's Linux kernel. agentmoat uses
gVisor (via `runsc`) as the substitute runtime for selected workloads.

## Components

- **Sentry.** A user-space application kernel written in Go that
  implements roughly 70% of the Linux syscall surface. The workload's
  syscalls never reach the host kernel directly; Sentry intercepts and
  re-implements them.
- **Gofer.** A proxy process for filesystem operations. The sandbox cannot
  open host files itself; every read or write is brokered through Gofer.
- **runsc.** The OCI runtime binary that starts Sentry + Gofer for each
  container.
- **containerd-shim-runsc-v1.** The containerd shim that bridges
  containerd to `runsc`.

## Platforms (how Sentry intercepts syscalls)

| Platform | Notes |
| --- | --- |
| **`systrap`** | Default since 2023. Uses seccomp `SIGSYS` traps. The only viable choice on virtualized infrastructure (most cloud VMs). |
| **KVM** | Bare-metal only, fastest. Requires nested virt, which is unavailable on AWS EC2. |
| **ptrace** | Deprecated; do not use. |

### On EKS specifically

Nested virtualization is disabled on EC2 instances, so KVM is not an
option. agentmoat's Packer template (`packer/files/runsc.toml`) pins
`platform = "systrap"` explicitly.

## Performance trade-offs

Sourced from gVisor's [official performance guide](https://gvisor.dev/docs/architecture_guide/performance/).
The classifier penalizes workloads in the bottom rows when ordering the
migration plan.

| Workload class | Typical overhead |
| --- | --- |
| CPU-bound (TensorFlow, batch compute) | <5% |
| Disk I/O bound (transcoding) | ~10% |
| Network throughput | 20-40% |
| Syscall-heavy (Redis) | 5-10x latency in pathological cases |
| Static file serving (Apache) | 40-60% |

The agentmoat classifier emits an info-severity finding for workloads
detected as network-throughput-bound (nginx, envoy, haproxy, traefik) or
syscall-heavy (redis, memcached) so operators can budget accordingly.

## See also

- gVisor architecture overview: https://gvisor.dev/docs/architecture_guide/intro/
- Platforms guide: https://gvisor.dev/docs/architecture_guide/platforms/
- Performance guide: https://gvisor.dev/docs/architecture_guide/performance/
- Compatibility matrix: https://gvisor.dev/docs/user_guide/compatibility/
