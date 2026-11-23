# Pipeline examples

Four files to copy into your repository. [`docs/ci-cd.md`](../../docs/ci-cd.md)
has the full description: read it before you run a pipeline that changes a
cluster.

| File | CI system | What it does | Changes a cluster |
| --- | --- | --- | --- |
| [`github-pr-check.yml`](github-pr-check.yml) | GitHub Actions | Scans the manifests in each pull request. Fails when a workload that asks for the gVisor RuntimeClass cannot run in it. | No. It reads files only. |
| [`github-migration.yml`](github-migration.yml) | GitHub Actions | Moves one namespace to gVisor: plan, approval, apply, verify, and a rollback when the apply or the verify fails. | Yes, after the approval. |
| [`github-patch-pr.yml`](github-patch-pr.yml) | GitHub Actions | Writes the move into the manifest files with `patch` (or into a Kustomize component), and opens a pull request with the change. For a repository that is the source of truth for the cluster. | No. It changes files in a branch. |
| [`gitlab-ci.yml`](gitlab-ci.yml) | GitLab CI | The pull request check and the migration pipeline, with the container image. | The migration jobs do, after the manual step. |

## Start with the check

The check is the smallest step and it cannot break anything: it has no
access to a cluster.

```yaml
# .github/workflows/agentmoat.yml
name: agentmoat
on: pull_request
jobs:
  scan:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: 0hardik1/agentmoat@v0
        with:
          path: k8s
```

For another CI system, run the same command in the container image
(`ghcr.io/0hardik1/agentmoat:<version>-alpine`):

```sh
agentmoat scan -f k8s --fail-on sandboxed
```

Exit code `0`: the gate did not fail. `2`: the gate failed. `1`: the input
could not be scanned, which is not a pass.

## What is tested

| Example | How this project tests it |
| --- | --- |
| The GitHub Action (`0hardik1/agentmoat`) | `make offline-smoke` runs the script of the action with a model of the runner, for each exit code, each input, and hostile input. The workflow `.github/workflows/action-test.yml` runs the action on real Linux and macOS runners when the action changes. |
| `github-pr-check.yml` | The file is the action with one input. It is not run as a file. |
| `github-migration.yml` | Not run: the CI of this project has no cluster with gVisor nodes. Its agentmoat commands are the ones that `make e2e` runs on a kind cluster with a real `runsc`, including an apply that fails on one step and the rollback after it. |
| `github-patch-pr.yml` | Not run: it needs a repository to open a pull request in. Its `agentmoat patch` command is the one that `make offline-smoke` runs on fixtures. The `git` and `gh` commands, the `permissions:` keys, and the repository setting that it needs are from the GitHub documentation only. A pull request that it opens with the `GITHUB_TOKEN` gets its checks only after a person approves the runs, and with the input `kustomize_dir` the pull request moves nothing until a person adds the directory to `components:` of an overlay: read the header of the file. |
| `gitlab-ci.yml` | Not run: this project is on GitHub. The keywords were checked against the GitLab documentation. The agentmoat commands are the same as in `github-migration.yml`. |
