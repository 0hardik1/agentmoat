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
| `ebpf` | Image hint (cilium, tetragon, falco) or `CAP_BPF` capability | gVisor does not expose the eBPF syscall surface. |
| `kvm-nested` | HostPath mount of `/dev/kvm` | gVisor cannot pass through KVM; nested virt is also unavailable on EKS. |

## Workloads needing review ("review")

| Rule ID | What it detects | Why it warrants review |
| --- | --- | --- |
| `host-path-mount` | Any HostPath volume | Gofer must proxy every read/write; some mount semantics not preserved. |
| `gpu-passthrough` | Any `nvidia.com/*` resource (`nvidia.com/gpu`, `nvidia.com/gpu.shared`, `nvidia.com/mig-*`) in container requests or limits | gVisor's `nvproxy` supports T4, A100, A10G, L4, and H100 cards, needs an exact host-driver version match with the installed `runsc`, and does not support MIG. With cluster facts the verdict is refined: supported card and listed driver becomes `info` (compatible); unsupported card, MIG, or unlisted driver becomes `error`. A `nvidia.com/mig-*` request is always `error`. See [`gpu-nvproxy.md`](gpu-nvproxy.md). |
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

## Detection notes

These behaviors are implemented in `pkg/classifier/builtin_rules.go` but easy
to miss when reading the table above:

- **Init containers** are scanned alongside main containers for capabilities,
  privileged mode, GPU resources, and FUSE env vars.
- **`/dev/kvm`** triggers both `kvm-nested` (error) and `host-path-mount`
  (warn) because it is a hostPath volume with that exact path.
- **`kvm-nested`** matches only `hostPath.path == "/dev/kvm"` exactly; variants
  like `/dev/kvm0` are not flagged by that rule (but may still hit
  `host-path-mount`).
- **`io-uring`** is annotation-only (`agentmoat.io/uses-iouring=true`); there
  is no static analysis of binaries or libraries.
- **Image-hint rules** (`ebpf`, `network-throughput`, `syscall-heavy`) are
  substring heuristics on container image refs, not exhaustive detection.
- **`CAP_SYS_ADMIN`** is handled by the `perf-events` rule, not `ebpf`.
  Legacy eBPF loaders that rely on `CAP_SYS_ADMIN` without `CAP_BPF` may
  surface as `review` via `perf-events` rather than `incompatible` via `ebpf`.
  Note that `CAP_SYS_ADMIN` is a near-root capability that gates far more
  than perf events; if your environment treats it as disqualifying, promote
  `perf-events` to `error` via a `--rules` override.

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
