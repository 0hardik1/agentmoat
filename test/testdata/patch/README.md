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
