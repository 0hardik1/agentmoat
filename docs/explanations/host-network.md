## Why incompatible

The pod requests `spec.hostNetwork: true`, which asks the kubelet to place
the pod's network namespace inside the host's. gVisor cannot honour this.
The sandbox ships its own user-space TCP/IP stack (netstack) that runs
inside the Sentry process, and that stack is deliberately detached from
the host's network namespace. There is no mechanism for the Sentry to
join, bridge, or share the host netns; doing so would defeat the
isolation guarantee the sandbox exists to provide.

In practice, `runsc` will either refuse to start the pod or start it with
a private netstack that ignores the `hostNetwork` request, which is worse
because the workload starts but cannot see the interfaces it expects.

## What this looks like under runc

Under `runc`, a `hostNetwork: true` pod sees the node's interfaces
directly: it can bind to host ports, see host-local services on
`127.0.0.1`, and uses the host's routing table. Common consumers are
node-local agents (CNIs, log collectors, metrics exporters) that need
to observe traffic on the node.

## What to do instead

- If the workload is a node-local agent (CNI, log forwarder, metrics
  scraper), keep it on `runc`. These are the workloads gVisor was not
  designed to host. Exclude them from your RuntimeClass opt-in and move
  on.
- If the workload only needs a specific host port, run it without
  `hostNetwork` and expose the port with a `NodePort` or `HostPort` on
  the pod spec. gVisor handles `HostPort` fine.
- If the workload needs to reach `127.0.0.1` services on the node, route
  through the node IP and a `NodePort` Service instead.
- If you only set `hostNetwork: true` for "faster networking", measure
  first: gVisor's netstack overhead is real but it is not always larger
  than the benefit you thought you were getting.

See also: [gVisor networking compatibility](https://gvisor.dev/docs/user_guide/compatibility/#networking)
