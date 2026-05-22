## Why incompatible

The container requests `CAP_NET_RAW`, or the workload was annotated
`agentmoat.io/needs-raw-socket=true`. Raw sockets (`socket(AF_INET,
SOCK_RAW, ...)`) let an application read and write IP datagrams at the
L3 layer, bypassing the kernel's transport stack. gVisor's netstack
disables raw sockets by default: the syscall returns `EPERM` even when
the capability is granted to the workload. This is a deliberate hardening
choice, because raw sockets expose enough of the wire format to
fingerprint, scan, and spoof in ways the kernel's normal TCP/UDP path
does not.

Raw sockets can be re-enabled, but only at runtime startup with
`runsc --net-raw`. That is a cluster-wide operator decision (the flag
lives in the runtime config, not on a Pod), so per-pod toggling is not
supported. If your cluster runs `runsc --net-raw`, this rule still fires
but the workload will actually function; treat the finding as a heads-up
that the security posture has been relaxed.

## What this looks like under runc

Under `runc`, granting `CAP_NET_RAW` is enough: the container can open
raw sockets, run `ping` (which uses ICMP raw sockets unless the system
has `ping_group_range` configured), run `tcpdump`, or implement custom
protocols at the IP layer.

## What to do instead

- For ICMP echo (the most common case), use a dgram ICMP socket
  (`SOCK_DGRAM` with `IPPROTO_ICMP`). gVisor supports this. Modern
  `ping` binaries will fall back to it automatically when the
  `ping_group_range` sysctl is permissive.
- For packet capture inside a pod, capture at the node level instead
  (a `runc`-hosted DaemonSet running `tcpdump`), not inside the
  sandboxed workload.
- If the workload is a network diagnostic or scanner that genuinely
  needs raw sockets, keep it on `runc`.
- If you control the cluster, you can enable raw sockets globally with
  `runsc --net-raw`. Understand the security trade-off first; this
  weakens the boundary for every sandboxed workload, not just one.

See also: [gVisor networking compatibility](https://gvisor.dev/docs/user_guide/compatibility/#networking)
