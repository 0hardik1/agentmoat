# GPU workloads and nvproxy

gVisor runs CUDA workloads through `nvproxy`, a Sentry subsystem that
forwards the NVIDIA driver `ioctl` surface from inside the sandbox to the
real driver on the host. It works, and it works with near-native
performance, but only for a specific set of hardware and driver versions.
This page explains what agentmoat checks, how the `gpu-passthrough` verdict
is decided, and how to feed the real driver list into a scan.

## What nvproxy supports

Three facts decide whether a GPU workload can move to gVisor. None of them
is visible in the workload spec.

| Fact | Rule | Source |
| --- | --- | --- |
| Card model | nvproxy supports **T4, A100, A10G, L4, and H100**. Other cards (V100, A10, L40S, consumer GeForce) are unsupported. | [gVisor GPU guide](https://gvisor.dev/docs/user_guide/gpu/) |
| Host driver version | Each `runsc` release ships an explicit list of NVIDIA driver versions it can proxy. The host driver must match one of them exactly; a mismatch fails at the first GPU `ioctl`, not at pod start. | `runsc nvproxy list-supported-drivers` on the node |
| MIG | Multi-Instance GPU slicing is not supported. A node with `nvidia.com/mig.strategy` other than `none`, or a workload that requests `nvidia.com/mig-*`, cannot use nvproxy. | GFD labels, workload resources |

The card list is compiled into agentmoat (`preflight.NvproxySupportedProducts`)
and revisited with each gVisor pin bump ([`gvisor-version.md`](gvisor-version.md)).
The driver list is not compiled in anywhere but `runsc` itself, which is
why the probe below exists.

## Where the facts come from

**GPU Feature Discovery (GFD)**, part of the NVIDIA GPU Operator and of the
device plugin Helm chart, labels every GPU node:

| Label | Example | Used for |
| --- | --- | --- |
| `nvidia.com/gpu.product` | `Tesla-T4`, `NVIDIA-A10G`, `NVIDIA-H100-80GB-HBM3` | card support (token match: `A10G` matches, `A10` does not) |
| `nvidia.com/cuda.driver-version.full` | `535.183.06` | driver support (older GFD: `cuda.driver.major/minor/rev`) |
| `nvidia.com/mig.strategy` | `none`, `single`, `mixed` | MIG detection |
| `nvidia.com/gpu.count` | `4` | informational |

A node also counts as a GPU node when it advertises an `nvidia.com/*`
extended resource in `status.capacity`, even without GFD; the card is then
"unknown" and the finding `gpu-product-unknown` asks you to install GFD.

`agentmoat preflight` and `agentmoat scan` read these labels into
`spec.facts.gpu` / `metadata.clusterFacts.gpu`: the GPU nodes grouped by
(card, driver, MIG), with a `productSupport` verdict each. `driverSupport`
stays `unknown` until the probe has run.

## The probe

```
agentmoat probe nvproxy                                 # dry run: describe the pod
agentmoat probe nvproxy --dry-run=false --output json > probe.json
```

`probe nvproxy` runs the preflight, then creates one pod pinned to a Ready
node that matches the RuntimeClass `nodeSelector` (preferring a node with
GPUs, since its `runsc` is the one that matters). The pod:

- runs under `runc`, not the RuntimeClass, so the binary it executes is the
  host's `runsc` and not a copy inside a sandbox;
- mounts `/usr/local/bin/runsc` read-only (`hostPath`, type `File`) and
  runs `runsc --version` and `runsc nvproxy list-supported-drivers`;
- runs as an unprivileged user with no capabilities, a read-only root
  filesystem, the `RuntimeDefault` seccomp profile, and no service account
  token;
- is deleted when the probe finishes, success or not.

It does not touch a GPU, load CUDA, or open `/dev/nvidia*`. It answers one
narrow question: does this `runsc` know this driver version?

Like `apply` and `rollback`, the probe defaults to `--dry-run=true`. The dry
run reports the pod it would create (namespace, name, node, image, host
path) under `metadata.probe` and adds the `nvproxy-probe-dry-run` finding.

The output is a `PreflightReport`, the same document `agentmoat preflight`
produces, plus `metadata.probe` and, after a real run,
`spec.facts.gpu.nvproxy` with the `runsc` version and the sorted driver list.
Every GPU node group then carries a settled `driverSupport`.

### Namespace, PSA, and RBAC

The `hostPath` mount is the one thing Pod Security Admission "baseline"
rejects. The default namespace is `default`, which needs no setup on kind or
on a fresh EKS cluster. On clusters that enforce PSA cluster-wide, apply
[`deploy/nvproxy-probe.yaml`](../deploy/nvproxy-probe.yaml) (a namespace
labeled `privileged` plus a namespaced Role) and pass
`--probe-namespace agentmoat-probe`.

The probe needs `create`, `get`, and `delete` on `pods` and `get` on
`pods/log` in that namespace, plus the preflight's `get`/`list` on nodes
and RuntimeClasses. Nothing cluster-wide is written.

`--image` (default `busybox:1.36.1`, only `/bin/sh` is used), `--runsc-path`
(default `/usr/local/bin/runsc`), and `--timeout` (default `2m`) cover
non-standard nodes and air-gapped registries.

## How the verdict is decided

`gpu-passthrough` fires (severity `warn`) for any `nvidia.com/*` resource
request. With cluster facts, the classifier refines it. "Eligible" GPU nodes
are those matching the RuntimeClass `nodeSelector` when any do, else every
GPU node.

| Situation | Severity | Verdict |
| --- | --- | --- |
| Workload requests `nvidia.com/mig-*` | error | incompatible, whatever the cluster has |
| No cluster facts, or no GPU nodes | warn (unchanged) | review |
| Every eligible GPU node: supported card, driver listed by the probed `runsc` | info | compatible |
| Some eligible GPU nodes usable, others not | warn | review; the note says to pin the workload with a `nodeSelector` on `nvidia.com/gpu.product` |
| Supported card, probe not run yet | warn (unchanged) | review; the note says to run the probe |
| Supported card, driver not in the probed list | error | incompatible |
| GPU nodes without GFD labels | warn (unchanged) | review; the note says to install GFD |
| Only unsupported cards or MIG-sliced nodes | error | incompatible |

The refinement appends a "Cluster facts: ..." sentence to the reason's
description, so `scan --output json`, `explain workload`, and
`assess_workload` all show why the severity moved. A `--rules` override of
`gpu-passthrough` sets the base severity the unchanged rows keep; the
rows that raise to `error` or lower to `info` do so regardless. Run with
`--no-cluster-facts` to turn the refinement off.

## Workflow

```
agentmoat probe nvproxy --dry-run=false --output json > probe.json
agentmoat scan --facts probe.json --output json > scan.json
agentmoat plan --scan scan.json --output json > plan.json
```

`--facts` accepts a `PreflightReport` (from `probe nvproxy` or `preflight`)
or a `ScanReport` and replaces the live node reads with the file's facts.
The scan records them under `metadata.clusterFacts`, so the plan built from
that scan carries the same GPU warnings (`gpu-product-unsupported`,
`gpu-driver-unsupported`, `gpu-mig-enabled`) under `spec.warnings`.
`explain namespace` / `explain workload` take `--facts` too.

Over MCP: `probe_nvproxy` (with `"dry_run": false`) returns the report; save
it and pass the path as `facts_path` to `scan_cluster`, `assess_workload`,
or `propose_plan`.

## Findings

| ID | Severity | Meaning |
| --- | --- | --- |
| `gpu-product-unsupported` | warn | GPU nodes carry a card nvproxy does not support. |
| `gpu-product-unknown` | info | GPU nodes carry no GFD labels; install GFD. |
| `gpu-mig-enabled` | warn | GPU nodes slice their GPUs with MIG. |
| `gpu-driver-unsupported` | warn | The probed `runsc` does not list the host driver on nodes whose card is supported. |
| `gpu-driver-unconfirmed` | info | Supported card, driver not probed yet. |
| `gpu-nvproxy-ready` | info | Card and driver both supported by the probed `runsc`. |
| `nvproxy-probe-dry-run` | info | The probe described its pod and created nothing. |
| `nvproxy-probe-skipped` | warn | The preflight has an error finding, so there was no node to run on. |
| `nvproxy-probe-failed` | warn | The pod did not complete or its output did not parse; the message says why (image pull, timeout, `runsc` not at the given path). |

GPU findings never block: the RuntimeClass can still host CPU workloads.
They change the `gpu-passthrough` verdict and appear as plan warnings.

## Limits

- The probe reads one node's `runsc`. A node pool with mixed `runsc`
  versions needs one probe per version (`--runtime-class` selects the pool).
- Labels can be stale: GFD publishes what it saw at startup. Restart GFD
  after a driver upgrade.
- "Supported" means nvproxy can proxy the driver. It does not mean your
  application's CUDA calls are all implemented. The end-to-end check stays a
  CUDA smoke test in a real gVisor pod on the target node; see
  [`explanations/gpu-passthrough.md`](explanations/gpu-passthrough.md).
- The card list mirrors the gVisor documentation at the time of the pin.
  A newer card listed upstream but not here reads as unsupported until the
  list is updated.
