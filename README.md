# agentmoat

agentmoat moves Kubernetes workloads from the default `runc` runtime to gVisor
(`runsc`), the user-space kernel that defends against the kernel-exploit step of
a container-escape chain. It scans a cluster, classifies every workload by
gVisor compatibility, generates a deterministic migration plan with a stable
hash, applies it via `RuntimeClass`, and rolls it back on demand. On EKS
specifically there is no managed switch: Bottlerocket does not ship `runsc`,
managed node groups have no gVisor toggle, and AWS does not officially support
gVisor. agentmoat fills that gap.

The threat model and CVE backdrop live in [`docs/threat-model.md`](docs/threat-model.md).

## Quickstart

From a fresh clone, against a local kind cluster with real gVisor preinstalled:

```bash
git clone https://github.com/0hardik1/agentmoat
cd agentmoat
make kind-up          # builds the gVisor-enabled kind node image, then creates the cluster
make e2e              # runs scan -> plan -> apply -> rollback end-to-end and asserts the results
```

`make e2e` exercises the full pipeline against a real `runsc` runtime and
probes patched pods for gVisor markers in `dmesg`. It is the fastest way to
see the tool work.

Against your own cluster (`kubectl` context already set):

```bash
make build                                            # produces ./bin/agentmoat
./bin/agentmoat scan                                  # human-readable table
./bin/agentmoat scan --output json > scan.json        # versioned schema
./bin/agentmoat plan --scan scan.json --output json > plan.json
./bin/agentmoat apply --plan plan.json                # dry-run by default
./bin/agentmoat apply --plan plan.json --dry-run=false # actually mutate
./bin/agentmoat rollback --plan plan.json --dry-run=false
```

`scan` exits 0 when nothing is incompatible, 2 when at least one workload is.
`apply` and `rollback` are idempotent: re-running an applied plan reports every
step as `already-applied` and exits 0.

## What it does

```
   ┌──────────┐      ┌──────────┐      ┌──────────┐      ┌────────────┐
   │   scan   │ ───> │   plan   │ ───> │  apply   │ ───> │  rollback  │
   │ ScanReport│      │ Migration│      │  Apply   │      │  Rollback  │
   │  (RO)    │      │   Plan   │      │  Result  │      │   Result   │
   └──────────┘      └──────────┘      └──────────┘      └────────────┘
       cluster      pure function      strategic-merge      reverse, also
       client-go    over a Scan        patch, default        default dry-run
                    Report             dry-run
```

- **`scan`** is strictly read-only. It enumerates every Pod, Deployment,
  StatefulSet, DaemonSet, Job, and CronJob across the selected namespaces,
  classifies each one against the built-in rules, and emits a versioned
  `ScanReport`.
- **`plan`** is a pure function from a `ScanReport` to a `MigrationPlan`.
  Same scan in, same plan and same SHA-256 `planHash` out. The plan orders
  steps by risk (stateless first, no host-network first, etc.) and excludes
  any workload classified `incompatible`.
- **`apply`** is the only stage that mutates the cluster. It patches each pod
  template with `runtimeClassName` and the matching `runtime=gvisor:NoSchedule`
  toleration, stamps the namespace with `agentmoat.io/plan-hash`, emits a
  Kubernetes Event per step, and appends a line to `~/.agentmoat/audit.jsonl`.
  It defaults to `--dry-run=true`. Re-running an applied plan reports every
  step as `already-applied` and exits 0 via the namespace annotation.
- **`rollback`** walks the same plan in reverse, removes `runtimeClassName`,
  and clears the namespace annotation. It deliberately leaves the toleration
  in place (a toleration without a matching taint is harmless, and removing a
  specific toleration by JSON Patch index is fragile).

The manual alternative is a sequence of `kubectl get` to enumerate, hand
classification against the gVisor docs, per-controller `kubectl patch` for
every Deployment/StatefulSet/DaemonSet, and a separate audit trail you build
yourself. agentmoat does the classification, orders the mutations, makes them
idempotent, records them, and undoes them.

