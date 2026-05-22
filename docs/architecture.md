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
library exposes `Scan`, and (in later phases) `Plan`, `Apply`, `Verify`,
`Rollback`. Anyone who wants to embed agentmoat in their own operator,
controller, or SDK can `import` the package and call those functions
directly. No shell-out, no subprocess plumbing.

## Why both a CLI and an MCP server

Two distinct operators consume agentmoat:

1. **Humans**, who type commands at a terminal and read tables.
2. **AI agents**, which exchange JSON-RPC messages over stdio.

Forcing one population through the other's interface creates friction.
Instead, `cmd/agentmoat` and `cmd/agentmoat-mcp` are sibling binaries, both
under 100 lines of glue each, that translate from their respective transport
to identical calls into `pkg/agentmoat`. That structural symmetry is what
keeps the two surfaces from drifting.

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
| `cmd/agentmoat-mcp` | MCP-over-stdio server (Phase 4). Thin shell. |
| `pkg/agentmoat` | Orchestration: `Scan`, `Plan`, `Apply`, `Verify`, `Rollback`. |
| `pkg/scanner` | Cluster enumeration via client-go; produces `Workload` values. |
| `pkg/classifier` | Pure-function rule engine; produces `Verdict` values. |
| `pkg/planner` | (Phase 2) Migration plan generation. |
| `pkg/applier` | (Phase 2) Pod-template patching with `runtimeClassName`. |
| `pkg/verifier` | (Phase 3) Kubelet runtime-field check + in-pod probe. |
| `pkg/explainer` | (Phase 3) Educational text for `agentmoat explain`. |
| `pkg/output` | Renderers (table / JSON / YAML). |
| `internal/kube` | client-go construction helpers. |
| `internal/containerd` | (Phase 5) Drop-in config parser. |
| `internal/schema` | Versioned wire types (`agentmoat.io/v1alpha1`). |
| `internal/rules` | YAML overrides for classifier rules. |
