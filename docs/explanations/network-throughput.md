## Why we flagged this

The image hint matches a known network-throughput-bound workload
(nginx, envoy, haproxy, traefik). These are L7 proxies and edge web
servers whose hot path is "shovel bytes between sockets as fast as the
NIC will go". Under gVisor, those sockets terminate in netstack, the
user-space TCP/IP stack that lives inside the Sentry. Every packet
crosses the trap-and-emulate boundary, and the netstack does not have
the same hardware-offload story (GRO, GSO, TSO) the host kernel does.
The result is real throughput overhead, especially on large flows.

This is informational, not blocking. The migration will succeed and the
workload will function correctly; you just need to budget for the
network tax up front.

## Expected overhead

- Small request/response (REST APIs, sub-1KB bodies): typically
  5-15% latency increase. Often acceptable for security-sensitive
  edge traffic.
- Mixed-traffic reverse proxies (most nginx and envoy deployments
  in front of backend services): 20-40% throughput drop versus `runc`
  on the same node. Source: gVisor performance guide.
- Large-flow static file serving (Apache, nginx serving big assets):
  40-60% throughput drop. This is the upper end and shows up most
  clearly on streams that would otherwise saturate the NIC.
- Long-lived connections with low request rate (websockets,
  server-sent events): negligible throughput cost; the per-message
  syscall overhead is what you actually pay.

## How to measure

1. Capture the production traffic profile (request rate, request size,
   p50/p95/p99 latency, concurrent connections). The overhead is
   profile-dependent; the gVisor docs numbers are a starting point.
2. Roll one replica into a staging environment running on a gVisor
   node, behind a replayed-traffic generator like `wrk2`, `vegeta`, or
   a tcpreplay rig.
3. Compare against the `runc` baseline at the same offered load. Watch
   p99 latency and saturating throughput; mean latency usually
   under-represents the cost.
4. Run `agentmoat verify --in-pod-probe` to confirm the pod is
   actually on `runsc` and not a silent fallback.
5. Decide on a per-workload basis whether the security benefit is
   worth the throughput cost. The classifier deliberately keeps this
   as info, not warn, because the answer is operator-specific.

See also: [gVisor performance guide](https://gvisor.dev/docs/architecture_guide/performance/)