## Compatibility checks

14 stable rule IDs across three severities. Rule IDs are a public surface:
they appear in `--output json`, in `--rules` overrides, and in the
`docs/compatibility-checklist.md` table. The rule implementations live in
[`pkg/classifier/builtin_rules.go`](pkg/classifier/builtin_rules.go).

| Rule ID             | Severity | What it inspects                                                       |
| ------------------- | -------- | ---------------------------------------------------------------------- |
| `raw-socket`        | error    | `CAP_NET_RAW`, or `agentmoat.io/needs-raw-socket=true`                 |
| `host-network`      | error    | `pod.spec.hostNetwork=true`                                            |
| `host-pid`          | error    | `pod.spec.hostPID=true`                                                |
| `host-ipc`          | error    | `pod.spec.hostIPC=true`                                                |
| `privileged`        | error    | Any container with `securityContext.privileged=true`                   |
| `ebpf`              | error    | Image hint (cilium, tetragon, falco) or `CAP_BPF`                      |
| `kvm-nested`        | error    | `hostPath` mount of `/dev/kvm`                                         |
| `host-path-mount`   | warn     | Any `hostPath` volume                                                  |
| `gpu-passthrough`   | warn     | `nvidia.com/gpu` resource request or limit                             |
| `fuse-mount`        | warn     | CSI driver name containing `fuse`, or `AGENTMOAT_USES_FUSE=true`       |
| `io-uring`          | warn     | Annotation `agentmoat.io/uses-iouring=true`                            |
| `perf-events`       | warn     | `CAP_PERFMON` or `CAP_SYS_ADMIN`                                       |
| `network-throughput`| info     | Image hint: nginx, envoy, haproxy, traefik (expect 20-40% overhead)    |
| `syscall-heavy`     | info     | Image hint: redis, memcached (expect higher latency)                   |

Any `error` rule firing makes the workload `incompatible` and excludes it from
the plan. `warn` makes it `review`; the planner only includes `review`
workloads when `--include-review` is passed. `info` is purely advisory and
never blocks.

Override severities (or add rules) without recompiling via `--rules
<file.yaml>`. See [`docs/compatibility-checklist.md`](docs/compatibility-checklist.md).

## Sample output

Shapes come straight from [`internal/schema/types.go`](internal/schema/types.go);
elided fields are marked `...`.

`agentmoat scan --output json` (one `WorkloadResult` from `.spec.workloads`):

```json
{
  "kind": "Deployment",
  "namespace": "edge",
  "name": "frontdoor",
  "compatibility": "review",
  "reasons": [
    {
      "ruleId": "network-throughput",
      "severity": "info",
      "description": "Workload appears network-throughput-bound (image hint: nginx, envoy, haproxy, traefik); expect ~20-40% throughput overhead under gVisor's sandbox network stack.",
      "remediationUrl": "https://gvisor.dev/docs/architecture_guide/performance/"
    }
  ],
  "recommendation": "Benchmark under gVisor before opting in; consider host-network alternatives if throughput-critical.",
  "overhead": "Network throughput: 20-40%"
}
```

`agentmoat plan --output json` (one `PlanStep` and the top-level `planHash`):

```json
{
  "apiVersion": "agentmoat.io/v1alpha1",
  "kind": "MigrationPlan",
  "metadata": {
    "generatedAt": "2026-05-22T17:14:03Z",
    "planHash": "9f4b1c2d8a3e6f5b7c1e0d2a4f6b8e3c5d7a9f1b2e4d6a8c0f1b3e5d7a9c2e4f"
  },
  "spec": {
    "summary": {"total": 3, "included": 2, "excluded": 1},
    "steps": [
      {
        "order": 1,
        "target": {"kind": "Deployment", "namespace": "default", "name": "web"},
        "action": "set-runtime-class",
        "runtimeClassName": "gvisor",
        "addToleration": true,
        "waitFor": "Ready",
        "riskScore": 10,
        "notes": "Stateless Deployment fronted by a Service; safe to roll first."
      }
    ],
    "excluded": [...]
  }
}
```

