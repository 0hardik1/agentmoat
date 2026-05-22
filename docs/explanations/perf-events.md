## Why this warrants review

The container requests `CAP_PERFMON` (or `CAP_SYS_ADMIN`, which
historically gates the same surface). These capabilities are the
gateway to `perf_event_open(2)`, the Linux interface that backs
`perf`, hardware performance counters, kernel tracepoints, and the
profiling tools that read from `/proc/<pid>/perf_event`. gVisor does
not implement `perf_event_open`: the syscall returns `ENOSYS` from the
Sentry, regardless of capabilities granted to the workload.

This is partly a security choice and partly a practical one. Perf
events expose hardware counters and kernel tracepoints that live on
the other side of the sandbox boundary, and re-implementing them
inside the Sentry would mean either lying to the workload about what
the CPU is doing or punching a hole through the boundary to the host's
PMU. Neither is acceptable, so the feature is simply absent.

## What might break

- `perf record`, `perf stat`, and `perf top` will fail with `ENOSYS`
  when run inside a sandboxed pod.
- Continuous profilers (`parca-agent`, `pyroscope-eBPF`, `pprof` with
  hardware-counter sources) will not be able to collect samples.
- JVM workloads that use `-XX:+UseProfiledLoads` or `async-profiler`
  with `perf_events` mode will fall back to safepoint sampling if they
  detect the absence, or fail outright.
- Applications that read `/proc/self/stat` for CPU stats still work;
  it is only `perf_event_open(2)` that is gone.
- Some language runtimes' built-in profilers (Go's pprof, .NET's
  EventPipe) are unaffected: they sample in user space without
  touching `perf_event_open`.

## How to validate

1. Determine whether the workload actually exercises `perf_events` or
   just had `CAP_SYS_ADMIN` set defensively. The capability is broad;
   most workloads holding it do not call `perf_event_open`.
2. If profiling is needed in production, switch to a user-space
   profiler that does not touch `perf_event_open` (Go pprof,
   `async-profiler` in safepoint mode, `py-spy`).
3. Run `agentmoat verify --in-pod-probe` and have the probe call
   `perf_event_open` directly; the explicit `ENOSYS` confirms the
   surface is missing rather than just misconfigured.
4. Move the profiler to a node-level DaemonSet on `runc` rather than
   profiling from inside the sandboxed pod.
5. For CI workloads that need `perf` for one-off benchmarks, exclude
   them from the RuntimeClass opt-in and keep them on `runc`.

See also: [gVisor compatibility matrix](https://gvisor.dev/docs/user_guide/compatibility/)
