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

Two cluster-scoped, read-only calls, plus one pod list per sandbox node:

| Read | Used for |
| --- | --- |
| `get runtimeclasses/<name>` (`node.k8s.io`) | handler, `scheduling.nodeSelector`, `scheduling.tolerations`, `overhead.podFixed` |
| `list nodes` | Ready condition, labels (selector match, EKS Auto Mode, Karpenter, EKS Nitro Sandbox), taints, `nodeInfo.osImage` (Bottlerocket), GPU Feature Discovery labels and `nvidia.com/*` capacity (GPU facts) |
| `list pods` with `fieldSelector=spec.nodeName=<node>`, all namespaces | Only for nodes that match the RuntimeClass nodeSelector or carry `eks.amazonaws.com/nitro-sandbox=true`: which pods run sandboxed, which run under `runc`, and which sandboxed pods carry AWS credentials (sandbox facts) |

All three are in [`deploy/clusterrole-readonly.yaml`](../deploy/clusterrole-readonly.yaml)
and [`deploy/clusterrole-apply.yaml`](../deploy/clusterrole-apply.yaml).
Nothing is written. A denied or failed pod list never fails the preflight:
it becomes the `sandbox-colocation-unchecked` info finding.

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
| `sandbox-node-runc-colocation` | warn | Nodes that run sandboxed pods (the target RuntimeClass, or the EKS Nitro Sandbox classes `isolated-sandbox` and `confidential-sandbox`) also run `runc` pods that no DaemonSet owns. A container escape from one of them is node root, and node root can reach the credentials of every pod on the node. | Taint the sandbox nodes and tolerate the taint only from sandboxed pods (the shipped RuntimeClass already tolerates `runtime=gvisor:NoSchedule`), then move the listed pods off. See [`gvisor-vs-nitro-sandboxes.md`](gvisor-vs-nitro-sandboxes.md). |
| `sandbox-pod-aws-credentials` | info | Sandboxed pods get AWS credentials from EKS Pod Identity or IRSA (detected from the environment the EKS webhooks inject). The sandbox does not hide them from the node. agentmoat does not read IAM, so role breadth is not checked. | One least-privilege role per sandboxed workload; restrict who can create pods that name a sandbox RuntimeClass. |
| `sandbox-colocation-unchecked` | info | The pods on the sandbox nodes could not be listed. | Grant `list` on `pods`. |
| `nvproxy-probe-dry-run` | info | `probe nvproxy` described its pod and created nothing. | Re-run with `--dry-run=false`. |
| `nvproxy-probe-skipped` | warn | The preflight has an error finding, so there was no node to run the probe pod on. | Fix the error findings first. |
| `nvproxy-probe-failed` | warn | The probe pod did not complete or its output did not parse. | The message names the cause (image pull, timeout, `runsc` path). |
| `seccomp-blocked-syscalls` | warn | During its trial, the workload's own seccomp profile refused one or more syscalls inside the sandbox. One finding per workload; the message lists each syscall, `denied` or `killed`, and how often. | Check whether the workload needs those calls. If it does, allow them in a `Localhost` profile. See [Seccomp probe](#seccomp-probe). |
| `seccomp-filter-not-loaded` | warn | The workload asks for `RuntimeDefault` or `Localhost`, but the Sentry installed no filter during its trial, so the trial says nothing about the profile. | Check that the audit handler's config on that node sets `oci-seccomp = "true"`. |
| `seccomp-runsc-before-default` | warn | The trial ran on a `runsc` older than `release-20261123.1`, the first release that loads the OCI seccomp filter by default. | Upgrade `runsc` on the gVisor nodes ([`gvisor-version.md`](gvisor-version.md)) and re-run the probe. |
| `seccomp-unconfined` | info | A probed workload has a container whose seccompProfile is `Unconfined`, so no filter covers it in the sandbox. | See [`explanations/seccomp-unconfined.md`](explanations/seccomp-unconfined.md). |
| `seccomp-probe-clean` | info | Every trial ran and no filter refused a syscall. It covers only what the workloads did during the trial. | None. |
| `seccomp-probe-dry-run` | info | `probe seccomp` described its trial and reader pods and created nothing. | Re-run with `--dry-run=false`. |
| `seccomp-probe-failed` | warn | A trial did not start, or its Sentry log could not be read. One finding per workload. | The message names the cause (scheduling, image pull, no log on the node). |
| `seccomp-probe-skipped` | warn | The probe ran no trial for a workload (or for any): the preflight has an error finding, the audit RuntimeClass is missing, or the workload could not be read. | The message names the cause. For the RuntimeClass: `kubectl apply -f deploy/runtimeclass-seccomp-audit.yaml`. |

