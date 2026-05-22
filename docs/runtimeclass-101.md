# RuntimeClass 101

`RuntimeClass` is the Kubernetes mechanism for opting a Pod into a non-default
container runtime. agentmoat uses it to route selected workloads to gVisor's
`runsc` while leaving the rest of the cluster on the default `runc`.

## The objects involved

1. **`RuntimeClass`** is a cluster-scoped resource (`node.k8s.io/v1`) that
   names a container runtime *handler*.
2. A **Pod** opts in by setting `spec.runtimeClassName: gvisor`.
3. The **kubelet** consults the named `RuntimeClass`, extracts its
   `handler` field, and tells **containerd** which OCI runtime to invoke.
4. **containerd** is configured per node (in `/etc/containerd/config.toml`
   or a drop-in under `/etc/containerd/config.d/`) to map the handler
   `runsc` to the `containerd-shim-runsc-v1` binary.

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
| `scheduling.nodeSelector` | Pin gVisor pods to nodes that actually have `runsc` installed (label `runtime: gvisor`). |
| `scheduling.tolerations` | Pair with a `runtime=gvisor:NoSchedule` taint on the gVisor nodes so other workloads stay off them. |
| `overhead.podFixed` | Tell the scheduler about Sentry's resident memory and CPU overhead (~140Mi / 250m by default). |

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