`agentmoat apply --output json` (one `StepResult` and the apply envelope):

```json
{
  "apiVersion": "agentmoat.io/v1alpha1",
  "kind": "ApplyResult",
  "metadata": {
    "planHash": "9f4b1c2d8a3e6f5b7c1e0d2a4f6b8e3c5d7a9f1b2e4d6a8c0f1b3e5d7a9c2e4f",
    "dryRun": false
  },
  "spec": {
    "summary": {"total": 2, "applied": 2, "alreadyApplied": 0, "skipped": 0, "failed": 0},
    "steps": [
      {
        "order": 1,
        "target": {"kind": "Deployment", "namespace": "default", "name": "web"},
        "status": "applied",
        "patch": "{\"spec\":{\"template\":{\"spec\":{\"runtimeClassName\":\"gvisor\",\"tolerations\":[{\"key\":\"runtime\",\"operator\":\"Equal\",\"value\":\"gvisor\",\"effect\":\"NoSchedule\"}]}}}}"
      }
    ]
  }
}
```

## Operational guarantees

- **Read-only by default.** `scan` and `plan` never mutate. They are safe to
  run against production from a CI job or a read-only kubeconfig.
- **Dry-run by default.** `apply` and `rollback` default to `--dry-run=true`:
  the patches are computed and surfaced in the `StepResult.patch` field, but
  nothing is sent to the API server. Mutating requires explicit
  `--dry-run=false`.
- **Idempotent.** Every `apply` writes the plan hash to the affected
  namespace as `agentmoat.io/plan-hash`. Re-running the same plan against
  the same cluster reports every step as `already-applied` and exits 0.
- **Auditable.** Every mutation appends one JSON line to
  `~/.agentmoat/audit.jsonl` (disable with `--no-audit`) and emits one
  Kubernetes Event on the patched object (disable with `--no-events`).
- **Deterministic exit codes.** CI scripts can branch on them:

  | Code | Meaning                                                   |
  | ---- | --------------------------------------------------------- |
  | 0    | Success. Cluster matches requested state.                 |
  | 1    | Generic error (kubeconfig, network, malformed plan, etc). |
  | 2    | `scan` / `explain namespace` / `explain workload`: at least one `incompatible` workload. |
  | 3    | `apply` or `rollback`: partial outcome; idempotent re-run is safe. |
  | 4    | `verify`: `runtimeClassName` mismatch and/or in-pod probe did not find gVisor. |

  Full table in [`docs/exit-codes.md`](docs/exit-codes.md).

## EKS via Packer

[`packer/eks-gvisor-al2023.pkr.hcl`](packer/eks-gvisor-al2023.pkr.hcl) builds
an EKS-optimized AL2023 AMI with `runsc` and the containerd v2 shim
preinstalled, the containerd drop-in pre-staged, and the systrap platform
pinned (KVM is unavailable on EKS instances).

```bash
cd packer
packer init .
packer validate .
packer build .
```

Wire the resulting AMI into a self-managed node group or a Karpenter
`EC2NodeClass`, label the nodes `runtime=gvisor`, taint them
`runtime=gvisor:NoSchedule`, and apply `deploy/runtimeclass.yaml`. The
end-to-end recipe (CFN/Terraform snippets, IAM, Karpenter wiring) is tracked
in [`docs/eks-deployment.md`](docs/eks-deployment.md).

## Local kind