GPU, probe, seccomp and sandbox findings never block. The sandbox facts (under
`spec.facts.sandbox`) and findings appear only once some pod runs sandboxed,
so a cluster that has not migrated yet reports exactly what it did before.
GPU findings also refine the `gpu-passthrough` verdict (see
[`gpu-nvproxy.md`](gpu-nvproxy.md)), and warn findings of every kind appear
as plan warnings.

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
  `runsc` version and nvproxy driver list (and a second pod when that `runsc`
  needs its sidecar binaries, see [`gpu-nvproxy.md`](gpu-nvproxy.md)), and
  returns the same
  PreflightReport with `metadata.probe`, `spec.facts.gpu.nvproxy`, and
  `spec.facts.runsc` filled in.
- **`agentmoat probe seccomp`**: runs the preflight, then (unless
  `--dry-run`, the default) runs each migrated workload once on the
  `gvisor-seccomp-audit` RuntimeClass and reports the syscalls its own
  seccomp profile refused, under `metadata.seccompProbe` and
  `spec.facts.seccomp`. See [Seccomp probe](#seccomp-probe).
- **`agentmoat scan`**: records the same facts under
  `metadata.clusterFacts` (best-effort: without node RBAC the scan still
  succeeds and omits them; `--no-cluster-facts` skips the reads;
  `--facts <file>` loads them from a saved PreflightReport or ScanReport
  instead). The classifier reads them too: `gpu-passthrough` is refined
  from the GPU facts, and `systemd-init` from the probed `runsc` version
  (see [`explanations/systemd-init.md`](explanations/systemd-init.md)).
- **`agentmoat plan`**: turns the scan's facts into `spec.warnings` (error
  and warn findings, plus `cluster-facts-runtime-class-mismatch` when the
  scan inspected a different RuntimeClass than the plan targets). Warnings do
  not change the steps or the plan hash. One more warning is not about the
  cluster: `source-managed`, when a workload in the plan has the marker of a
  deploy tool (see [Plan warnings](#plan-warnings-that-are-not-from-the-preflight)).
- **`agentmoat verify`**: checks each step's hosting nodes against the
  RuntimeClass nodeSelector and reports `nodePlacement` per result. A node
  outside the selector demotes the step to `mismatch`. Without node RBAC the
  result says `checked: false` and the step keeps its spec-level status.
- **MCP**: `preflight_cluster` returns the same PreflightReport; `apply_plan`
  runs the gate and accepts `skip_preflight`; `probe_nvproxy` and
  `probe_seccomp` are the probes;
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

## Seccomp probe

From gVisor release `20261123.1`, `runsc`'s `--oci-seccomp` flag defaults to
true: `runsc` loads the container's OCI seccomp filter inside the sandbox,
and the Sentry applies it to the application's syscalls. Before that release
`runsc` skipped the filter unless the flag was set, so a pod with
`seccompProfile: RuntimeDefault` ran on gVisor with no filter at all. A
workload that worked on an older `runsc` can now get `EPERM` (or be killed)
on a syscall that its profile does not allow, though nothing in its spec
changed. containerd's default profile, for example, allows `unshare`,
`mount` and `bpf` only with `CAP_SYS_ADMIN`.

`agentmoat probe seccomp` finds those syscalls before the move:

```bash
kubectl apply -f deploy/runtimeclass-seccomp-audit.yaml   # once per cluster
agentmoat plan --output json > plan.json
agentmoat probe seccomp --plan plan.json                  # dry run: describes, creates nothing
agentmoat probe seccomp --plan plan.json --dry-run=false --output json > seccomp.json
```

Without `--plan` it probes the workloads in `--namespace` that already
request the RuntimeClass (default `gvisor`).

For each workload it:

1. Creates one trial pod from the workload's pod template, with
   `runtimeClassName: gvisor-seccomp-audit`, `restartPolicy: Never` and a
   deadline. The seccomp profiles, image, command, service account and
   environment are the workload's own. What changes is listed in the report:
   no workload labels (so no Service sends it traffic and no controller
   adopts it), PersistentVolumeClaims and ephemeral volumes become
   `emptyDir`, and a fixed `nodeName`, hostname and ephemeral containers
   are dropped.
2. Lets it run for `--run-for` (default `1m`), then deletes it and waits
   until it is gone.
3. On each node that ran a trial, starts a reader pod (runc, uid 65534, no
   capabilities, read-only root, a read-only `hostPath` of
   `/var/log/runsc/seccomp-audit`) that prints, from each trial's Sentry
   debug log, the `runsc` version, how many filters were installed or
   ignored, and each `Syscall N: denied by seccomp` or `killed by seccomp`
   line. The numbers are mapped to names for the node's architecture.

The handler behind `gvisor-seccomp-audit` is the same `runsc` as `gvisor`,
with `oci-seccomp = "true"` set explicitly (so the trial applies the filter
even on an older `runsc`) and the Sentry's debug log on. The denials are
logged only at debug level, which is why the trials need their own handler.
The agentmoat AMI and the kind node image register it; see
[`kind/runsc-seccomp-audit.toml`](../kind/runsc-seccomp-audit.toml) and
[`packer/files/runsc-seccomp-audit.toml`](../packer/files/runsc-seccomp-audit.toml).

The identity that runs it needs `create`, `get`, `list`, `watch` and
`delete` on pods, and `get` on `pods/log`, in the workloads' namespaces and
in `--reader-namespace` (default `default`, which must allow `hostPath`
under Pod Security admission), on top of the read-only role.

Limits:

- A trial runs the workload's real code. Whatever it does at start (a
  migration, a message to a queue, a call to an API) it does again. Use the
  dry run to see the pods first, and probe in a cluster where that is safe.
- The result covers what the workload did during the trial, with no Service
  traffic. A syscall it makes only under load, or only on a request, is not
  seen.
- Denials are counted per pod, not per container.
- The Sentry debug logs stay on the node under
  `/var/log/runsc/seccomp-audit`; clean them up or rotate them.

To check the change itself on a node group, without a workload, run
`make check-oci-seccomp` (or `scripts/check-oci-seccomp.sh --context <ctx>`):
it runs a `RuntimeDefault` pod and an `Unconfined` control on the `gvisor`
RuntimeClass and checks that only the first one has a filter and is refused
`unshare`. CI runs it, and `make seccomp-probe-smoke`, on the kind cluster.

The `seccomp-unconfined` rule of `scan` flags `Unconfined` profiles, and
`plan` repeats it as a warning; see
[`explanations/seccomp-unconfined.md`](explanations/seccomp-unconfined.md).

## Plan warnings that are not from the preflight

`spec.warnings` of a MigrationPlan has three more IDs. They are here so that
all the warning IDs are in one document.

| ID | When | What to do |
| --- | --- | --- |
| `manifest-source` | The plan was made from `scan -f` (manifests, no cluster). | The plan is a preview. `apply`, `rollback`, and `verify` refuse it. Run `agentmoat plan` against the cluster. |
| `source-managed` | One or more workloads in the plan have the marker of a deploy tool. | Add `runtimeClassName` to the source that the tool deploys from (the git repository, the chart values) too. `agentmoat patch -f <path>` adds it to manifest files. |
| `seccomp-unconfined` | One or more workloads in the plan set `seccompProfile: Unconfined` on the pod or a container. | Nothing blocks. From gVisor `20261123.1` those containers are the only ones with no seccomp filter in the sandbox; switch them to `RuntimeDefault` or a `Localhost` profile unless that is deliberate. See [Seccomp probe](#seccomp-probe). |

### `source-managed`

`apply` patches the live object. When a tool deploys that object from a
source, the source does not get the change. What the tool does with a live
change that is not in its source depends on the tool and on its settings, so
the warning makes no statement about that. It tells you, before the apply,
that there is a second place to change.

For manifest files, `agentmoat patch -f <path>` makes that change: it adds
`runtimeClassName` to the pod spec of each workload that can run in the
sandbox, and prints the diff. For a Helm chart, set the chart value. See
[The move through the repository](ci-cd.md#3-the-move-through-the-repository).

`scan` reads these markers from the labels and annotations of the workload
(not of its pods), and records the first match as `managedBy` (`tool` and
`evidence`) in the report:

| Tool | Marker | Source of the key |
| --- | --- | --- |
| `argocd` | annotation `argocd.argoproj.io/tracking-id` | The default resource tracking method from Argo CD 3.0, and the `annotation` and `annotation+label` methods before that. |
| `argocd` | label `argocd.argoproj.io/instance` | The custom instance label in the Argo CD documents. |
| `flux` | label `kustomize.toolkit.fluxcd.io/name` | Written by the kustomize-controller on each object that it applies. |
| `flux` | label `helm.toolkit.fluxcd.io/name` | Written by the helm-controller on each object of a release. |
| `helm` | annotation `meta.helm.sh/release-name` | Written by Helm on install and upgrade. |

The order is Argo CD, Flux, Helm. An object that Flux installed from a chart
has the Flux marker and the Helm marker; Flux owns the source, so the report
names Flux.

The label `app.kubernetes.io/instance` is not a marker here. It is the default
tracking label of Argo CD 2.x, but charts and operators write it too, so it is
not evidence of Argo CD. An Argo CD 2.x install with its default settings
leaves no marker that `scan` reads.

No marker is not proof of no owner. A pipeline that runs `kubectl apply`
leaves no marker, and each tool can use another key. The warning says so.
The warning is not in the plan hash, and a plan from manifests does not get
it (a preview changes no live object).
