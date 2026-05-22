## Why incompatible

The pod sets `spec.hostPID: true`, which under `runc` means the
container's processes share the host's PID namespace and can see (and
signal) every process on the node. gVisor cannot honour this. The Sentry
runs its own PID namespace inside the sandbox: PID 1 in the workload is
the Sentry-managed init, and the Sentry has no view of host PIDs at all.
The Linux PID namespace is enforced by the host kernel, and the Sentry
sits between the workload and that kernel by design.

Allowing the sandbox to share the host PID namespace would require
exposing host PIDs through the Sentry's process table, which is exactly
the kind of host-visible state the sandbox boundary is built to deny.

## What this looks like under runc

Under `runc`, a `hostPID: true` container can run `ps -ef` and see every
process on the node, can `kill -SIGTERM <host-pid>`, and can inspect
`/proc/<host-pid>/`. This is how node-local debuggers, profilers, and
some "sidecar that nudges the kubelet" patterns work.

## What to do instead

- For node-level debugging, run a `kubectl debug node/<node>` ephemeral
  pod on `runc` rather than trying to make a long-lived gVisor pod do
  it. The debug pod is a one-off and does not need to be sandboxed.
- For process metrics, scrape via the kubelet's `/metrics/cadvisor`
  endpoint or a dedicated `runc`-hosted DaemonSet rather than reaching
  into host PIDs from inside the workload.
- For graceful shutdown ordering between containers in the same pod,
  use `shareProcessNamespace: true` (in-pod, not host-wide). gVisor
  supports this because it is fully inside the sandbox.
- If the workload genuinely requires `hostPID`, it is a node agent.
  Keep it on `runc` and exclude it from the RuntimeClass opt-in.

See also: [gVisor compatibility matrix](https://gvisor.dev/docs/user_guide/compatibility/)