`make kind-up` builds a custom kind worker image
([`kind/Dockerfile.gvisor-node`](kind/Dockerfile.gvisor-node)) that ships
`/usr/local/bin/runsc` and the containerd v2 shim. The cluster topology
([`kind/cluster.yaml`](kind/cluster.yaml)) is a stock control plane plus one
worker labelled `runtime=gvisor`; the `RuntimeClass` in
[`test/e2e/manifests/runtimeclass.yaml`](test/e2e/manifests/runtimeclass.yaml)
uses `handler: gvisor` so pods carrying `runtimeClassName: gvisor` really
execute under `runsc`. Honors `CLUSTER_NAME` and `KEEP_CLUSTER=1` for
iteration.

```bash
make kind-up          # idempotent; rebuilds the image only when missing
make e2e              # full scan -> plan -> apply -> rollback against the cluster
KEEP_CLUSTER=1 make e2e
make kind-down
```

## Cheatsheet

### Commands

| Command              | Purpose                                                       |
| -------------------- | ------------------------------------------------------------- |
| `agentmoat scan`     | Enumerate workloads and classify gVisor compatibility. RO.    |
| `agentmoat plan`     | Produce a deterministic `MigrationPlan` from a scan. RO.      |
| `agentmoat apply`    | Patch workloads per the plan. Default dry-run. Idempotent.    |
| `agentmoat rollback` | Reverse a previously applied plan. Default dry-run.           |
| `agentmoat verify`   | Confirm live pods match the plan's `runtimeClassName`. RO.    |
| `agentmoat explain`  | Embedded docs viewer; `explain namespace` / `explain workload` for deep scans. |
| `agentmoat version`  | Print binary version and git SHA.                             |

### Global flags

| Flag                       | Default          | Purpose                                                                  |
| -------------------------- | ---------------- | ------------------------------------------------------------------------ |
| `--output / -o`            | `table`          | `table`, `json`, or `yaml`. `json`/`yaml` follow `agentmoat.io/v1alpha1`.|
| `--kubeconfig`             | `$KUBECONFIG`    | Path to kubeconfig.                                                      |
| `--context`                | current-context  | kubeconfig context to use.                                               |
| `--namespace / -n`         | (all)            | Repeatable. Default: scan every namespace.                               |
| `--all-namespaces / -A`    | true if `-n` unset | Explicit all-namespaces flag.                                          |
| `--selector / -l`          | (none)           | Kubernetes label selector applied to every list call.                    |
| `--include-system`         | `false`          | Include `kube-system` and other `kube-*` namespaces.                     |
| `--rules`                  | (none)           | Path to YAML overriding rule severities or adding rules.                 |
| `--explain`                | `false`          | Inline educational notes in supported output formats.                    |

### Per-command flags

| Flag                       | Command      | Default     | Purpose                                                                 |
| -------------------------- | ------------ | ----------- | ----------------------------------------------------------------------- |
| `--scan`                   | `plan`       | (inline)    | Read a stored `ScanReport` from disk instead of scanning.               |
| `--include-review`         | `plan`       | `false`     | Also include `review`-class workloads in the plan.                      |
| `--runtime-class`          | `plan`       | `gvisor`    | RuntimeClass name to patch onto migrated workloads.                     |
| `--plan`                   | apply/rollback | required  | Path to a `MigrationPlan` JSON/YAML.                                    |
| `--dry-run`                | apply/rollback | `true`    | Compute patches but do not mutate the cluster.                          |
| `--no-events`              | apply/rollback | `false`   | Do not emit Kubernetes Events per mutation.                             |
| `--no-audit`               | apply/rollback | `false`   | Do not append to `~/.agentmoat/audit.jsonl`.                            |
| `--in-pod-probe`           | verify         | `false`   | Exec into a running pod and check dmesg/cmdline for gVisor markers.     |

### Output formats

| Format | Use case                                                                           |
| ------ | ---------------------------------------------------------------------------------- |
| `table`| Default human-readable view; per-workload row with verdict and top reason.         |
| `json` | Versioned, stable schema (`agentmoat.io/v1alpha1`). Pipe into `jq` or store as evidence. |
| `yaml` | Byte-identical to JSON after canonicalisation; convenient for review/diff.         |

