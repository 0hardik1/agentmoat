# RuntimeClass 101

`RuntimeClass` is the Kubernetes mechanism for opting a Pod into a non-default
container runtime. agentmoat uses it to route selected workloads to gVisor's
`runsc` while leaving the rest of the cluster on the default `runc`.

## The objects involved

1. **`RuntimeClass`** is a cluster-scoped resource (`node.k8s.io/v1`) that
   names a container runtime *handler*.
2. A **Pod** opts in by setting `spec.runtimeClassName: gvisor`.
3. The **kubelet** consults the named `RuntimeClass`, extracts its
   `handler` field, and tells **containerd** which CRI runtime to invoke.
4. **containerd** is configured per node (in `/etc/containerd/config.toml`
   or a drop-in under `/etc/containerd/config.d/`) with a runtime entry
   whose **key matches the handler name**. That entry points at the gVisor
   shim (`io.containerd.runsc.v1` → `containerd-shim-runsc-v1` → `runsc`).

### agentmoat vs upstream handler naming

The handler string is arbitrary as long as it matches between the
RuntimeClass and containerd config. agentmoat uses **`gvisor`** everywhere:

| Layer | agentmoat | Upstream gVisor quick start |
| --- | --- | --- |
| RuntimeClass `handler` | `gvisor` | `runsc` |
| containerd runtime key | `gvisor` | `runsc` |
| OCI binary | `runsc` | `runsc` |

See `deploy/runtimeclass.yaml`, `packer/files/gvisor-runtime.toml`, and
`kind/cluster.yaml`. If you copy upstream examples that use `handler: runsc`
while your nodes register `gvisor`, pods will fail with a handler-not-found
error.

## A minimal RuntimeClass

```yaml
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: gvisor
```

The agentmoat-shipped variant adds scheduling fields and overhead
accounting; see `deploy/runtimeclass.yaml`.

## Optional fields you should set in production

| Field | Purpose |
| --- | --- |
| `scheduling.nodeSelector` | Pin gVisor pods to nodes that actually have `runsc` installed (label `runtime: gvisor`). agentmoat treats a missing selector as an error (`agentmoat preflight` finding `runtimeclass-no-node-selector`). |
| `scheduling.tolerations` | Pair with a `runtime=gvisor:NoSchedule` taint on the gVisor nodes so other workloads stay off them. |
| `overhead.podFixed` | Tell the scheduler about Sentry's resident memory and CPU overhead (~140Mi / 250m by default). |

## How the scheduling block reaches the pod

When a pod requests a RuntimeClass, the RuntimeClass admission controller
(enabled by default since Kubernetes 1.16) merges the class's
`scheduling.nodeSelector` into the pod's own `nodeSelector` and unions its
`scheduling.tolerations` into the pod's tolerations. Two consequences:

- The pod spec you write (or agentmoat patches) needs only
  `runtimeClassName`. Placement follows from the RuntimeClass. This is why
  `agentmoat apply` patches nothing else by default; `plan --add-toleration`
  exists for a RuntimeClass that lacks `scheduling.tolerations`.
- If the pod already has a `nodeSelector` that conflicts with the class's,
  admission rejects the pod. A workload pinned to a `runc` node pool by its
  own labels cannot be moved by setting `runtimeClassName` alone.

`agentmoat preflight` reads the RuntimeClass and the nodes and reports
whether that merge can land anywhere: the selector must match at least one
Ready node whose taints the class tolerates, and those nodes must be able to
run `runsc` (not EKS Auto Mode, not Bottlerocket). See
[`preflight.md`](preflight.md). `agentmoat verify` later confirms that the
pods' hosting nodes satisfy the selector.

## When to NOT use RuntimeClass

- For a security boundary, `RuntimeClass` alone is not enough. You still
  need the underlying runtime (gVisor, Kata, etc.) installed on the nodes
  and the containerd handler configured. agentmoat's Packer template
  handles that node-side plumbing.
- For policy enforcement ("agent workloads MUST use gVisor"), pair
  `RuntimeClass` with an admission controller or a Kyverno/OPA policy.
  agentmoat itself does not provide one (Phase 6 stretch).

## See also

- Upstream Kubernetes docs: https://kubernetes.io/docs/concepts/containers/runtime-class/
- agentmoat-shipped manifest: [`deploy/runtimeclass.yaml`](../deploy/runtimeclass.yaml)
- gVisor + containerd quick start: https://gvisor.dev/docs/user_guide/containerd/quick_start/

## Related agentmoat topics

These are reachable from the CLI via `agentmoat explain <topic>`:

- [gVisor 101](gvisor-101.md): how the runtime that `runsc` invokes is
  architected (Sentry, Gofer, platforms).
- [Threat model](threat-model.md): why agentmoat steers workloads onto
  this RuntimeClass in the first place.
- [Compatibility checklist](compatibility-checklist.md): which workload
  features force agentmoat to decline opting into the RuntimeClass.
