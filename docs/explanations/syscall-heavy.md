## Why we flagged this

The image hint matches a known syscall-heavy workload (redis,
memcached). These are in-memory data stores whose hot path is many
small syscalls per second: `recvfrom`, `sendto`, `epoll_wait`,
`writev`, and friends. Under gVisor, each one of those syscalls is
trapped by `systrap`, handed to the Sentry, executed in Go inside the
Sentry, and the result returned. The per-syscall fixed cost is low,
but at hundreds of thousands of syscalls per second per pod it adds
up. This is informational, not blocking: the workload will run, just
slower than on `runc`.

## Expected overhead

- Mostly-pipelined Redis (large `MGET`, `LRANGE`, `MSET` batches):
  20-50% latency increase. The batching amortises the per-syscall
  cost, so this is the friendly end.
- Memcached or Redis with small commands at high rate (typical web
  cache workload): 2-3x latency on p99, occasionally worse.
- Pathological cases (very small commands, single client connections,
  high concurrency, fsync-heavy persistence): up to 5-10x latency on
  p99 versus `runc`. Source: gVisor performance guide.
- Throughput (operations per second per core) drops by a similar
  factor; the per-op cost is what dominates.
- CPU usage on the same throughput rises proportionally; capacity
  planning should account for the extra cores.

## How to measure

1. Replay a representative trace against a Redis or Memcached running
   on a gVisor node. Use `memtier_benchmark`, `redis-benchmark`, or a
   recorded production trace; synthetic uniform load understates the
   real overhead.
2. Compare p50, p95, and p99 latency at the same offered QPS against
   the `runc` baseline. The mean usually looks reasonable while the
   tail tells the real story.
3. Watch CPU saturation: if the workload was already near a core's
   capacity on `runc`, it may need more cores on gVisor for the same
   QPS.
4. Run `agentmoat verify --in-pod-probe` to confirm the pod is on
   `runsc` and the benchmark is meaningful.
5. If the overhead is unacceptable, the classifier's info severity
   means migration is still possible; you have to make the trade-off
   explicit. Most teams leave latency-critical caches on `runc` and
   sandbox the surrounding application tier instead.

See also: [gVisor performance guide](https://gvisor.dev/docs/architecture_guide/performance/)
