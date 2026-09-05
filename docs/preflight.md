# Preflight

`agentmoat preflight` answers one question before anything is mutated: can a
pod that requests the target RuntimeClass actually start on this cluster?

The per-workload verdicts from `agentmoat scan` say whether a workload can run
under gVisor. Nothing there can notice that the cluster has no gVisor node,
that the RuntimeClass steers pods nowhere, or that every node is an EKS Auto
Mode instance on which `runsc` can never be installed. Without this check
`apply` succeeds (the API server accepts the patch) and the pods sit Pending
or, worse, run on `runc` while their spec says gVisor.

```
agentmoat preflight                      # RuntimeClass "gvisor"
agentmoat preflight --runtime-class foo  # another RuntimeClass
agentmoat preflight --output json        # PreflightReport document
```

Exit codes: `0` ready, `5` not ready (at least one error finding), `1` the
cluster could not be read. See [`exit-codes.md`](exit-codes.md).

## How placement works, in one paragraph

A RuntimeClass may carry `scheduling.nodeSelector` and
`scheduling.tolerations`. When a pod requests that RuntimeClass, the
RuntimeClass admission controller merges the nodeSelector into the pod's own
nodeSelector (intersection; a conflict rejects the pod) and unions the
tolerations into the pod's tolerations. agentmoat relies on that merge:
`apply` patches only `runtimeClassName`, and the preflight checks that the
RuntimeClass and the nodes make the merge land somewhere real. The shipped
[`deploy/runtimeclass.yaml`](../deploy/runtimeclass.yaml) selects
`runtime: gvisor` and tolerates `runtime=gvisor:NoSchedule`.

## What it reads

Two cluster-scoped, read-only calls:

| Read | Used for |
| --- | --- |
| `get runtimeclasses/<name>` (`node.k8s.io`) | handler, `scheduling.nodeSelector`, `scheduling.tolerations`, `overhead.podFixed` |
| `list nodes` | Ready condition, labels (selector match, EKS Auto Mode, Karpenter), taints, `nodeInfo.osImage` (Bottlerocket), GPU Feature Discovery labels and `nvidia.com/*` capacity (GPU facts) |

Both are in [`deploy/clusterrole-readonly.yaml`](../deploy/clusterrole-readonly.yaml)
and [`deploy/clusterrole-apply.yaml`](../deploy/clusterrole-apply.yaml).
Nothing is written.

## Findings

IDs are stable identifiers: scripts may branch on them, and the JSON output
carries them under `spec.findings[].id`. Only `error` findings block.

