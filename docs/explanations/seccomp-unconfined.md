## Why we flagged this

At least one container in this workload sets
`securityContext.seccompProfile.type: Unconfined` (on the container, or
on the pod with no container setting to override it).

Until gVisor release 20261123.1 that made no difference under `runsc`:
runsc did not load a container's OCI seccomp filter inside the sandbox
unless the runtime was configured with `--oci-seccomp`, so a
`RuntimeDefault` pod and an `Unconfined` pod could call the same
syscalls. From 20261123.1 the flag defaults to true. runsc now installs
the container's filter in the Sentry, and a `RuntimeDefault` or
`Localhost` profile refuses the syscalls it lists (with containerd's
default profile, calls such as `unshare`, `mount`, `keyctl`, `bpf` and
`get_mempolicy` fail with `EPERM`). `Unconfined` is now the only way to
opt a gVisor workload out of that filter.

This is informational. gVisor still isolates the workload from the host
kernel; the seccomp filter is a second layer inside the sandbox, and the
verdict and the plan do not change. The point is that the setting now
does something, so it should be a choice.

Not flagged: a container with no profile at all. What it gets depends on
the kubelet (`Unconfined`, unless the kubelet runs with
`seccompDefault: true`), which the pod spec cannot show. agentmoat reads
`securityContext.seccompProfile` only, not the deprecated seccomp
annotations.

## How to check what the filter would block

`agentmoat probe seccomp --plan plan.json --dry-run=false` runs a trial
copy of each workload in the plan in a gVisor sandbox with its own
profile and lists the syscalls the filter refused. To see what
`RuntimeDefault` would block for this workload, change its profile in a
copy of the manifest, apply that copy to a test namespace, and probe it
there.

## What to do

1. Find out why the profile is `Unconfined`. It is often left over from
   a runc-era workaround for one syscall that the default profile
   refused.
2. If the probe shows that `RuntimeDefault` refuses nothing the
   workload needs, switch to it.
3. If the workload needs a few syscalls the default profile refuses,
   write a `Localhost` profile that allows them, rather than staying
   `Unconfined`.
4. If `Unconfined` is the right choice, keep it. To make this rule
   block instead, raise its severity in
   `internal/rules/gvisor.yaml`.
