# kind quickstart

> Status: STUB. Filled in alongside Phase 1's e2e suite.

## Planned table of contents

1. Why kind (local Linux VM under Docker Desktop, easy CI integration)
2. Building the custom gVisor-enabled kind node image (`kind/Dockerfile.gvisor-node`)
3. Bringing up the cluster (`scripts/kind-up.sh`)
4. Applying the RuntimeClass (`kubectl apply -f deploy/runtimeclass.yaml`)
5. Labelling the kind node `runtime=gvisor`
6. Running `agentmoat scan` against the local cluster
7. Tearing down (`scripts/kind-down.sh`)

## Known caveats (to be expanded)

- gVisor inside a Docker container has known seccomp friction. The kind
  config will grant `SYS_PTRACE` and turn on `unconfined` seccomp on the
  node container only (not the workloads).
- On macOS, kind runs inside the Linux VM that Docker Desktop manages.
  `systrap` works there; KVM does not. (This matches the EKS constraint.)

For Phase 0 + Phase 1 (today), there is no kind tooling in the repo yet.
You can still run `agentmoat scan` against any reachable kubeconfig.
