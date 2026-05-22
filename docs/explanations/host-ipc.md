## Why incompatible

The pod sets `spec.hostIPC: true`, which asks for the container to share
the host's IPC namespace (System V semaphores, message queues, shared
memory segments, and POSIX shared memory under `/dev/shm`). gVisor
cannot honour this. The Sentry maintains a per-sandbox IPC namespace and
implements the SysV and POSIX shared-memory surface itself; there is no
plumbing that lets the sandbox reach into the host's IPC namespace,
because doing so would expose a kernel-mediated communication channel
across the sandbox boundary.

This is the same architectural reason `hostPID` and `hostNetwork` are
incompatible: the sandbox owns its namespaces, and host namespaces are
on the other side of the Sentry trust boundary.

## What this looks like under runc

Under `runc`, a `hostIPC: true` container can attach to host SysV shared
memory segments, exchange messages over host message queues, and read
or write files under the host's `/dev/shm`. This pattern shows up with
PostgreSQL replication helpers, scientific computing rigs, and some
legacy x11/audio sidecars.

## What to do instead

- For pod-internal shared memory (the common case), drop `hostIPC` and
  use the pod's own `/dev/shm`. gVisor supports POSIX shared memory
  inside the sandbox; only the host-wide variant is rejected.
- For cross-pod data sharing, use a Unix-domain socket over an
  `emptyDir` or a real Service: it survives a runtime swap and does
  not couple the workload to a node.
- For databases that genuinely depend on host SysV shm (rare in
  containerised deployments), keep the database on `runc`. Sandboxing
  a single-tenant database is rarely worth the complexity anyway.
- For audio/X11 forwarding patterns, prefer a Unix socket bind-mount
  over an `emptyDir` shared between sidecars in the same pod.

See also: [gVisor compatibility matrix](https://gvisor.dev/docs/user_guide/compatibility/)
