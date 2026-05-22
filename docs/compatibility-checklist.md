# gVisor compatibility checklist

These are the categories the agentmoat classifier flags. Each item lists
its rule ID, severity, what gVisor does about it, and a link to the
upstream gVisor docs.

For the live list of rules, see [`internal/rules/gvisor.yaml`](../internal/rules/gvisor.yaml).
For the rule implementations, see [`pkg/classifier/builtin_rules.go`](../pkg/classifier/builtin_rules.go).

## Blocking ("incompatible") issues

| Rule ID | What it detects | Why it blocks |
| --- | --- | --- |
| `raw-socket` | `CAP_NET_RAW` capability or `agentmoat.io/needs-raw-socket=true` | Raw sockets are off by default in gVisor; require runsc `--net-raw`. |
| `host-network` | `pod.spec.hostNetwork=true` | gVisor cannot bridge to the host network namespace. |
| `host-pid` | `pod.spec.hostPID=true` | gVisor isolates the PID namespace. |
| `host-ipc` | `pod.spec.hostIPC=true` | gVisor isolates the IPC namespace. |
| `privileged` | Any container with `securityContext.privileged=true` | The sandbox boundary makes this meaningless; many capabilities unsupported. |
| `ebpf` | Image hint (cilium, tetragon, falco) or BPF/SYS_ADMIN capability | gVisor does not expose the eBPF syscall surface. |
| `kvm-nested` | HostPath mount of `/dev/kvm` | gVisor cannot pass through KVM; nested virt is also unavailable on EKS. |

## Workloads needing review ("review")

| Rule ID | What it detects | Why it warrants review |
| --- | --- | --- |
| `host-path-mount` | Any HostPath volume | Gofer must proxy every read/write; some mount semantics not preserved. |
| `gpu-passthrough` | `resources.requests["nvidia.com/gpu"]` set | gVisor's `nvproxy` supports only a subset of CUDA versions. |
| `fuse-mount` | CSI driver name containing "fuse" or `AGENTMOAT_USES_FUSE=true` | gVisor supports a subset of FUSE behavior. |
| `io-uring` | Annotation `agentmoat.io/uses-iouring=true` | gVisor does not implement `io_uring`. |
| `perf-events` | `CAP_PERFMON` or `CAP_SYS_ADMIN` | gVisor does not expose perf events. |

## Informational ("info") findings

These do not block migration but signal that performance under gVisor
will differ. Use them when ordering the rollout.

| Rule ID | What it detects | Expected impact |
| --- | --- | --- |
| `network-throughput` | Image hints: nginx, envoy, haproxy, traefik | ~20-40% throughput overhead under Sentry's network stack. |
| `syscall-heavy` | Image hints: redis, memcached | Higher latency; benchmark before migration. |

## Overriding severity

Every rule's severity can be dialed down (or up) without recompiling.
Pass `--rules <path>` to `agentmoat scan` with a YAML file that mirrors
`internal/rules/gvisor.yaml`. Unknown IDs are reported as warnings on
stderr but do not fail the run.

## Upstream reference

The canonical compatibility matrix is maintained by the gVisor team at
https://gvisor.dev/docs/user_guide/compatibility/. If you discover a
category not flagged by agentmoat, please file an issue with the rule
proposal.

## Related agentmoat topics

- [gVisor 101](gvisor-101.md): why these particular features are hard
  for a user-space kernel to support.
- [RuntimeClass 101](runtimeclass-101.md): how a flagged workload is
  excluded from the RuntimeClass opt-in.
- [Performance trade-offs](performance.md): the cost agentmoat trades
  away in exchange for the safety boundary.
