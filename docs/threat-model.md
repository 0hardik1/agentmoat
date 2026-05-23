# Threat model

agentmoat exists because the default container runtime (`runc` plus a
shared host kernel) is a weak isolation boundary for untrusted or agentic
workloads. This document spells out the threat model agentmoat assumes and
which classes of vulnerability gVisor mitigates.

## Attacker model

1. The attacker can execute arbitrary code inside one container. The
   entry point is typically prompt injection in an agentic workload, an
   RCE in an exposed service, or a compromised supply-chain dependency.
2. The container itself runs unprivileged (no root, no extra capabilities).
3. The attacker's goal is to escape the container, reach the host kernel
   syscall surface, and pivot to other tenants or the cluster control
   plane.

This is the exact path documented in Microsoft's May 2026 blog "Prompts
become shells", and the path exploited by the three Nov 2025 `runc` CVEs.

## What gVisor changes

| Layer | `runc` | gVisor (`runsc`) |
| --- | --- | --- |
| Kernel syscall surface | ~319 syscalls reach the host | ~100-150 reach Sentry; Sentry re-implements the rest in Go |
| Filesystem access | Direct bind mounts and namespaces | All filesystem ops brokered by Gofer |
| Network stack | Host bridge | Sentry's user-space netstack (network=sandbox) |
| Capabilities | Honored at host level | Most are no-ops; the sandbox boundary makes them meaningless |

## CVEs gVisor would have mitigated

### CVE-2025-31133, CVE-2025-52565, CVE-2025-52881 (Nov 2025 `runc` CVEs)

All three allow container escape to host root via symlink races,
masked-paths manipulation, and procfs write redirects. gVisor workloads
use `runsc`, not `runc`, as the inner runtime; the host's `runc` may still
be vulnerable but it is not on the path. See the
[Sysdig writeup](https://www.sysdig.com/blog/runc-container-escape-vulnerabilities).

### CVE-2022-0847 (Dirty Pipe)

Host kernel page-cache write to privilege escalation. Sentry never touches
the host pipe implementation; the vulnerable code path is unreachable from
inside the sandbox.

### CVE-2020-14386 (Packet Ring Buffer)

Out-of-bounds write via `PACKET_RX_RING`. Sentry does not implement
`PACKET_RX_RING`; the vulnerable code path cannot be triggered. See the
[gVisor blog post on this CVE](https://gvisor.dev/blog/2020/09/18/containing-a-real-vulnerability/).

## What gVisor does NOT protect against

- **Application-level vulnerabilities.** SQL injection, prompt injection
  ending in data exfiltration via approved tools, hardcoded secrets, etc.
  These need application-layer mitigations.
- **Resource exhaustion.** Sentry accounts cgroup usage but does not
  *enforce* limits as aggressively as the host kernel. Pair with K8s
  resource requests/limits.
- **Bugs in gVisor itself.** Sentry has had CVEs; defense in depth still
  applies. Report at https://gvisor.dev/.
- **Side channels and timing attacks.** Sentry shares the host CPU; cache
  side-channel attacks against other tenants are not mitigated.

## How agentmoat helps

1. **`agentmoat scan`** flags every workload that would not work under
   gVisor (raw sockets, eBPF, GPU passthrough outside `nvproxy`, etc.) so
   you know the blast radius before flipping the switch.
2. **`agentmoat plan`** orders the migration lowest-risk first (stateless,
   no host-network, no privileged) so you can validate the pipeline on
   safe workloads before touching critical ones.
3. **`agentmoat apply --dry-run`** shows you the exact pod-spec diff
   before you mutate the cluster.
4. **`agentmoat verify`** confirms post-migration that live pods carry the
   expected `runtimeClassName`. Pass `--in-pod-probe` to also check for
   gVisor markers inside the container (dmesg, `/proc/cmdline`, `uname`).

## References

- Microsoft: [Prompts become shells: RCE in AI agent frameworks](https://www.microsoft.com/en-us/security/blog/2026/05/07/prompts-become-shells-rce-vulnerabilities-ai-agent-frameworks/)
- Sysdig: [New runc vulnerabilities](https://www.sysdig.com/blog/runc-container-escape-vulnerabilities)
- gVisor blog: [Containing a real vulnerability (CVE-2020-14386)](https://gvisor.dev/blog/2020/09/18/containing-a-real-vulnerability/)

## Related agentmoat topics

- [gVisor 101](gvisor-101.md): how Sentry, Gofer, and the platforms make
  the syscall-surface reduction possible.
- [Compatibility checklist](compatibility-checklist.md): the workloads
  whose threat-mitigation gains agentmoat declines to capture (and why).
