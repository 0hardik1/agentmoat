# Example: agentic workloads

Two Deployments that model the case agentmoat is built for: code that runs
untrusted or model-generated logic, where a container escape is the risk you
actually care about. Use them to watch `scan -> plan -> apply` end to end
without inventing your own manifests.

| File | Workload | agentmoat verdict |
|------|----------|-------------------|
| `01-agent-runner.yaml` | Stateless agent runner (executes tool calls / model-generated code) | **Compatible** -> included in the plan |
| `02-raw-socket-tool.yaml` | Network-diagnostic sidecar needing `CAP_NET_RAW` | **Incompatible** (`raw-socket`) -> excluded |

The point: agentmoat migrates the workload that *benefits* from gVisor
(`agent-runner`) and refuses to silently migrate the one that would break under
it (`raw-socket-tool`), telling you why and what to do instead.

## Run it

```bash
kubectl apply -f examples/agentic-workload/

# Classify (table). The raw-socket tool shows up incompatible; scan exits 2.
agentmoat scan -n agentic-demo

# Deterministic plan: includes agent-runner, excludes raw-socket-tool.
agentmoat scan -n agentic-demo --output json > scan.json
agentmoat plan --scan scan.json --output json > plan.json

# Dry-run apply (the default): prints the strategic-merge patch, mutates nothing.
agentmoat apply --plan plan.json

# When you are ready, mutate for real (requires deploy/clusterrole-apply.yaml RBAC):
#   agentmoat apply --plan plan.json --dry-run=false
```

## Clean up

```bash
kubectl delete -f examples/agentic-workload/
```
