# Manifest fixtures for `agentmoat patch`

These files are input for `scripts/offline-smoke.sh` (`make offline-smoke`).
The script copies them to a temporary directory and runs the built binary on
the copy, with no cluster. The files here are never changed.

| Path | What it is | Expected |
| --- | --- | --- |
| `kustomize/base/` | A Deployment that can run in the sandbox and has no class, with comments | `patch` adds one line to `web.yaml` |
| `kustomize/overlay/` | An overlay with a name prefix, a namespace, and a patch that sets the replicas | `patch` does not edit it: the patch file is a partial object. `kubectl kustomize` of the overlay, after the edit of the base, gives a Deployment that has the class |
| `forms/block.yaml` | A Pod in block style | Patched |
| `forms/flow.yaml` | A Pod in flow style | Not edited, with the reason |
| `forms/pod.json` | A Pod in JSON | Not edited, with the reason |

`patch -f forms` exits 3: one workload is patched and two are not.

## `component/`: the input for `patch --kustomize`

A Kustomize tree where one overlay must move and the other must not.

| Path | What it is | Expected |
| --- | --- | --- |
| `component/base/` | Five workloads and a Service. Four can run in the sandbox, in four forms: a Deployment, a CronJob that names a namespace, an Argo `Rollout`, and a flow-style Pod. A DaemonSet has host networking. The kustomization gives each object a name prefix and a namespace | `patch -f component/base --kustomize <dir>` makes four patch files and their list, and changes no file of the base |
| `component/staging/` | The overlay that moves. It has a name prefix and a patch of its own | With the directory in its `components:`, its build has the class on the four workloads |
| `component/prod/` | The overlay that does not move. It uses the same base | Its build has no class |
| `component/other/stray.yaml` | A workload that no kustomization of the tree reads | With this file in the input too, the build of the overlay stops: the patch has no target |

The script does the one step that `patch` leaves for a person: it adds the
directory to `components:` of the overlay. The builds run when `kubectl` is
installed (`kubectl kustomize`, a local build).
