# kind quickstart

`make e2e` runs the whole pipeline against a real gVisor runtime inside a
local [kind](https://kind.sigs.k8s.io/) cluster. This page is the manual
version of what that target does, for when you want to poke at the cluster
yourself.

## Prerequisites

- Docker (Docker Desktop on macOS is fine; kind runs inside its Linux VM).
- `kind` and `kubectl` on your PATH.
- `jq` (the e2e assertions use it).

## 1. Build the gVisor worker image

```bash
make kind-build
```

This builds [`kind/Dockerfile.gvisor-node`](../kind/Dockerfile.gvisor-node):
the upstream `kindest/node` image plus `/usr/local/bin/runsc`, the containerd
v2 shim, and `/etc/containerd/runsc.toml` (platform pinned to `systrap`, since
KVM is unavailable inside Docker). The gVisor release is pinned; see
[`gvisor-version.md`](gvisor-version.md). The build is skipped when an image
with the same tag and the same baked gVisor version already exists.

## 2. Create the cluster

```bash
make kind-up
```

[`kind/cluster.yaml`](../kind/cluster.yaml) describes a stock control-plane
plus one worker running the image above. The worker carries the label
`runtime=gvisor`, and `containerdConfigPatches` registers a runtime handler
named `gvisor` on every node. The target is idempotent: an existing cluster
named `agentmoat-e2e` is reused, even after a pin bump, so run
`make kind-down` first when you need the new node image.

## 3. Install the RuntimeClass and some workloads

```bash
kubectl apply -f test/e2e/manifests/runtimeclass.yaml
kubectl apply -f test/e2e/manifests/workloads.yaml
```

The e2e RuntimeClass uses `handler: gvisor` and a `scheduling.nodeSelector` of
`runtime: gvisor`, so any pod that carries `runtimeClassName: gvisor` is
admitted with that selector and lands on the worker.

## 4. Run the pipeline

```bash
./bin/agentmoat scan -n agentmoat-e2e
./bin/agentmoat plan -n agentmoat-e2e -o json > plan.json
./bin/agentmoat apply --plan plan.json                 # dry-run by default
./bin/agentmoat apply --plan plan.json --dry-run=false
./bin/agentmoat verify --plan plan.json --in-pod-probe
./bin/agentmoat rollback --plan plan.json --dry-run=false
```

Or let the harness do all of it, with assertions:

```bash
make e2e                # creates, tests, and deletes the cluster
KEEP_CLUSTER=1 make e2e # keep it around for inspection
```

## 5. Tear down

```bash
make kind-down
```

## Caveats

- kind launches node containers with `--privileged`, which is what lets
  `runsc` use the systrap platform without extra flags.
- The control-plane node has the `gvisor` handler registered but no `runsc`
  binary. The RuntimeClass nodeSelector is what keeps gVisor pods off it.
- The worker is not tainted, so pre-migration workloads (which carry no
  toleration) still schedule. On a real cluster you may taint gVisor nodes;
  put the matching toleration in `RuntimeClass.scheduling.tolerations`.