## FAQ

**Why not just `kubectl patch` everything?** You can. agentmoat is the same
operation written down: it classifies (so you do not silently patch
incompatible workloads), it orders by risk (stateless and no-host-network
first), it is idempotent (the namespace annotation makes re-runs safe), it
keeps an audit trail, and it has a one-command rollback.

**Is it safe to run against production?** `scan` and `plan` are strictly
read-only. `apply` and `rollback` default to `--dry-run=true` and surface the
exact strategic-merge patch in `StepResult.patch` before any mutation. A
read-only kubeconfig is sufficient to run `scan` and `plan`.

**What about workloads that need raw sockets, eBPF, or GPU passthrough?**
The classifier marks them `incompatible`. The planner excludes them. The
`reasons[]` field on each `WorkloadResult` names the rule that fired and
links to the relevant gVisor doc, so the recommendation is concrete: either
leave the workload on `runc` (and put it on a non-gVisor node pool) or
adopt the gVisor option that supports it (`--net-raw`, `nvproxy`).

**Does agentmoat install anything in-cluster?** No CRDs, no webhooks, no
controllers. It patches pod templates and reads/writes one namespace
annotation (`agentmoat.io/plan-hash`). The only in-cluster prerequisite is
a `RuntimeClass` named `gvisor` (or whatever `--runtime-class` was passed)
that points at a real `runsc`-shipping node.

**Why a custom Packer AMI on EKS?** Bottlerocket does not ship `runsc`,
managed node groups have no gVisor switch, and AWS does not officially
support gVisor. The path of least resistance is to bring your own AL2023
node image with `runsc` baked in. `packer/eks-gvisor-al2023.pkr.hcl` is that
image.

**Why `systrap`, not KVM?** On EKS the instance kernels do not expose
`/dev/kvm` to user-space; on macOS-hosted kind, KVM is unavailable too. The
`systrap` platform works in both environments. Pinned in
[`kind/runsc.toml`](kind/runsc.toml) and `packer/files/runsc.toml`.

## Status and roadmap

Phase 0 (foundation), Phase 1 (read-only scan + classifier), and Phase 2
(planner + applier + rollback, with idempotency and audit) are committed.
`agentmoat scan`, `plan`, `apply`, and `rollback` are wired and exercised
end-to-end against a real gVisor kind cluster.

Roadmap:

- **Phase 3**: `agentmoat verify` (pod `runtimeClassName` check; optional
  `--in-pod-probe` for in-container confirmation) and `agentmoat explain`
  (embedded docs viewer). Both shipped.
- **Phase 5+**: EKS end-to-end recipe (CloudFormation/Terraform/Karpenter),
  additional Packer variants.

The in-tree design notes (`plan.md`, `plan-checklist.md`) are gitignored
locally; they hold the per-phase gates and rationale but are not part of the
public surface.

## Where to go next

- [Architecture](docs/architecture.md): the Go library at the core; CLI as a thin shell.
- [gVisor 101](docs/gvisor-101.md): Sentry, Gofer, platforms, and where the overhead lives.
- [RuntimeClass 101](docs/runtimeclass-101.md): one-page intro to the `RuntimeClass` API.
- [Threat model](docs/threat-model.md): what gVisor stops that `runc` does not, with CVE references.
- [Compatibility checklist](docs/compatibility-checklist.md): the full rule catalog and `--rules` override schema.
- [Exit codes](docs/exit-codes.md): the deterministic exit codes by command.
- [EKS deployment](docs/eks-deployment.md): the Packer + EKS recipe (stub today; tracked for Phase 5).
- [Kind quickstart](docs/kind-quickstart.md): bring up a local cluster with gVisor preinstalled.

## License

Apache License 2.0. See [LICENSE](LICENSE).