| ID | Severity | Meaning | Fix |
| --- | --- | --- | --- |
| `runtimeclass-missing` | error | No RuntimeClass with that name. Pods requesting it are rejected at admission. | `kubectl apply -f deploy/runtimeclass.yaml`, or pass `--runtime-class`. |
| `runtimeclass-no-node-selector` | error | The RuntimeClass has no `scheduling.nodeSelector`. Nothing steers pods to runsc nodes; a pod can run on a runc node with a spec that says gVisor. | Add `scheduling.nodeSelector` and label the gVisor nodes to match. |
| `runtimeclass-no-matching-nodes` | error | The selector matches no node. | Label the nodes that ship runsc, or add a node group built from the agentmoat AMI. |
| `runtimeclass-no-ready-matching-nodes` | error | Nodes match but none is Ready. | Wait for or repair the matching nodes. |
| `runtimeclass-taint-without-toleration` | error when all matching nodes are affected, warn when some | Matching nodes carry a `NoSchedule`/`NoExecute` taint the RuntimeClass does not tolerate. | Add the taint to `scheduling.tolerations` (preferred), or re-plan with `--add-toleration` when the taint is exactly `runtime=gvisor:NoSchedule`. |
| `eks-auto-mode-nodes` | error when all candidates, warn when some | Candidate nodes are EKS Auto Mode managed instances (`eks.amazonaws.com/compute-type=auto`). AWS owns their Bottlerocket image and runtime; runsc cannot be installed. | Add a self-managed or Karpenter node group from the agentmoat AL2023 AMI, labeled to match the selector. See [`eks-deployment.md`](eks-deployment.md). |
| `bottlerocket-nodes` | error when all candidates, warn when some | Candidate nodes run Bottlerocket outside Auto Mode. It ships no runsc and its root filesystem is immutable. | Use the agentmoat AL2023 AMI for the gVisor node group. |
| `runtimeclass-no-overhead` | info | `overhead.podFixed` is unset, so the scheduler does not account for the Sentry's footprint. | Consider `overhead.podFixed` (the shipped manifest uses `memory: 140Mi`, `cpu: 250m`). |
| `gpu-product-unsupported` | warn | GPU nodes carry a card gVisor `nvproxy` does not support (supported: T4, A100, A10G, L4, H100). | Keep those workloads on runc, or add gVisor GPU nodes with a supported card. |
| `gpu-product-unknown` | info | GPU nodes carry no GPU Feature Discovery labels, so the card and driver cannot be checked. | Install GFD (NVIDIA GPU Operator, or `gfd.enabled=true` in the device plugin chart). |
| `gpu-mig-enabled` | warn | GPU nodes slice their GPUs with MIG; `nvproxy` does not support MIG. | Use whole-GPU nodes (`mig.strategy=none`) for workloads that move to gVisor. |
| `gpu-driver-unsupported` | warn | The probed `runsc` does not list the host driver on nodes whose card is supported. | Install a listed driver version, or move to a `runsc` release that lists yours. |
| `gpu-driver-unconfirmed` | info | Supported card, but the driver has not been checked against `runsc`. | Run `agentmoat probe nvproxy --dry-run=false`. |
| `gpu-nvproxy-ready` | info | Card and driver both supported by the probed `runsc`. | None. |
| `nvproxy-probe-dry-run` | info | `probe nvproxy` described its pod and created nothing. | Re-run with `--dry-run=false`. |
| `nvproxy-probe-skipped` | warn | The preflight has an error finding, so there was no node to run the probe pod on. | Fix the error findings first. |
| `nvproxy-probe-failed` | warn | The probe pod did not complete or its output did not parse. | The message names the cause (image pull, timeout, `runsc` path). |

GPU and probe findings never block. They refine the `gpu-passthrough`
verdict and appear as plan warnings; see [`gpu-nvproxy.md`](gpu-nvproxy.md).

"Candidate nodes" are the nodes matching the RuntimeClass nodeSelector when
there are any, else every node in the cluster (so a RuntimeClass-less Auto
Mode cluster still gets the loud message). Findings are sorted error, warn,
info, then by ID, so the same cluster state renders identically.

## Where the check runs

- **`agentmoat preflight`**: on demand, day zero, in CI.
- **`agentmoat apply`**: before the first step, in dry-run too. An error
  finding blocks the apply: the result has every step `skipped`,
  `metadata.preflight.ready: false`, the findings under
  `spec.preflightFindings`, and the CLI exits `5`. Nothing is mutated.
  `--skip-preflight` bypasses the gate. `rollback` never runs it: moving pods
  back to runc needs no gVisor node.
- **`agentmoat probe nvproxy`**: runs the preflight, then (unless
  `--dry-run`, the default) creates one pod on a matching node to read the
  `runsc` nvproxy driver list, and returns the same PreflightReport with
  `metadata.probe` and `spec.facts.gpu.nvproxy` filled in.
- **`agentmoat scan`**: records the same facts under
  `metadata.clusterFacts` (best-effort: without node RBAC the scan still
  succeeds and omits them; `--no-cluster-facts` skips the reads;
  `--facts <file>` loads them from a saved PreflightReport or ScanReport
  instead). The classifier reads them too: `gpu-passthrough` is refined
  from the GPU facts.
