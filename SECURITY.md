# Security policy

## Reporting a Vulnerability

If you think you have found a security vulnerability in agentmoat, please do
**not** open a public issue. Instead, choose one of:

1. **Private GitHub Security Advisory.** Open a private vulnerability report
   via the repository's "Security" tab. This is the preferred channel: it
   gives us a private space to coordinate a fix and a CVE if warranted.
2. **Email.** Send details to `security@agentmoat.example`.
   <!-- TODO: replace this placeholder address before the first public release. -->

Please include:

- A short description of the issue and its impact.
- Reproduction steps or a proof-of-concept, if you have one.
- The agentmoat version (binary version, commit SHA, or release tag) and the
  environment (Kubernetes version, OS, runtime).
- Any suggested remediation, if you have ideas.

We will acknowledge receipt within 5 business days and aim to provide a
substantive response within 14 days. Coordinated disclosure timelines are
agreed case by case.

## Threat model

agentmoat exists because the default container runtime (`runc` plus shared
host kernel) is a weak isolation boundary for untrusted or agentic workloads.
The Nov 2025 `runc` CVEs (container escape via symlink races, masked-paths
manipulation, procfs write redirects) and the broader history of kernel
exploits (Dirty Pipe, packet ring-buffer OOB, and others) all assume an
attacker can reach the host syscall surface from inside a container. gVisor
inserts the Sentry user-space kernel between the workload and the host,
shrinking that surface from roughly 319 syscalls to roughly 100-150 and
removing entire classes of bug-prone host code paths from reach.

For the full attacker model, the trust boundaries agentmoat introduces, and
which CVEs are quoted in the design, see
[`docs/threat-model.md`](docs/threat-model.md).

## Scope

### In scope

- The `agentmoat` CLI binary and the `pkg/agentmoat` Go library.
- The `agentmoat-mcp` MCP server.
- The Packer template at `packer/eks-gvisor-al2023.pkr.hcl` and the AL2
  variant, plus the containerd and `runsc` config files baked into the AMI.
- The deploy manifests under `deploy/` (RuntimeClass, Karpenter NodeClass
  example, etc.) and the kind support under `kind/`.

### Out of scope

- **gVisor itself.** Vulnerabilities in `runsc`, the Sentry, the Gofer, or the
  containerd shim belong to the upstream gVisor project. Report at
  [gvisor.dev/security](https://gvisor.dev/) or via
  [the gVisor GitHub repository](https://github.com/google/gvisor).
- **`runc` CVEs.** Vulnerabilities in `runc` itself (the default OCI runtime
  that agentmoat is migrating *away* from) belong to the OCI / `runc`
  upstream. Report at
  [github.com/opencontainers/runc/security](https://github.com/opencontainers/runc/security).
- **Kubernetes core, containerd, AWS EKS, and the EKS-optimized base AMI.**
  Report to those projects' respective security channels.

We are happy to relay credible reports against upstream components to the
correct channel if you are unsure where to send them.
