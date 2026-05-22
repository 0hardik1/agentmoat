## Why this warrants review

The workload uses FUSE: either a CSI driver whose name contains "fuse"
is mounting a volume into the pod, or the `AGENTMOAT_USES_FUSE`
environment variable is set on a container. FUSE (Filesystem in
Userspace) lets a user-space process implement a filesystem that the
kernel then exposes as a mount. Common consumers are object-storage
adapters (`s3fs`, `gcsfuse`, `goofys`), overlay-style developer tooling,
and some CSI drivers.

gVisor implements a subset of FUSE. The Sentry now has a FUSE client
that can talk to a FUSE server running in user space on the host, and
the common read/write path works. Edge cases around extended
attributes, locking, splice, and some control-plane messages still have
sharp corners, and the support matrix improves release to release.

## What might break

- Throughput on large sequential reads can drop sharply versus the
  same FUSE mount under `runc`, because every operation hops Sentry,
  the FUSE protocol, and Gofer.
- Extended attributes (`xattr`) used by some object-storage backends
  for metadata projection may be silently dropped.
- `flock` and POSIX advisory locks across FUSE mounts behave
  inconsistently. Workloads that coordinate via lockfiles on the
  mount need re-testing.
- `mmap` of large files backed by FUSE can fail or be very slow.
- Concurrent writes from multiple pods to the same FUSE mount on the
  same node may see ordering differences from the `runc` case.

## How to validate

1. Identify the exact FUSE driver in play (CSI driver image tag,
   userspace binary version). gVisor's FUSE compatibility is per-driver
   in practice, not a single matrix.
2. Run `agentmoat verify --in-pod-probe` to confirm the mount is
   actually visible inside the sandbox and a basic read/write works.
3. Replay a representative workload trace against the mount in a
   staging cluster. Include the largest files you expect in production.
4. If the workload uses `mmap` against the mount, build a small mmap
   smoke test and run it before committing to the migration.
5. Watch the gVisor logs (`runsc debug`) for `unimplemented`
   diagnostics during the trial run. Those are the surface you have
   not exercised under `runc` either.

See also: [gVisor filesystem guide](https://gvisor.dev/docs/user_guide/filesystem/)
