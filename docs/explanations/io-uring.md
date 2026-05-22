## Why this warrants review

The workload is annotated `agentmoat.io/uses-iouring=true`, which says
the application uses `io_uring` for asynchronous I/O. `io_uring` is a
relatively recent (kernel 5.1+) interface that lets a process submit
batches of I/O operations through a shared ring buffer with the kernel,
avoiding the per-syscall cost of `read`, `write`, and friends. It is
the modern fast path for high-IOPS storage workloads.

gVisor does not implement `io_uring`. The Sentry returns `ENOSYS` for
`io_uring_setup(2)`, which the application has to handle. Most modern
libraries that use `io_uring` (libuv, tokio's `io_uring` backend,
Postgres's experimental backend) have a runtime probe and fall back to
plain `read`/`write` or `epoll`+`aio` when `io_uring` is unavailable.
Some do not; those will fail at startup. Even when the fallback works,
the performance shape changes: you lose the batching and you lose the
syscall-amortisation that was the whole point of using `io_uring`.

## What might break

- Applications without a runtime fallback will fail at startup with
  `ENOSYS` from `io_uring_setup`.
- IOPS-bound workloads (database write paths, log ingestion) can see
  20-50% drops once the fallback path is in use, on top of the normal
  Sentry/Gofer overhead.
- Latency p99 on small writes typically gets worse because the
  fallback path issues one syscall per operation.
- Code paths that batch many file descriptors in a single ring may end
  up doing N times more syscalls under the fallback, multiplying the
  Sentry trap overhead.
- Library combinations that assume `io_uring` for both networking and
  storage (some Rust async runtimes) may misreport health metrics.

## How to validate

1. Confirm whether the workload has an `io_uring` fallback. Read the
   library's documentation or set `IORING_DISABLED=1` (or equivalent)
   on the runc baseline and see if the app still starts.
2. Run the workload in a gVisor pod and watch for `ENOSYS` log lines
   on `io_uring_setup` at startup; that tells you the fallback path is
   live.
3. Run `agentmoat verify --in-pod-probe` to confirm the runtime swap
   actually happened.
4. Benchmark the I/O hot path under both runtimes with `fio` or the
   workload's own load generator. The `io_uring` to fallback delta is
   usually larger than the gVisor-vs-runc delta.
5. If the workload is latency-critical and `io_uring` was load-bearing,
   plan to keep it on `runc` rather than absorb both costs.

See also: [gVisor compatibility matrix](https://gvisor.dev/docs/user_guide/compatibility/)
