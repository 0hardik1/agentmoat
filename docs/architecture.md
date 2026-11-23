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
                    |    patcher/        |
                    |    applier/        |
                    |    verifier/       |
                    |    explainer/      |
                    +---------+----------+
                              | client-go
                    +---------v----------+
                    |  Kubernetes API    |   <-- kind locally, EKS in target
                    +--------------------+
```

Two paths read no cluster and build no Kubernetes client: `scan -f` (the
scan of manifest files) and `patch` (the edit of manifest files). They use
the scanner, the classifier, and, for `patch`, the patcher.

## Why a Go library at the core

Both the CLI and the MCP server are thin shells over `pkg/agentmoat`. The
library exposes `Scan`, `Preflight`, `Probe`, `Plan`, `Patch`, `Apply`,
`Verify`, `Rollback`, and `Explain`. Anyone who wants to embed agentmoat in their own operator,
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
| `pkg/agentmoat` | Orchestration: `Scan`, `Preflight`, `Probe`, `Plan`, `Patch`, `Apply`, `Verify`, `Rollback`, `Explain`, `AssessWorkload`. `Scan` reads a cluster or, with manifest paths, files and no cluster. `EvaluateGate` is the `scan --fail-on` decision. `Patch` reads manifest files, selects the workloads that `Plan` selects, and writes the edit of `pkg/patcher`. With a Kustomize directory (`patch_kustomize.go`) it edits no source file: it reads the directory, refuses one that has files of a person, and writes the patch files first and their list last. `Verify` takes its workloads from a plan or, with manifest paths (`verify_manifests.go`), from the files: each workload that asks for the RuntimeClass. `VerifyExitCode` is the exit code of a report. |
| `pkg/scanner` | Produces `Workload` values from a cluster (client-go) or from manifest text with no cluster (`FromManifests`, `ReadManifestPaths`). |
| `pkg/classifier` | Pure-function rule engine; produces `Verdict` values. `ClassifyWithFacts` lets a rule refine its severity from `ClusterFacts` (today: `gpu-passthrough`). |
| `pkg/preflight` | Reads the RuntimeClass, nodes, and GPU labels into `ClusterFacts`; pure `Evaluate` turns facts into findings. Gates `apply`. |
| `pkg/probe` | The nvproxy probe: a one-shot pod that reads `runsc --version` and `runsc nvproxy list-supported-drivers` from a gVisor node. Dry-run by default. |
| `pkg/planner` | Migration plan generation from a `ScanReport`. `ExclusionCause` is the selection rule that `plan` and `patch` share. |
| `pkg/patcher` | The text edit of `patch`: adds one `runtimeClassName` line to the pod spec of an object in YAML text, and changes nothing else. It finds the place with the YAML node positions, never writes YAML again from the objects, checks its own result (the new text must decode to the old objects plus the class), and makes the unified diff. Pure: text in, text out, no files and no cluster. `kustomize.go` is the other form of the change: the content of one strategic merge patch file for an object, its file name, and the `kustomization.yaml` of kind `Component` that lists the files. Those files are new, so they come from an encoder, with the identity of the object as the scanner read it (`scanner.ManifestObject`). |
| `pkg/applier` | Pod-template patching with `runtimeClassName`. |
| `pkg/verifier` | Compares live pods' `runtimeClassName` and hosting nodes to the plan, or to a list of targets from manifest files; optional in-pod probe. A target of a kind that it has no reader for is checked on the pods that the selector of its manifest selects, and a target with no verdict goes to the not-checked list of the report, never to the results. With a wait time, looks again until every step is ok and its pods stay Ready (`wait.go`, `readiness.go`). |
| `pkg/explainer` | Educational text for `agentmoat explain`. |
| `pkg/output` | Renderers: table, JSON, YAML, Markdown (job summaries and pull-request comments), and SARIF (`scan` only). `Format.Supports` is the list of which document takes which format. See [`output-formats.md`](output-formats.md). |
| `internal/kube` | client-go construction helpers. |
| `internal/containerd` | (Phase 5) Drop-in config parser. |
| `internal/schema` | Versioned wire types (`agentmoat.io/v1alpha1`). |
| `internal/rules` | YAML overrides for classifier rules. |
