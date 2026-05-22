# agentmoat

A Go-based read-only assessment and one-shot migration toolkit that helps EKS
(and any Kubernetes) operators move workloads from the default `runc`/containerd
runtime to gVisor (`runsc`): safely, observably, and with AI agents as
first-class operators.

agentmoat scans a cluster, classifies workloads by gVisor compatibility, generates a
migration plan, applies it via `RuntimeClass`, and verifies post-migration
health. It is designed to be operable by humans and by AI agents (MCP server,
JSON output, default dry-run, deterministic exit codes).

## Problem

In 2025-2026, agentic workloads (LLM agents running tools, executing code,
fetching URLs) moved from prototypes to production, but the container security
floor has not kept up. Three November 2025 `runc` CVEs allow container escape
to host root, and Microsoft disclosed RCE-via-prompt-injection in major agent
frameworks. The combined attack path is concrete: prompt injection then
in-container shell then kernel exploit then cluster compromise. gVisor is the
most pragmatic mitigation for this profile, but on AWS EKS it is not turn-key:
AWS does not officially support gVisor, Bottlerocket does not ship `runsc`, EKS
managed node groups have no gVisor switch, and there is no published gVisor
compatibility scanner or migration tool. agentmoat fills that gap.

## What it does

- **Scans and classifies** every Pod/Deployment/StatefulSet/DaemonSet/Job/CronJob
  for gVisor compatibility, flagging raw sockets, eBPF, GPU passthrough, and
  other known incompatibilities from the gVisor user guide.
- **Plans and applies** the migration: orders workloads lowest-risk first,
  patches Pod templates with `runtimeClassName: gvisor`, and labels/taints
  nodes. Every mutating command defaults to `--dry-run=true`.
- **Verifies** post-migration health by polling the kubelet runtime field and
  running an in-pod probe. Deterministic exit codes (`0` success, `2`
  compatibility issues, `3` partial apply, `4` verify failed) make it agent-
  friendly.

## Status

Phase 0 + Phase 1 in progress. See `plan.md` for the full design. (`plan.md`
is gitignored locally but referenced for design history.)

## Quickstart

### Local (kind)

```bash
# Bring up a kind cluster with gVisor preinstalled in the node container.
# (Script to be added in Phase 1.)
./scripts/kind-up.sh

# Scan the cluster and emit a human-readable table.
agentmoat scan

# Same scan as machine-readable JSON.
agentmoat scan --output json
```

### EKS (Packer AMI build)

```bash
cd packer
packer init .
packer validate .
packer build .
```

The build produces an EKS-optimized AL2023 AMI with gVisor (`runsc` plus the
containerd shim) preinstalled and the containerd drop-in config pre-staged. See
[`docs/eks-deployment.md`](docs/eks-deployment.md) for the end-to-end recipe.

## Documentation

- [Architecture](docs/architecture.md): the Go library at the core, CLI and MCP
  as thin shells.
- [RuntimeClass 101](docs/runtimeclass-101.md): a one-page intro for K8s
  engineers new to the API.
- [gVisor 101](docs/gvisor-101.md): Sentry, Gofer, platforms, and the
  performance trade-offs.
- [Threat model](docs/threat-model.md): what gVisor stops that `runc` does not,
  with CVE references.
- [Compatibility checklist](docs/compatibility-checklist.md): which workloads
  will not work under gVisor, and why.
- [Exit codes](docs/exit-codes.md): the deterministic exit codes used by every
  agentmoat command.
- [EKS deployment](docs/eks-deployment.md): the Packer + EKS end-to-end recipe.
- [Kind quickstart](docs/kind-quickstart.md): bring up a local cluster with
  gVisor preinstalled.
- [MCP integration](docs/mcp-integration.md): wiring `agentmoat-mcp` into
  Claude Code and other MCP clients.

## Why agentmoat

The name encodes both the threat model and the AI-ready posture. *moat*: a
defensive perimeter (the gVisor sandbox boundary). *agent*: the workload class
that most benefits, and the operator persona for MCP integration.

## License

Apache License 2.0. See [LICENSE](LICENSE).
