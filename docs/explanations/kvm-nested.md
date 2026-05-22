## Why incompatible

The pod mounts `/dev/kvm` from the host (typically as a hostPath
volume), which a workload uses to spin up nested virtual machines via
the KVM hypervisor. gVisor cannot pass `/dev/kvm` into the sandbox.
Even if it could, the Sentry does not implement the `ioctl` surface that
KVM uses for VM lifecycle, vCPU control, and memory region management;
the workload would see the device node but every interesting call would
return `ENOTTY`.

There is a separate concern that compounds this on cloud infrastructure.
On AWS EC2 (and therefore EKS), nested virtualization is disabled at the
hypervisor level: even on `runc`, KVM does not work in a guest VM. So
even if you swapped the runtime back to `runc` to satisfy the workload,
the cluster topology still would not support it. This rule is incompatible
on EKS regardless of runtime.

## What this looks like under runc

Under `runc` on bare-metal nodes (or VMs with nested virt enabled), a
container that mounts `/dev/kvm` can run KubeVirt VMs, kata-containers,
Firecracker microVMs, or anything else that uses KVM as its hypervisor.

## What to do instead

- For lightweight isolation (the usual reason teams reach for nested
  KVM), use gVisor itself. That is what this whole exercise is about:
  if the goal was "stronger boundary than `runc`", gVisor delivers
  that without nested virt.
- For VM-style workloads on EKS, consider a separate node pool on
  bare-metal instance types (`m5.metal`, `c5.metal`) where nested virt
  is actually available, and keep that pool on `runc`.
- For KubeVirt specifically, the project's own documentation recommends
  dedicated bare-metal nodes; do not try to sandbox the virt-launcher.
- If the workload only uses `/dev/kvm` for development convenience
  (e.g. nested Docker for builds), look at rootless or fully unprivileged
  build tooling (kaniko, buildah) that does not need a hypervisor.

See also: [gVisor compatibility matrix](https://gvisor.dev/docs/user_guide/compatibility/)
