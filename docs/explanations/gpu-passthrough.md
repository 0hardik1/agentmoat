## Why this warrants review

The container requests an NVIDIA GPU: an `nvidia.com/*` resource in
`resources.requests` or `resources.limits`, usually `nvidia.com/gpu` in
limits (the scheduler treats the limit as the request). gVisor supports
NVIDIA GPUs through `nvproxy`, a Sentry subsystem that proxies the NVIDIA
driver `ioctl` surface from inside the sandbox out to the real driver on the
host. CUDA workloads run inside gVisor with near-native performance once
`nvproxy` is configured. The support surface is narrow, though, and the
workload spec does not show whether the cluster is inside it:

- `nvproxy` supports the T4, A100, A10G, L4, and H100 cards. Other cards
  (V100, A10, L40S, consumer GeForce) are not supported.
- Each `runsc` release ships an explicit list of host driver versions it can
  proxy. The host driver must match one of them exactly. A mismatch fails at
  the first GPU `ioctl`, not at pod startup, so it can look like an
  application bug.
- Multi-Instance GPU (MIG) is not supported. A workload that requests a MIG
  slice (`nvidia.com/mig-*`) is classified `incompatible` outright.

agentmoat refines this verdict from cluster facts. `scan` reads the GPU
Feature Discovery labels on the GPU nodes (card model, driver version, MIG
strategy), and `agentmoat probe nvproxy` reads the supported-driver list
from the `runsc` binary on a gVisor node. When every eligible GPU node has a
supported card and a listed driver, the finding drops to `info` and the
workload is `compatible`. When the cards are unsupported, MIG-sliced, or the
driver is not listed, it rises to `error`. The "Cluster facts:" sentence
appended to this finding says which case applies. See
[`gpu-nvproxy.md`](../gpu-nvproxy.md).

## What might break

- CUDA workloads on a driver version `nvproxy` has not been taught about
  fail with `CUDA_ERROR_UNKNOWN` or `CUDA_ERROR_NOT_INITIALIZED` at the
  first kernel launch.
- New CUDA runtime features (recent NVML calls, MPS) may not be in the
  proxy yet even when the underlying driver supports them.
- Multi-GPU workloads that use NVLink or peer-to-peer DMA across GPUs can
  hit unsupported `ioctl`s.
- Some profiling tools (NSight, `nvprof`) issue privileged driver calls
  that `nvproxy` rejects.
- Software that touches `/dev/nvidia-uvm-tools` or `/dev/nvidia-modeset`
  may not see the device node.

## How to validate

1. Run `agentmoat probe nvproxy --dry-run=false --output json > probe.json`
   to read the installed `runsc` version and its supported driver list, then
   `agentmoat scan --facts probe.json`. A `compatible` verdict means the card
   and the driver on the gVisor GPU nodes are both on the list.
2. Cross-check with the [nvproxy support table](https://gvisor.dev/docs/user_guide/gpu/)
   for your `runsc` release.
3. Run a minimal CUDA smoke test (`vectorAdd` from the CUDA samples) in a
   gVisor pod on the target node. If this fails, the migration is blocked at
   the driver layer; deeper testing is moot.
4. Run the actual workload's quickest CUDA path (model warmup, single
   inference) on one gVisor pod and compare to the `runc` baseline.
5. Run `agentmoat verify --in-pod-probe` to confirm the pod is on `runsc`
   and did not silently fall back to `runc`.
6. Benchmark with the production traffic shape: GPU compute overhead is
   small, but the host-sandbox boundary tax shows up on workloads that do
   many short kernel launches.

See also: [gVisor GPU guide](https://gvisor.dev/docs/user_guide/gpu/)