- **`agentmoat plan`**: turns the scan's facts into `spec.warnings` (error
  and warn findings, plus `cluster-facts-runtime-class-mismatch` when the
  scan inspected a different RuntimeClass than the plan targets). Warnings do
  not change the steps or the plan hash.
- **`agentmoat verify`**: checks each step's hosting nodes against the
  RuntimeClass nodeSelector and reports `nodePlacement` per result. A node
  outside the selector demotes the step to `mismatch`. Without node RBAC the
  result says `checked: false` and the step keeps its spec-level status.
- **MCP**: `preflight_cluster` returns the same PreflightReport; `apply_plan`
  runs the gate and accepts `skip_preflight`; `probe_nvproxy` is the probe;
  `scan_cluster`, `assess_workload`, and `propose_plan` accept `facts_path`.

## Example

```
$ agentmoat preflight
agentmoat preflight
runtime-class: gvisor   cluster: arn:aws:eks:us-east-1:111122223333:cluster/prod
SUMMARY  1 findings   ✗ blocked
  ✗ error  ▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒▒   1 (100%)

FACTS
  RuntimeClass "gvisor": handler=gvisor  nodeSelector=runtime=gvisor  tolerations=1  overhead=cpu=250m,memory=140Mi
  Nodes: total=6  ready=6  matching-selector=0  matching-and-ready=0  tainted-without-toleration=0
  Platform: eks-auto-mode=6 (matching 0)  bottlerocket=0 (matching 0)  karpenter=6

FINDINGS
  ✗ error  eks-auto-mode-nodes
           6 of 6 node(s) in the cluster are EKS Auto Mode managed instances (eks.amazonaws.com/compute-type=auto); ...
           fix: add a self-managed or Karpenter node group built from the agentmoat AL2023 AMI (packer/), ...
```

The same cluster, as JSON (trimmed):

```json
{
  "apiVersion": "agentmoat.io/v1alpha1",
  "kind": "PreflightReport",
  "metadata": {"runtimeClassName": "gvisor", "agentmoatVersion": "0.2.0"},
  "spec": {
    "summary": {"ready": false, "total": 1, "error": 1, "warn": 0, "info": 0},
    "facts": {
      "runtimeClass": {"name": "gvisor", "found": true, "handler": "gvisor", "nodeSelector": {"runtime": "gvisor"}, "tolerations": 1},
      "nodes": {"total": 6, "ready": 6, "matchingSelector": 0, "matchingAndReady": 0, "matchingTaintedWithoutToleration": 0},
      "platform": {"eksAutoModeNodes": 6, "eksAutoModeMatchingNodes": 0, "bottlerocketNodes": 0, "bottlerocketMatchingNodes": 0, "karpenterNodes": 6}
    },
    "findings": [{"id": "eks-auto-mode-nodes", "severity": "error", "message": "...", "remediation": "..."}]
  }
}
```

## CI pattern

```bash
# Gate a deploy job on the cluster being able to host the migration.
rc=0
agentmoat preflight --output json > preflight.json || rc=$?
case "$rc" in
  0) echo "cluster ready" ;;
  5) jq -r '.spec.findings[] | select(.severity=="error") | "\(.id): \(.remediation)"' preflight.json; exit 5 ;;
  *) echo "preflight could not read the cluster ($rc)"; exit "$rc" ;;
esac
```

## What it cannot tell you

Kubernetes cannot tell agentmoat whether the nodes' containerd actually
registers the handler the RuntimeClass names, or whether the `runsc` binary
on those nodes works. `agentmoat verify --in-pod-probe` is the runtime check
for that: it execs into a migrated pod and looks for gVisor's markers.

Nor can it tell which NVIDIA driver versions that `runsc` can proxy: the
list is compiled into the binary. `agentmoat probe nvproxy` reads it by
running the binary on a node; see [`gpu-nvproxy.md`](gpu-nvproxy.md).
