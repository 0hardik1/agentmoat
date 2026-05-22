## Why incompatible

The container sets `securityContext.privileged: true`. Under `runc` this
is a single flag that drops capability and seccomp filtering, exposes
the full `/dev` tree, and lets the container speak to the host kernel
with very few restrictions. Under gVisor the flag is essentially
meaningless: the sandbox boundary remains in place regardless, and the
syscalls the workload needs to actually exercise its "privileged" status
are exactly the ones the Sentry refuses to forward to the host.

Several capabilities that commonly accompany `privileged: true` are also
the ones gVisor declines to honour even when granted: `CAP_SYS_ADMIN` is
the canonical example. gVisor implements `CAP_SYS_ADMIN` as a partial
surface, the bits used for namespace bookkeeping inside the sandbox
work, but the bits used to mount host filesystems, manipulate cgroups,
or call `bpf(2)` do not.

## What this looks like under runc

A privileged `runc` container can mount filesystems, load kernel
modules, see all devices under `/dev`, manipulate host networking, run
nested Docker, or attach to host processes via `/proc/<pid>/`. The flag
is a hammer; lots of legacy automation reaches for it when the actual
need is one or two narrow capabilities.

## What to do instead

- Audit which capabilities the workload actually exercises. Often
  `privileged: true` was inherited from a base image or copy-pasted
  helm chart, and the real requirement is a single capability like
  `CAP_NET_BIND_SERVICE`.
- Grant only the narrow `capabilities.add` list the workload needs and
  drop the privileged flag. Re-run `agentmoat scan` and see whether the
  workload is then compatible.
- If the workload truly needs `CAP_SYS_ADMIN` to mount filesystems or
  call `bpf(2)`, it is a node-level workload and should stay on `runc`.
- If the workload is "privileged because docker-in-docker", consider
  switching to a non-privileged alternative (kaniko, buildah rootless)
  that does not need to mount overlay filesystems on the host.

See also: [gVisor compatibility matrix](https://gvisor.dev/docs/user_guide/compatibility/)
