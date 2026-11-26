# agentmoat

[![release](https://img.shields.io/github/v/release/0hardik1/agentmoat?label=release&sort=semver)](https://github.com/0hardik1/agentmoat/releases)
[![license](https://img.shields.io/github/license/0hardik1/agentmoat)](LICENSE)
[![CI](https://img.shields.io/github/actions/workflow/status/0hardik1/agentmoat/ci.yml?branch=main&label=CI&logo=github)](https://github.com/0hardik1/agentmoat/actions/workflows/ci.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/0hardik1/agentmoat?logo=go&label=Go)](go.mod)
[![Go Report Card](https://goreportcard.com/badge/github.com/0hardik1/agentmoat)](https://goreportcard.com/report/github.com/0hardik1/agentmoat)

**Move Kubernetes workloads from `runc` to gVisor (`runsc`), safely and reversibly.**

https://github.com/user-attachments/assets/2b04bc86-74a2-4936-a829-4653559eb954

agentmoat moves Kubernetes workloads from the default `runc` runtime to gVisor
(`runsc`), the user-space kernel that defends against the kernel-exploit step of
a container-escape chain. It scans a cluster, classifies every workload by
gVisor compatibility, generates a deterministic migration plan with a stable
hash, applies it via `RuntimeClass`, and rolls it back on demand. On EKS
specifically there is no managed switch: Bottlerocket does not ship `runsc`,
managed node groups have no gVisor toggle, and AWS does not officially support
gVisor. agentmoat fills that gap.

The threat model and CVE backdrop live in [`docs/threat-model.md`](docs/threat-model.md).

## Install

Prebuilt binaries for linux and darwin (amd64 and arm64) and a checksums file
are attached to each
[GitHub release](https://github.com/0hardik1/agentmoat/releases). Download the
archive for your platform, extract it, and put `agentmoat` on your `PATH`.

With Go 1.26 or newer:

```bash
go install github.com/0hardik1/agentmoat/cmd/agentmoat@latest
go install github.com/0hardik1/agentmoat/cmd/agentmoat-mcp@latest
```

`@latest` installs the newest tagged release. Use `@main` to build the tip of
`main`.

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

Against manifests, with no cluster and no kubeconfig:

```bash
./bin/agentmoat scan -f ./k8s                               # a file or a directory
helm template ./chart | ./bin/agentmoat scan -f -           # rendered Helm output
kustomize build ./overlays/prod | ./bin/agentmoat scan -f - # rendered Kustomize output
./bin/agentmoat scan -f ./k8s --fail-on sandboxed           # the gate for a pull-request check
```

`scan -f` reads the six workload kinds and any other kind with a pod template
(an Argo `Rollout`). It lists what it did not classify. It reads each object as
written: render Helm templates and Kustomize overlays first, as above.
`--fail-on sandboxed` exits 2 only when a workload that asks for the gVisor
RuntimeClass is incompatible, so a CNI DaemonSet that will never move does not
fail the check. See [`docs/exit-codes.md`](docs/exit-codes.md).

In a pipeline, write the report where the reviewer reads it:

```bash
./bin/agentmoat scan -f ./k8s --fail-on sandboxed -o markdown >> "$GITHUB_STEP_SUMMARY"
./bin/agentmoat scan -f ./k8s --fail-on sandboxed -o sarif > agentmoat.sarif
```

The Markdown report starts with the result of the gate. A SARIF result has the
level `error` only when it makes the gate fail. See
[`docs/output-formats.md`](docs/output-formats.md).

On GitHub Actions, the action does the same in two lines (from release
`v0.3.0`). It scans, writes the report to the job summary, and puts an error
annotation on each workload that makes the gate fail. It reads no cluster and
needs no secret:

```yaml
- uses: actions/checkout@v7
- uses: 0hardik1/agentmoat@v0
  with:
    path: k8s
```

[`docs/ci-cd.md`](docs/ci-cd.md) has the inputs, the Kustomize and Helm
renders, and the same check for other CI systems.
[`examples/ci/`](examples/ci/) has the files to copy.

To move workloads through your repository, `patch` writes the change into
the manifest files. It reads no cluster:

```bash
./bin/agentmoat patch -f ./k8s                     # dry-run: show the diff
./bin/agentmoat patch -f ./k8s --dry-run=false     # add runtimeClassName to the files
./bin/agentmoat scan -f ./k8s --fail-on sandboxed  # check the result
```

`patch` selects the workloads that are compatible and have no RuntimeClass,
and adds one line (`runtimeClassName: gvisor`) to the pod spec of each.
Nothing else in a file changes: comments, the order of keys, and quotes stay.
Review the diff in a pull request, let your pipeline deploy it, and use
`git revert` as the rollback. See
[`docs/ci-cd.md`](docs/ci-cd.md#3-the-move-through-the-repository).

After the deploy, `verify -f` checks the cluster against the same files. It
needs no plan: the files say which workloads must run in the sandbox.

```bash
./bin/agentmoat verify -f ./k8s -n shop --wait 5m
```

It checks each workload of the files that asks for the RuntimeClass, and
exits `4` when the cluster shows that one does not run with it. See
[Check the result of a change that came through the source](docs/ci-cd.md#check-the-result-of-a-change-that-came-through-the-source)
for the input rules and the kinds.

With Kustomize, an edit of a base changes each overlay that uses the base.
To move one overlay first, `--kustomize <dir>` edits no source file. It
writes the change into the directory as a Kustomize component (one patch
file for each workload), and you add the directory to `components:` of the
overlay:

```bash
./bin/agentmoat patch -f ./base --kustomize ./overlays/staging/gvisor --dry-run=false
kustomize build ./overlays/staging | ./bin/agentmoat scan -f - --fail-on sandboxed
```

See [Kustomize](docs/ci-cd.md#kustomize) for the rule about the input.

A pipeline that moves the workloads runs `verify` as the next command after
`apply`, with a wait time, and rolls back when it fails:

```bash
./bin/agentmoat apply --plan plan.json --dry-run=false
./bin/agentmoat verify --plan plan.json --wait 3m \
  || ./bin/agentmoat rollback --plan plan.json --dry-run=false
```

`verify --wait` looks again until every workload has the RuntimeClass on all
its pods and those pods stay Ready. A pod that has the class and does not
start in gVisor fails the check, with the reason in the report. `rollback`
with the same plan also undoes an apply that failed on some steps (exit 3).
In a CI system, make the rollback its own step with a condition:
[`docs/ci-cd.md`](docs/ci-cd.md#2-the-migration-pipeline) has the pipeline
(plan, approval, apply, verify, rollback) and what the rollback does for each
way that a run can end.

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
  `ScanReport`. It also records `metadata.clusterFacts`: whether the
  RuntimeClass exists, how many Ready nodes its `nodeSelector` matches, and
  which GPU cards and drivers the GPU nodes carry. The classifier reads
  those facts too: a GPU workload on a card gVisor's `nvproxy` supports,
  with a driver the installed `runsc` lists, is `compatible`; one on an
  unsupported card or a MIG-sliced node is `incompatible`.
- **`preflight`** is read-only too. It answers "can a pod that requests
  this RuntimeClass schedule at all?": the RuntimeClass exists, its
  `scheduling.nodeSelector` matches at least one Ready node whose taints it
  tolerates, and the candidate nodes are not EKS Auto Mode or Bottlerocket
  instances (AWS-managed, immutable, no `runsc`). Findings have stable IDs
  and a remediation each; a blocking finding exits `5`. See
  [`docs/preflight.md`](docs/preflight.md).
- **`probe nvproxy`** creates one short-lived pod on a gVisor node, reads
  `runsc --version` and `runsc nvproxy list-supported-drivers` from the
  host's `runsc` binary, and deletes the pod (it runs the pod a second time
  when that `runsc` needs its sidecar binaries). That driver list exists
  nowhere else, and it decides whether GPU workloads can move. Dry-run by
  default like `apply`; save the report and hand it to `scan --facts`. See
  [`docs/gpu-nvproxy.md`](docs/gpu-nvproxy.md).
- **`probe seccomp`** runs each migrated workload once, as a short trial
  pod on gVisor with the workload's own seccomp profile, and reports the
  syscalls that profile refused inside the sandbox. From gVisor
  `20261123.1`, runsc enforces the pod's OCI seccomp filter by default, so a
  `RuntimeDefault` workload can now get `EPERM` on a call that used to work.
  Dry-run by default. See [`docs/preflight.md`](docs/preflight.md#seccomp-probe).
- **`plan`** is a pure function from a `ScanReport` to a `MigrationPlan`.
  Same scan in, same plan and same SHA-256 `planHash` out. The plan orders
  steps by risk (stateless first, no host-network first, etc.) and excludes
  any workload classified `incompatible`. It also excludes standalone Pods
  and Jobs with an actionable reason: the Kubernetes API rejects in-place
  `runtimeClassName` patches for both (a running Pod's spec and a Job's
  template are immutable), so those workloads must be re-created instead.
  A workload that already has a RuntimeClass gets no step: the plan lists
  what `apply` will change, so that `rollback` undoes only that. In a
  pipeline, the plan of each run then has the new workloads, and not the
  ones that an earlier run moved.
  When a workload in the plan has the marker of a deploy tool (Argo CD,
  Flux, Helm), the plan has the warning `source-managed`: `apply` changes
  the live object only, so the source that the tool deploys from needs the
  change too (`patch` writes it into manifest files). `scan` records the
  marker as `managedBy` on the workload.
- **`patch`** makes the same move as a change to manifest files, for a
  repository that a pipeline or a deploy tool deploys from. It reads the
  files as `scan -f` does, selects the workloads that `plan` selects
  (compatible, no RuntimeClass; `--include-review` adds the ones that need
  review), and adds `runtimeClassName` to the pod spec of each: one added
  line, and no other change to the file. A Pod and a Job can change here
  too, because a file has no immutable field. It checks each edit before it
  uses it (the new text must decode to the old objects plus the class), and
  it lists what it does not edit (JSON, flow-style YAML, a pod spec behind
  a YAML anchor) with the reason. It reads no cluster and defaults to
  `--dry-run=true`, which prints the diff. A second run changes nothing.
  With `--kustomize <dir>` it edits no source file: it writes one strategic
  merge patch file for each selected workload, and a `kustomization.yaml` of
  kind `Component` that lists them, so that one Kustomize overlay can move
  before the others. A run only adds files and entries there, and it refuses
  a directory that has files of a person.
- **`apply`** is the only stage that mutates the cluster. It runs the
  preflight first and refuses to touch anything when the cluster cannot host
  the plan (exit `5`, every step `skipped`, `--skip-preflight` to override).
  Then it patches each pod template with `runtimeClassName` only: placement
  comes from the RuntimeClass's `scheduling` block, which admission merges
  into every pod (`plan --add-toleration` also patches the
  `runtime=gvisor:NoSchedule` toleration in, for clusters whose RuntimeClass
  lacks `scheduling.tolerations`). It records itself on the namespace:
  `agentmoat.io/apply-started` before its first patch there, and
  `agentmoat.io/plan-hash` when every step is done. It emits a Kubernetes
  Event for each step it actually mutates, and appends an audit line for
  every step (dry-run included) to `~/.agentmoat/audit.jsonl`. It defaults
  to `--dry-run=true`. Re-running an applied plan reports every step as
  `already-applied` and exits 0 via the namespace annotation.
- **`rollback`** walks the same plan in reverse, removes `runtimeClassName`,
  and clears the namespace annotations. It also undoes an apply that did not
  finish (a step failed, or the process was stopped): `apply-started` says
  that the plan was at work in the namespace, and rollback then un-patches
  the workloads that have the class of the plan. In a namespace with no
  annotation of the plan it changes nothing. The annotations hold one plan
  for a namespace: a later apply of another plan replaces them, and the
  older plan must be applied again before it can be rolled back. A
  toleration added by an opt-in apply is deliberately left in place (a
  toleration without a matching taint is harmless, and removing a specific
  toleration by JSON Patch index is fragile). Rollback runs no preflight:
  moving pods back to runc needs no gVisor node.
- **`verify`** is read-only. For each step of the plan (or, with `-f`, for
  each workload of the manifest files that asks for the RuntimeClass) it
  checks that every live pod has the `runtimeClassName` and runs on a node
  that the RuntimeClass selects; `--in-pod-probe` also looks for gVisor markers in
  one pod. One look directly after `apply` finds a rollout in progress and
  reports a mismatch, so a pipeline passes `--wait <duration>`: verify then
  looks again until every step is ok or the time ends. With a wait, a step
  is ok only when its pods are Ready and stay Ready for 10 seconds with no
  restart, so exit `4` after a wait means that the workloads did not come
  up in gVisor. A workload with no pods now (`replicas: 0`, a CronJob
  between two runs) is checked on its pod template. A Job that failed is a
  mismatch with `--wait`.

The manual alternative is a sequence of `kubectl get` to enumerate, hand
classification against the gVisor docs, per-controller `kubectl patch` for
every Deployment/StatefulSet/DaemonSet, and a separate audit trail you build
yourself. agentmoat does the classification, orders the mutations, makes them
idempotent, records them, and undoes them.

## Compatibility checks

15 stable rule IDs across three severities. Rule IDs are a public surface:
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
| `gpu-passthrough`   | warn     | `nvidia.com/*` resource request or limit; refined to `info` or `error` from the GPU nodes' card, driver, and MIG facts |
| `fuse-mount`        | warn     | CSI driver name containing `fuse`, or `AGENTMOAT_USES_FUSE=true`       |
| `io-uring`          | warn     | Annotation `agentmoat.io/uses-iouring=true`                            |
| `perf-events`       | warn     | `CAP_PERFMON` or `CAP_SYS_ADMIN`                                       |
| `systemd-init`      | warn     | systemd as PID 1 (`command`/`args` of `/sbin/init` or `/usr/lib/systemd/systemd`, a UBI init image, or `agentmoat.io/runs-systemd=true`); refined to `error` when the probed runsc predates `release-20260831.0` |
| `network-throughput`| info     | Image hint: nginx, envoy, haproxy, traefik (expect 20-40% overhead)    |
| `seccomp-unconfined`| info     | A container's seccompProfile is `Unconfined`; from gVisor 20261123.1 that opts it out of the OCI filter runsc enforces in the sandbox |
| `syscall-heavy`     | info     | Image hint: redis, memcached (expect higher latency)                   |

Any `error` rule firing makes the workload `incompatible` and excludes it from
the plan. `warn` makes it `review`; the planner only includes `review`
workloads when `--include-review` is passed. `info` is purely advisory and
never blocks.

Override rule severities without recompiling via `--rules <file.yaml>`
(unknown rule IDs are warned about and skipped; adding brand-new rules
requires a code change). See
[`docs/compatibility-checklist.md`](docs/compatibility-checklist.md).

## Sample output

Shapes come straight from [`internal/schema/types.go`](internal/schema/types.go);
elided fields are marked `...`.

`agentmoat scan --output json` (one `WorkloadResult` from `.spec.workloads`):

```json
{
  "kind": "Deployment",
  "namespace": "edge",
  "name": "frontdoor",
  "compatibility": "compatible",
  "reasons": [
    {
      "ruleId": "network-throughput",
      "severity": "info",
      "description": "Workload appears network-throughput-bound (image hint: nginx, envoy, haproxy, traefik); expect ~20-40% throughput overhead under gVisor's sandbox network stack.",
      "remediationUrl": "https://gvisor.dev/docs/architecture_guide/performance/"
    }
  ],
  "recommendation": "Safe to migrate: set spec.runtimeClassName: gvisor on this workload.",
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
    "options": {"runtimeClassName": "gvisor"},
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
        "patch": "{\"spec\":{\"template\":{\"spec\":{\"runtimeClassName\":\"gvisor\"}}}}"
      }
    ]
  }
}
```

## Operational guarantees

- **Read-only by default.** `scan`, `preflight`, and `plan` never mutate.
  They are safe to run against production from a CI job or a read-only
  kubeconfig.
- **Fails loudly when the cluster cannot host the plan.** `apply` runs the
  preflight before its first step, dry-run included. No RuntimeClass, a
  RuntimeClass that steers pods nowhere, no Ready `runsc` node, or an EKS
  Auto Mode cluster all block the apply with exit `5` and nothing mutated,
  instead of producing pods that sit Pending forever.
- **Dry-run by default.** `apply` and `rollback` default to `--dry-run=true`:
  the patches are computed and surfaced in the `StepResult.patch` field, but
  nothing is sent to the API server. Mutating requires explicit
  `--dry-run=false`. `probe nvproxy` and `probe seccomp`, the only other
  verbs that create anything (short-lived pods, deleted afterwards), follow
  the same rule.
- **Idempotent.** Every `apply` writes the plan hash to the affected
  namespace as `agentmoat.io/plan-hash`. Re-running the same plan against
  the same cluster reports every step as `already-applied` and exits 0.
- **Reversible, also in the middle.** `apply` writes
  `agentmoat.io/apply-started` on a namespace before its first patch there,
  and patches nothing when that write fails. So the cluster always has a
  record that a plan changed a namespace, and `rollback` with the same plan
  can undo an apply that failed on one step or was stopped. A plan with no
  `planHash` cannot be recorded, so `apply` and `rollback` refuse it.
  [`docs/exit-codes.md`](docs/exit-codes.md#rollback-after-each-way-that-apply-can-end)
  has the table of what `rollback` does for each way that an `apply` can
  end, and its limits.
- **Auditable.** Every step appends one JSON line to
  `~/.agentmoat/audit.jsonl` (disable with `--no-audit`), including dry-run
  steps, which carry `dryRun: true`. Each step that actually mutates also
  emits one Kubernetes Event on the patched object (disable with
  `--no-events`).
- **Deterministic exit codes.** CI scripts can branch on them:

  | Code | Meaning                                                   |
  | ---- | --------------------------------------------------------- |
  | 0    | Success. Cluster matches requested state.                 |
  | 1    | Generic error (kubeconfig, network, malformed plan, etc). |
  | 2    | `scan` / `explain namespace` / `explain workload`: at least one `incompatible` workload. |
  | 3    | `apply` or `rollback`: partial outcome; idempotent re-run is safe. |
  | 4    | `verify`: `runtimeClassName` mismatch, hosting node outside the RuntimeClass `nodeSelector`, in-pod probe did not find gVisor, and/or (with `--wait`) pods that did not stay Ready before the time ended. With `-f`, a workload that verify could not check is exit `1`, not `4`. |
  | 5    | `preflight` / `probe` / `apply`: the cluster cannot host the RuntimeClass; `apply` mutated nothing. |

  Full table in [`docs/exit-codes.md`](docs/exit-codes.md).

## EKS via Packer

[`packer/eks-gvisor-al2023.pkr.hcl`](packer/eks-gvisor-al2023.pkr.hcl) builds
an EKS-optimized AL2023 AMI with `runsc` and the containerd v2 shim
preinstalled, the containerd drop-in pre-staged, and the systrap platform
pinned (KVM is unavailable on EKS instances). The gVisor release is pinned
and refreshed weekly by an automated PR; see
[`docs/gvisor-version.md`](docs/gvisor-version.md).

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
make kind-up          # idempotent; rebuilds the image when missing or built for another gVisor release
make e2e              # full scan -> plan -> apply -> rollback against the cluster
KEEP_CLUSTER=1 make e2e
make kind-down
```

## Cheatsheet

### Commands

| Command              | Purpose                                                       |
| -------------------- | ------------------------------------------------------------- |
| `agentmoat scan`     | Classify workloads for gVisor compatibility: a cluster, or manifests with `-f` (no cluster). RO. |
| `agentmoat preflight`| Check that the RuntimeClass steers pods onto a Ready `runsc` node. RO. Exit 5 when not. |
| `agentmoat probe nvproxy` | Read the nvproxy supported-driver list from `runsc` on a gVisor node via a one-shot pod. Default dry-run. |
| `agentmoat probe seccomp` | Run each migrated workload once on gVisor with its own seccomp profile and report the syscalls the filter refused. Default dry-run. |
| `agentmoat plan`     | Produce a deterministic `MigrationPlan` from a scan. RO.      |
| `agentmoat patch`    | Add `runtimeClassName` to manifest files (`-f`), for a move through a pull request. With `--kustomize <dir>`: write the change as a Kustomize component, and edit no source file. No cluster. Default dry-run (prints the diff). |
| `agentmoat apply`    | Patch workloads per the plan. Default dry-run. Idempotent.    |
| `agentmoat rollback` | Reverse a previously applied plan. Default dry-run.           |
| `agentmoat verify`   | Confirm live pods match the `runtimeClassName` of the plan (`--plan`) or of manifest files (`-f`). With `--wait`, wait for the rollout and for Ready pods. RO. |
| `agentmoat explain`  | Embedded docs viewer; `explain namespace` / `explain workload` for deep scans. |
| `agentmoat version`  | Print binary version and git SHA.                             |

### Global flags

| Flag                       | Default          | Purpose                                                                  |
| -------------------------- | ---------------- | ------------------------------------------------------------------------ |
| `--output / -o`            | `table`          | `table`, `json`, `yaml`, `markdown`, or `sarif`. `json`/`yaml` follow `agentmoat.io/v1alpha1`. See [Output formats](#output-formats). |
| `--kubeconfig`             | `$KUBECONFIG`    | Path to kubeconfig.                                                      |
| `--context`                | current-context  | kubeconfig context to use.                                               |
| `--namespace / -n`         | (all)            | Repeatable. Default: scan every namespace. With `scan -f`, `patch`, and `verify -f`: one value, the namespace for objects that name none (not a filter). |
| `--all-namespaces / -A`    | true if `-n` unset | Explicit all-namespaces flag.                                          |
| `--selector / -l`          | (none)           | Kubernetes label selector applied to every list call.                    |
| `--include-system`         | `false`          | Include `kube-system` and other `kube-*` namespaces.                     |
| `--rules`                  | (none)           | Path to YAML overriding built-in rule severities.                        |

### Per-command flags

| Flag                       | Command      | Default     | Purpose                                                                 |
| -------------------------- | ------------ | ----------- | ----------------------------------------------------------------------- |
| `--scan`                   | `plan`       | (inline)    | Read a stored `ScanReport` from disk instead of scanning.               |
| `--include-review`         | plan/patch   | `false`     | Also include `review`-class workloads in the plan or the patch.         |
| `--runtime-class`          | plan/patch   | `gvisor`    | RuntimeClass name to patch onto migrated workloads, or to write into the files. |
| `--add-toleration`         | `plan`       | `false`     | Also patch the `runtime=gvisor:NoSchedule` toleration into each pod template. Changes the plan hash. |
| `--filename / -f`          | `scan`       | (cluster)   | Scan manifests and no cluster: a file, a directory, or `-` for stdin. Repeatable. |
| `--filename / -f`          | `patch`      | required    | The manifests to edit: a file or a directory. Repeatable. With `--kustomize`, also `-` for stdin. |
| `--kustomize`              | `patch`      | (edit the files) | A directory. Write the change there as a Kustomize component (patch files and their list), and edit no source file. |
| `--dry-run`                | `patch`      | `true`      | Show the diff and write no file.                                        |
| `--fail-on`                | `scan`       | `incompatible` | When to exit 2: `incompatible`, `sandboxed` (only workloads that ask for the gVisor RuntimeClass), or `none`. |
| `--runtime-class`          | scan/preflight | `gvisor`  | RuntimeClass to inspect for cluster facts / the preflight. For `scan`, also the class that `--fail-on sandboxed` looks for. |
| `--no-cluster-facts`       | `scan`       | `false`     | Skip the node and RuntimeClass reads; omit `metadata.clusterFacts`.     |
| `--facts`                  | scan/plan/patch/explain | (live) | Load cluster facts from a saved `PreflightReport` (`probe nvproxy` output) or `ScanReport` instead of reading the cluster. |
| `--dry-run`                | `probe nvproxy` | `true`   | Describe the probe pod without creating it.                             |
| `--probe-namespace`        | `probe nvproxy` | `default` | Namespace for the probe pod (must allow `hostPath` under PSA; see `deploy/nvproxy-probe.yaml`). |
| `--image` / `--runsc-path` / `--timeout` | `probe nvproxy` | `busybox:1.36.1` / `/usr/local/bin/runsc` / `2m` | Probe pod image, host path of `runsc`, wait budget. |
| `--skip-preflight`         | `apply`      | `false`     | Bypass the cluster preflight gate (not recommended).                    |
| `--plan`                   | apply/rollback | required  | Path to a `MigrationPlan` JSON/YAML.                                    |
| `--plan`                   | verify         | this or `-f` | Path to the `MigrationPlan` whose steps to check.                    |
| `--filename / -f`          | verify         | this or `--plan` | Check the workloads of these manifests that ask for the RuntimeClass: a file, a directory, or `-` for stdin. Repeatable. The input must be rendered; `-n` is the namespace of the objects that name none. |
| `--runtime-class`          | verify         | `gvisor`  | With `-f`: the RuntimeClass name to look for.                           |
| `--dry-run`                | apply/rollback | `true`    | Compute patches but do not mutate the cluster.                          |
| `--no-events`              | apply/rollback | `false`   | Do not emit Kubernetes Events per mutation.                             |
| `--no-audit`               | apply/rollback | `false`   | Do not append to `~/.agentmoat/audit.jsonl`.                            |
| `--in-pod-probe`           | verify         | `false`   | Exec into a running pod and check dmesg/cmdline for gVisor markers.     |
| `--wait`                   | verify         | `0`       | Look again until every step is ok and its pods stay Ready, for at most this long (`3m`). `0` looks one time and does not read readiness. |

### Output formats

| Format | Use case                                                                           |
| ------ | ---------------------------------------------------------------------------------- |
| `table`| Default human-readable view; per-workload row with runtime class, verdict, and top reasons. |
| `json` | Versioned, stable schema (`agentmoat.io/v1alpha1`). Pipe into `jq` or store as evidence. |
| `yaml` | Byte-identical to JSON after canonicalisation; convenient for review/diff.         |
| `markdown` (or `md`) | A CI job summary, a pull-request comment, a merge-request note. For `scan`, the first line is the result of the `--fail-on` gate. For `patch`, it has the diff. Not for `explain`. |
| `sarif` | SARIF 2.1.0 for a code scanning service. `scan` only. A result is an `error` only when it makes the gate fail. |

A command refuses a format that it cannot write before it does any work. The
details (the gate line, the SARIF levels and locations, the limits) are in
[`docs/output-formats.md`](docs/output-formats.md).

## FAQ

**Why not just `kubectl patch` everything?** You can. agentmoat is the same
operation written down: it classifies (so you do not silently patch
incompatible workloads), it orders by risk (stateless and no-host-network
first), it is idempotent (the namespace annotation makes re-runs safe), it
keeps an audit trail, and it has a one-command rollback.

**Is it safe to run against production?** `scan` and `plan` are strictly
read-only. `apply` and `rollback` default to `--dry-run=true` and surface the
exact strategic-merge patch in `StepResult.patch` before any mutation. A
read-only kubeconfig is sufficient to run `scan`, `preflight`, and `plan`:
ready-to-bind RBAC for both modes ships under [`deploy/`](deploy/) as
`clusterrole-readonly.yaml` (scan / preflight / plan / verify) and
`clusterrole-apply.yaml` (apply / rollback). The read-only role includes
`get`/`list` on nodes and RuntimeClasses for the preflight; without them
`scan` still works and simply omits `metadata.clusterFacts`.

**How do I know the cluster can host gVisor pods at all?** Run
`agentmoat preflight`. It checks that the RuntimeClass exists, that its
`scheduling.nodeSelector` matches at least one Ready node whose taints it
tolerates, and that those nodes are not EKS Auto Mode or Bottlerocket
instances. `apply` runs the same check first and refuses to patch anything
(exit `5`) when it fails, so a plan cannot produce pods that never schedule.

**Does agentmoat work on EKS Auto Mode?** Not on Auto Mode's own nodes. AWS
owns their Bottlerocket image and container runtime; software cannot be
installed on them, so `runsc` can never be present. `preflight` reports
`eks-auto-mode-nodes` and blocks `apply`. The supported path is a
self-managed or Karpenter node group built from the agentmoat AL2023 AMI
next to the Auto Mode pool; see [`docs/eks-deployment.md`](docs/eks-deployment.md).

**What about workloads that need raw sockets or eBPF?**
The classifier marks them `incompatible`. The planner excludes them. The
`reasons[]` field on each `WorkloadResult` names the rule that fired and
links to the relevant gVisor doc, so the recommendation is concrete: either
leave the workload on `runc` (and put it on a non-gVisor node pool) or
adopt the gVisor option that supports it (`--net-raw`).

**And GPU workloads?** gVisor runs CUDA through `nvproxy`, which supports
T4, A100, A10G, L4, and H100 cards, needs an exact host-driver match with
the installed `runsc`, and does not support MIG. `scan` reads the GPU nodes'
card and driver from GPU Feature Discovery labels, and `agentmoat probe
nvproxy --dry-run=false` reads the driver list from `runsc` itself through a
one-shot pod. With both, a GPU workload on supported hardware is
`compatible` and lands in the plan; one on a V100, a MIG slice, or an
unlisted driver is `incompatible`. See [`docs/gpu-nvproxy.md`](docs/gpu-nvproxy.md).

**Does agentmoat install anything in-cluster?** No CRDs, no webhooks, no
controllers. It patches pod templates and reads/writes two namespace
annotations (`agentmoat.io/plan-hash` and `agentmoat.io/apply-started`). The
only in-cluster prerequisite is
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

Phase 0 (foundation), Phase 1 (read-only scan + classifier), Phase 2
(planner + applier + rollback, with idempotency and audit), Phase 3
(`agentmoat verify` with optional `--in-pod-probe`, plus `agentmoat
explain`), and Phase 4 (the `agentmoat-mcp` MCP server: 11 tools over
stdio, see [`docs/mcp-integration.md`](docs/mcp-integration.md)) are all
shipped and exercised end-to-end against a real gVisor kind cluster.
`agentmoat preflight`, the apply gate, `agentmoat probe nvproxy`, the scan
of manifests (`scan -f`), and `agentmoat patch` landed after v0.1.0.

Roadmap:

- **Phase 5+**: EKS end-to-end recipe (CloudFormation/Terraform/Karpenter),
  additional Packer variants.

## Where to go next

- [CI/CD](docs/ci-cd.md): the pull request check (the GitHub Action, other CI systems), the migration pipeline, and the move through the repository with `patch`.
- [Architecture](docs/architecture.md): the Go library at the core; CLI as a thin shell.
- [gVisor 101](docs/gvisor-101.md): Sentry, Gofer, platforms, and where the overhead lives.
- [RuntimeClass 101](docs/runtimeclass-101.md): one-page intro to the `RuntimeClass` API.
- [Preflight](docs/preflight.md): the finding IDs, what `apply` does with them, and EKS Auto Mode.
- [GPU workloads and nvproxy](docs/gpu-nvproxy.md): supported cards, the driver probe, and how `gpu-passthrough` is decided.
- [Threat model](docs/threat-model.md): what gVisor stops that `runc` does not, with CVE references.
- [Compatibility checklist](docs/compatibility-checklist.md): the full rule catalog and `--rules` override schema.
- [Exit codes](docs/exit-codes.md): the deterministic exit codes by command.
- [Output formats](docs/output-formats.md): table, JSON, YAML, Markdown for a job summary, and SARIF for code scanning.
- [EKS deployment](docs/eks-deployment.md): the Packer + EKS recipe (stub today; tracked for Phase 5).
- [Kind quickstart](docs/kind-quickstart.md): bring up a local cluster with gVisor preinstalled (doc is a stub today; `make kind-up` is the working path).

## License

Apache License 2.0. See [LICENSE](LICENSE).
