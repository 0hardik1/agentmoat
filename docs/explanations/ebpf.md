## Why incompatible

The image hint matches a known eBPF-loading tool (Cilium agent,
Tetragon, Falco's eBPF driver) or the container requests `CAP_BPF`.
(`CAP_SYS_ADMIN` is handled separately by the `perf-events` rule; it
covers perf profiling and many admin operations, not eBPF loading
specifically. Legacy loaders that only drop `CAP_SYS_ADMIN` may surface
there instead of here.) eBPF programs run in the host kernel: the workload
calls `bpf(2)`, hands the kernel a verified program, and the program
then executes at points like syscall entry, tc ingress, kprobe, or
sched events. gVisor does not implement `bpf(2)`. There is no way to
forward the call to the host kernel either, because doing so would
hand a sandboxed workload the ability to install code that runs at
kernel privilege on the node, which is exactly the boundary the
sandbox exists to enforce.

The Sentry catches `bpf(2)` and returns `ENOSYS`. A program that
expects to load an eBPF object will fail at startup; one that probes
for eBPF support gracefully will fall back to a slower path if it has
one (Falco's kernel-module driver, for example).

## What this looks like under runc

Under `runc`, an eBPF agent loads its programs into the host kernel at
startup, attaches them to tracepoints or networking hooks, and reads
back events via perf or ring buffers. This is how Cilium implements
network policy, how Tetragon observes process execution, and how Falco
detects rule violations.

## What to do instead

- For network policy specifically, swap Cilium's eBPF datapath for an
  iptables-based one on the affected nodes, or run Cilium on a node
  pool that stays on `runc`. eBPF agents are infrastructure, not
  workloads; they belong outside the sandbox.
- For runtime security, use Falco's kernel-module driver on `runc`
  nodes, or rely on Sentry's own audit hooks for sandboxed workloads.
  Falco-on-eBPF inside a gVisor pod will not work.
- For tracing inside a single sandboxed workload, prefer
  user-space-only tracers (Sentry exposes some `ptrace` surface that
  works for in-sandbox probing).
- Run eBPF tools on the node, not in the pod. A `runc`-hosted
  DaemonSet observing the kernel is fine; the gVisor pods on the same
  node remain isolated.

See also: [gVisor compatibility matrix](https://gvisor.dev/docs/user_guide/compatibility/)
