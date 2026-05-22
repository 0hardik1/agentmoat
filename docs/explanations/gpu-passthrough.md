## Why this warrants review

The container requests `resources.requests["nvidia.com/gpu"]`. gVisor
supports NVIDIA GPUs through `nvproxy`, a Sentry subsystem that proxies
the NVIDIA driver `ioctl` surface from inside the sandbox out to the
real driver on the host. The good news is that it works: CUDA workloads
run inside gVisor with near-native performance once `nvproxy` is
configured. The bad news is that `nvproxy` only supports a subset of
driver and CUDA versions, and the support matrix changes per gVisor
release.

The mismatch surface is "what driver is on the host" against "which
`nvproxy` version ships with the `runsc` binary you have installed".
A mismatch usually fails at first GPU `ioctl`, not at pod startup, so
the failure can look like an application-level bug rather than a
compatibility issue.

## What might break

- CUDA workloads against a driver version `nvproxy` has not been
  taught about will fail with `CUDA_ERROR_UNKNOWN` or
  `CUDA_ERROR_NOT_INITIALIZED` at the first kernel launch.
- New CUDA runtime features (recent NVML calls, MIG slicing, MPS) may
  not be in the proxy yet even when the underlying driver supports them.
- Multi-GPU workloads that use NVLink or peer-to-peer DMA across GPUs
  can hit unsupported `ioctl`s.
- Some profiling tools (NSight, `nvprof`) issue privileged driver
  calls that `nvproxy` rejects.
- Software that touches `/dev/nvidia-uvm-tools` or
  `/dev/nvidia-modeset` may not see the device node.

## How to validate

1. Pin the host's NVIDIA driver version and the `runsc` version, then
   read the [nvproxy support table](https://gvisor.dev/docs/user_guide/gpu/)
   for that combination.
2. Run a minimal CUDA smoke test (`vectorAdd` from the CUDA samples) in
   a gVisor pod on the target node. If this fails, the migration is
   blocked at the driver layer; deeper testing is moot.
3. Run the actual workload's quickest CUDA path (model warmup, single
   inference) on one gVisor pod and compare to the `runc` baseline.
4. Run `agentmoat verify --in-pod-probe` to confirm the pod is on
   `runsc` and not silently fell back to `runc`.
5. Benchmark with the production traffic shape: GPU compute overhead
   is small, but the host-sandbox boundary tax shows up on workloads
   that do many short kernel launches.

See also: [gVisor GPU guide](https://gvisor.dev/docs/user_guide/gpu/)
