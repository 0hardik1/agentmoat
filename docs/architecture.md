# agentmoat architecture

```
+-----------------------------------------------------------------+
|                  User / Operator / AI Agent                     |
+---------------------+-----------------+-------------------------+
                      | CLI (humans)    | MCP stdio (agents)
            +---------v--------+ +------v------------+
            |  agentmoat       | |  agentmoat-mcp    |
            |  (cobra)         | |  (MCP server)     |
            +---------+--------+ +------+------------+
                      |                 |
                      +-------+---------+
                              |
                    +---------v----------+
                    |  pkg/agentmoat     |   <-- the Go library, importable
                    |    scanner/        |
                    |    classifier/     |
                    |    preflight/      |
                    |    probe/          |
                    |    planner/        |
                    |    applier/        |
                    |    verifier/       |
                    |    explainer/      |
                    +---------+----------+
                              | client-go
                    +---------v----------+
                    |  Kubernetes API    |   <-- kind locally, EKS in target
                    +--------------------+
```

## Why a Go library at the core

Both the CLI and the MCP server are thin shells over `pkg/agentmoat`. The
library exposes `Scan`, `Preflight`, `Plan`, `Apply`, `Verify`, `Rollback`,
and `Explain`. Anyone who wants to embed agentmoat in their own operator,
controller, or SDK can `import` the package and call those functions
directly. No shell-out, no subprocess plumbing.

## Why both a CLI and an MCP server

Two distinct operators consume agentmoat:

1. **Humans**, who type commands at a terminal and read tables.
2. **AI agents**, which exchange JSON-RPC messages over stdio.

Forcing one population through the other's interface creates friction.
Instead, `cmd/agentmoat` and `cmd/agentmoat-mcp` are sibling binaries that
translate from their respective transport to identical calls into
`pkg/agentmoat`. That structural symmetry is what keeps the two surfaces
from drifting.

## Why client-go directly

agentmoat never shells out to `kubectl`. Every read goes through the typed
`client-go` interface (Deployments, StatefulSets, DaemonSets, CronJobs,
Jobs, Pods). This pattern matches the K8s-tooling ecosystem (`kube-bench`,
`polaris`, `pluto`) and provides:

- Strong typing (no JSON parsing of `kubectl` output).
- Predictable error surface (typed K8s errors, not stderr scraping).
- Single auth path (kubeconfig or in-cluster service account, no extra binary).
- Embeddability (no PATH dependency on `kubectl`).

## Package map

| Package | Responsibility |
| --- | --- |
| `cmd/agentmoat` | CLI entrypoint (cobra). Thin shell over `pkg/agentmoat`. |
| `cmd/agentmoat-mcp` | MCP-over-stdio server. Thin shell over `pkg/agentmoat`. |
| `pkg/agentmoat` | Orchestration: `Scan`, `Preflight`, `Probe`, `Plan`, `Apply`, `Verify`, `Rollback`, `Explain`, `AssessWorkload`. |
| `pkg/scanner` | Cluster enumeration via client-go; produces `Workload` values. |
| `pkg/classifier` | Pure-function rule engine; produces `Verdict` values. `ClassifyWithFacts` lets a rule refine its severity from `ClusterFacts` (today: `gpu-passthrough`). |
| `pkg/preflight` | Reads the RuntimeClass, nodes, and GPU labels into `ClusterFacts`; pure `Evaluate` turns facts into findings. Gates `apply`. |
| `pkg/probe` | The nvproxy probe: a one-shot pod that reads `runsc --version` and `runsc nvproxy list-supported-drivers` from a gVisor node. Dry-run by default. |
| `pkg/planner` | Migration plan generation from a `ScanReport`. |
| `pkg/applier` | Pod-template patching with `runtimeClassName`. |
| `pkg/verifier` | Compares live pods' `runtimeClassName` and hosting nodes to the plan; optional in-pod probe. |
| `pkg/explainer` | Educational text for `agentmoat explain`. |
| `pkg/output` | Renderers (table / JSON / YAML). |
| `internal/kube` | client-go construction helpers. |
| `internal/containerd` | (Phase 5) Drop-in config parser. |
| `internal/schema` | Versioned wire types (`agentmoat.io/v1alpha1`). |
| `internal/rules` | YAML overrides for classifier rules. |
