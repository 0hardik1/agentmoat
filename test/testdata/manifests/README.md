# Manifest fixtures for `agentmoat scan -f`

These files are input for `scripts/offline-smoke.sh` (`make offline-smoke`),
which runs the built binary against them with no cluster.

| Path | What it is | Expected |
| --- | --- | --- |
| `sandboxed-hostnetwork.yaml` | A Deployment that asks for `runtimeClassName: gvisor` and uses host networking | Incompatible. `--fail-on sandboxed` exits 2 |
| `sandboxed-compatible.yaml` | A Deployment that asks for `gvisor` and can run in it, plus a Service | Compatible. `--fail-on sandboxed` exits 0. The Service is "not classified" |
| `rollout-privileged.yaml` | An Argo `Rollout` with a privileged container | Incompatible, kind `Rollout.argoproj.io` |
| `helm-unrendered/` | A Helm template with `{{ }}` in it | Exit 1, no report, a hint to run `helm template` |
| `kustomize/` | A base (incompatible, no class) and an overlay patch that adds `gvisor` | The default gate reads the base. `--fail-on sandboxed` exits 1: the input is not rendered. `kubectl kustomize kustomize/overlay \| agentmoat scan -f - --fail-on sandboxed` exits 2 |

Do not scan this whole directory as one input: `helm-unrendered/` makes any
scan that includes it exit 1, by design.
