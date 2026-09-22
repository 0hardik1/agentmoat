## Why this warrants review

The container runs systemd as PID 1. agentmoat takes a container to run
systemd when its `command[0]` (or `args[0]`, when there is no command) is
`/sbin/init`, `/usr/sbin/init`, `/lib/systemd/systemd`, or
`/usr/lib/systemd/systemd`; when it runs a Red Hat UBI init image
(`ubi9/ubi-init`, `ubi9-init`, ...) with no command or args of its own; or
when the workload carries the annotation `agentmoat.io/runs-systemd=true`.

Until September 2026 gVisor could not run systemd. Release
`20260831.0` added what systemd needs: cgroups v2 inside the sandbox, the
new mount API, pidfds, and `openat2(2)`
([gVisor blog, 2026-09-17](https://gvisor.dev/blog/2026/09/17/systemd-in-gvisor/)).
These are off by default. A systemd pod under gVisor needs three things:

| Requirement | Where it is set | What agentmoat can see |
| --- | --- | --- |
| runsc `release-20260831.0` or newer | The node image | The version, after `agentmoat probe nvproxy --dry-run=false` |
| The runsc flag `in-sandbox-cgroup=v2` | `runsc.toml`, or a pod annotation | Nothing: runsc flags are not in the Kubernetes API |
| `CAP_SYS_ADMIN` in the container | The pod spec | The capability (it also fires `perf-events`) |

With the runsc version from the probe, an older runsc makes this rule
`error`. A new enough runsc keeps the rule at `warn`, because agentmoat cannot
confirm the flag.

## What we tested

On the agentmoat kind cluster (runsc `release-20260914.0`, containerd with
`pod_annotations = ["dev.gvisor.*"]`), with `registry.access.redhat.com/ubi9/ubi-init`
and `runtimeClassName: gvisor`:

| `in-sandbox-cgroup=v2` | Container security | `systemctl is-system-running` |
| --- | --- | --- |
| no | default capabilities | `offline` (systemd stops early) |
| yes | default capabilities | `offline` |
| yes | `CAP_SYS_ADMIN` | `running` |
| yes | `privileged: true` | `running` |
| no | `privileged: true` | `degraded` (`systemd-journald` and other units fail) |

A tmpfs (`emptyDir: {medium: Memory}`) on `/run` made no difference. The
gVisor post covers Docker only; the Kubernetes results above are from
agentmoat's own test.

Why `CAP_SYS_ADMIN`: containerd mounts `/sys/fs/cgroup` read-only in an
unprivileged container. systemd remounts it read-write, and that needs
`CAP_SYS_ADMIN`. Under gVisor the capability is checked by the Sentry, gVisor's
user-space kernel, inside the sandbox. The gVisor post says the same about
`--privileged`: it "does *not* give any extra host privilege to the
application running in the sandbox; gVisor *simulates* the extra privileges".

## What might break

- **The flag is missing.** systemd stops at `offline` or runs `degraded`,
  and journald does not start. The pod still shows `Running`, because PID 1
  is alive, so readiness probes are the only signal.
- **`CAP_SYS_ADMIN` is missing.** systemd stops at `offline`. `systemctl` in
  the container reports "System has not been booted with systemd as init
  system".
- **setuid binaries.** `su`, `sudo`, and `ping` as a non-root user need the
  runsc flag `allow-suid` too. Without it runsc ignores SUID/SGID bits and
  logs "--allow-suid is disabled". systemd itself does not need it.
- **`privileged: true`.** It works under gVisor, but agentmoat's `privileged`
  rule is `error` and blocks the workload. Use `CAP_SYS_ADMIN` instead, which
  is enough for systemd.
- **`perf-events` also fires.** That rule matches `CAP_SYS_ADMIN`. Under
  gVisor it is expected for a systemd pod.
- **A different image.** agentmoat does not read image config. An image that
  starts systemd from its own `ENTRYPOINT`/`CMD` (not a UBI init image) is
  not detected. Set `agentmoat.io/runs-systemd=true` on it.

## How to turn systemd on

The flag can be set for every sandbox of a runtime handler, or per pod.

For every sandbox, in the `runsc.toml` that the containerd runtime's
`ConfigPath` names (`/etc/containerd/runsc.toml` in agentmoat's kind and
Packer images):

```toml
[runsc_config]
  in-sandbox-cgroup = "v2"
  # Only for images that use setuid binaries:
  # allow-suid = "true"
```

Per pod, with the annotation below. runsc accepts it because
`in-sandbox-cgroup` is on its list of flags that container authors may set.
containerd passes the annotation to runsc only if the runtime is registered
with `pod_annotations = ["dev.gvisor.*"]`. agentmoat's kind config does this.
The Packer drop-in (`packer/files/gvisor-runtime.toml`) does not.
`allow-suid` is not on runsc's list, so it can only go in `runsc.toml`.

```yaml
metadata:
  annotations:
    dev.gvisor.flag.in-sandbox-cgroup: "v2"
spec:
  runtimeClassName: gvisor
  containers:
    - name: os
      image: registry.access.redhat.com/ubi9/ubi-init
      securityContext:
        capabilities:
          add: ["SYS_ADMIN"]
```

A per-pod annotation is part of the pod spec, and `agentmoat apply` changes
only `runtimeClassName`. Add the annotation and the capability to the
workload's own manifest before the migration.

## How to validate

1. Run `agentmoat probe nvproxy --dry-run=false --output json > probe.json`,
   then `agentmoat scan --facts probe.json`. An `error` says the node's runsc
   is older than `release-20260831.0`: upgrade the node image first (see
   [`gvisor-version.md`](../gvisor-version.md)).
2. Set the flag (`runsc.toml` or the pod annotation) and add
   `CAP_SYS_ADMIN`.
3. Migrate a test copy of the workload first, then run
   `kubectl exec <pod> -- systemctl is-system-running`. Expect `running`.
   `degraded` usually means the flag is not in effect:
   `kubectl exec <pod> -- stat -f -c %T /sys/fs/cgroup` prints `cgroup2fs`
   with the flag and `tmpfs` without it.
4. Run `kubectl exec <pod> -- systemctl --failed` and check that each unit
   the workload needs is active.
5. When the flag is confirmed on the nodes, lower `systemd-init` to `info`
   with a `--rules` override. A runsc older than `release-20260831.0` still
   makes the rule `error`.

See also: [gVisor blog: systemd in gVisor](https://gvisor.dev/blog/2026/09/17/systemd-in-gvisor/)
