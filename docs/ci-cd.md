# CI/CD

agentmoat is made to run in a pipeline. This page has the three uses:

1. **[The pull request check](#1-the-pull-request-check).** Scan the
   manifests in a repository on each pull request. No cluster, no
   credentials.
2. **[The migration pipeline](#2-the-migration-pipeline).** Move workloads
   to gVisor in the cluster with a plan, an approval, an apply, a verify,
   and a rollback when something fails.
3. **[The move through the repository](#3-the-move-through-the-repository).**
   Write the change into the manifest files with `patch`, so that a pull
   request reviews it, your deploy pipeline or deploy tool deploys it, and
   `git revert` undoes it.

Use 2 when a pipeline job deploys to the cluster and the manifests are not
the source of truth. Use 3 when a repository is the source of truth (plain
manifests, Kustomize, Argo CD, Flux): a change that is only in the cluster
is then not in the source.

The files to copy are in [`examples/ci/`](../examples/ci/). The exit codes
are the interface between agentmoat and a pipeline:
[`exit-codes.md`](exit-codes.md) has all of them.
[`What is tested`](#4-what-is-tested-and-what-is-not) says which parts of
this page a test proves and which parts it does not.

The action, the container image, and the scan of manifests (`scan -f`) are
in the releases from `v0.3.0`. The release `v0.1.0` has none of them.

## 1. The pull request check

The check answers one question for a change to your manifests: can each
workload that asks for the gVisor RuntimeClass run in it? It stops a change
such as `hostNetwork: true` on a sandboxed workload before the merge, when
the cost is one review comment. After the deploy, the same error is a pod
that does not start.

### GitHub Actions

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

The action downloads an agentmoat release, runs
`agentmoat scan -f <path> --fail-on sandboxed`, and reports in three places:

| Place | What is there |
| --- | --- |
| The job summary | The report in Markdown. Its first line is the result of the gate, for every outcome. When the scan did not run (a render failed, a download failed), the summary says that, and does not stay empty. |
| Annotations | One error for each workload that makes the gate fail, on the file and the line where the workload starts. With a render input (`kustomize`, `helm-chart`) the annotation has no file: a rendered object is not a file in the repository. 10 at most; the last one then says how many more there are. |
| The step log | The table, and what the tools printed. |

The step result follows the exit code of the scan:

| Exit code | Meaning | Step |
| --- | --- | --- |
| `0` | The gate did not fail. | passes |
| `2` | The gate failed. | fails |
| `1` | The scan got no answer: input that does not parse, input that is not rendered, a render that failed. This is not a pass. | fails |

#### Inputs

Set exactly one of `path`, `kustomize`, and `helm-chart`. For more than one
source, use the action one time for each.

| Input | Default | Meaning |
| --- | --- | --- |
| `path` | | Files or directories with manifests, one for each line. |
| `kustomize` | | A directory with a kustomization file. The action renders it with `kubectl kustomize` (or `kustomize build`) and scans the result. |
| `helm-chart` | | The directory of a Helm chart. The action renders it with `helm template` and scans the result. |
| `helm-values` | | Values files for the chart, one for each line. |
| `fail-on` | `sandboxed` | `sandboxed`: fail when a workload that asks for the sandbox RuntimeClass cannot run in it. `incompatible`: fail when a workload cannot run in gVisor, also one that does not ask for it. `none`: never fail; the report is the result. |
| `runtime-class` | `gvisor` | The name of the sandbox RuntimeClass. |
| `namespace` | | The namespace for objects that name none. It does not filter. With `helm-chart`, also the namespace of the render. |
| `rules` | | A file with rule overrides (`agentmoat scan --rules`). |
| `sarif-file` | | Where to write a SARIF report, for an upload to code scanning in a later step. |
| `version` | | The agentmoat release to download: `latest`, or a tag such as `v0.3.0`. See [Versions](#versions). |
| `binary` | | The path of an agentmoat binary on the runner. The action then downloads nothing. |

#### Outputs

| Output | Meaning |
| --- | --- |
| `exit-code` | `0`, `2`, or `1`, as in the table above. |
| `markdown-file` | The path of the Markdown report. |
| `json-file` | The path of the JSON report (schema `agentmoat.io/v1alpha1`). |
| `sarif-file` | The path of the SARIF report. Empty when the input `sarif-file` was not set. |

#### Why the action renders

With `fail-on: sandboxed`, the scan must see what gets deployed. A Kustomize
overlay can add `runtimeClassName` to a base that does not have it, and a
Helm template is not YAML before the render. So the scan refuses (exit `1`)
input that it can tell is not rendered, and the action has the two render
inputs. The render goes to a file, and the action checks its exit code
before the scan: a render that fails in the middle must not give a scan that
passes on half of the input.

#### What the runner needs

| Tool | For | When it is not there |
| --- | --- | --- |
| `bash`, `curl`, `tar`, and `sha256sum` or `shasum` | The download of the release. | The step fails with a message. Use the input `binary`. |
| `jq` | The annotations. | One annotation with no file, and the report is still in the job summary. The exit code does not change. |
| `kubectl` or `kustomize` | The input `kustomize`. | The step fails with a message. |
| `helm` | The input `helm-chart`. | The step fails with a message. |

The Ubuntu runners of GitHub have all of them. The macOS runners have no
`kubectl` and no `helm`: install the tool in an earlier step when you use a
render input there. There is no agentmoat release for Windows.

#### Versions

- `uses: 0hardik1/agentmoat@v0` follows the newest 0.x release of the action,
  and the action downloads the latest agentmoat release.
- `uses: 0hardik1/agentmoat@v0.3.0` (a full version) uses the action and the
  agentmoat binary of that release.
- With a commit SHA (`uses: 0hardik1/agentmoat@<sha>`), set the input
  `version` too. The action cannot tell a release from a SHA, and downloads
  the latest release when `version` is empty.
- The action checks the SHA-256 of the archive against the checksums file of
  the same release, and does not use an archive that does not agree.
- A runner with no access to github.com: put an agentmoat binary on the
  runner and give its path in `binary`.

#### Permissions and trust

- The scan reads files only. It builds no Kubernetes client and reads no
  kubeconfig, also when the runner has one.
- The action needs no secret and no token permission. A pull request from a
  fork gets a read-only token and no secrets, and the check works there.
- A pull request controls the text that the report shows: the names of
  workloads, files, and namespaces. A name can hold a line break or text
  that is a command for the runner. The action prints the output of every
  tool in a part of the log where the runner runs no command, makes each
  annotation with the encoding of the runner, and the Markdown report
  escapes every value from a manifest. A pull request cannot add an
  annotation, a link, or a mention to its own report.
- Code scanning is optional. It needs `security-events: write`, which a pull
  request from a fork does not get. The check itself does not need it.
  [`examples/ci/github-pr-check.yml`](../examples/ci/github-pr-check.yml)
  has the steps.

### Other CI systems

Each release has a container image with a shell, for a CI system that runs a
script in the job image:

```
ghcr.io/0hardik1/agentmoat:<version>-alpine     for example v0.3.0-alpine
```

The default tag (`ghcr.io/0hardik1/agentmoat:<version>`) has no shell. Use
it when the CI system runs the binary directly.

```sh
agentmoat scan -f k8s --fail-on sandboxed                  # the table, and the gate
agentmoat scan -f k8s --fail-on sandboxed -o markdown      # for a merge request note
agentmoat scan -f k8s --fail-on sandboxed -o sarif         # for a code scanning upload
```

For Kustomize or Helm, render to a file first and scan the file. A pipe
(`helm template ./chart | agentmoat scan -f -`) is correct only in a shell
with `set -o pipefail`: without it, a render that fails gives the scan cut
input, and the exit code of the pipe is the one of the scan.

[`examples/ci/gitlab-ci.yml`](../examples/ci/gitlab-ci.yml) has the job for
GitLab. [`output-formats.md`](output-formats.md) has the two report formats.

### What a file scan cannot see

- A container that admission adds in the cluster (a sidecar of a service
  mesh, for example). It is not in the manifests.
- A field name with a typing error. `hostNetwrok: true` is not a field, the
  scan ignores it, and Kubernetes does too (or refuses the object).
- The cluster: the nodes, the RuntimeClass, the GPU drivers.

The check of the result is `agentmoat verify` after the deploy. It needs a
plan from before the deploy:
[Check the result of a change that came through the source](#check-the-result-of-a-change-that-came-through-the-source)
has the steps.

## 2. The migration pipeline

```
plan job (read-only)      scan -> plan -> apply as a dry-run
                          plan.json is an artifact
approval                  a person reads the plan
apply job                 apply --dry-run=false
                          verify --wait 5m
                          rollback --dry-run=false     only when one of the two failed
```

[`examples/ci/github-migration.yml`](../examples/ci/github-migration.yml) and
[`examples/ci/gitlab-ci.yml`](../examples/ci/gitlab-ci.yml) are this pipeline.

### Identities

| Job | Commands | ClusterRole |
| --- | --- | --- |
| plan | `scan`, `plan`, `apply` (dry-run), `preflight` | `agentmoat-readonly` ([`deploy/clusterrole-readonly.yaml`](../deploy/clusterrole-readonly.yaml)) |
| apply | `apply --dry-run=false`, `verify`, `rollback --dry-run=false` | `agentmoat-apply` ([`deploy/clusterrole-apply.yaml`](../deploy/clusterrole-apply.yaml)) |

The two files have an example ServiceAccount and the bindings. As written,
they bind one ServiceAccount to the two roles. For a pipeline, make two
identities, one with the read-only role only, and give the apply identity
only to the job that is behind the approval.

`apply` needs `patch` on the namespace object: it records the plan there
before it changes a workload (see
[`exit-codes.md`](exit-codes.md#the-record-on-the-namespace)). An identity
that can patch the workloads and not the namespace changes nothing: each
step fails with `the workload was not changed`.

agentmoat finds the cluster as kubectl does: `--kubeconfig`, then the
`KUBECONFIG` variable, then `~/.kube/config`, then the ServiceAccount of the
pod when the job runs in the cluster.

### The plan is what the reviewer approves

- `plan` is a pure function: the same scan gives the same plan and the same
  `planHash`. The plan in the artifact is the plan in the summary.
- `apply` checks the hash of the plan file before it uses it. A file that
  was edited after `plan` is refused. A plan with no hash is refused too.
- A plan has a step only for a workload that has no RuntimeClass. A workload
  that already has one is listed under "excluded" with the reason.

The last point is what makes the pipeline safe to run again. Run 1 moves the
workloads of a namespace. Run 2, some weeks later, scans the same namespace:
its plan has only the workloads that are new. If one of them fails, the
rollback of run 2 undoes run 2, and the workloads of run 1 stay in the
sandbox.

### Apply, verify, roll back

| Step | Exit codes | The pipeline then |
| --- | --- | --- |
| `apply --dry-run=false` | `0` every step applied. `3` some steps failed. `5` the preflight refused; nothing changed. `1` an error, or every step failed. | On `0`: verify. On each other code: roll back. |
| `verify --wait 5m` | `0` every workload of the plan runs in the sandbox and stayed Ready. `4` one does not, after the wait time. `1` verify could not check. | On `0`: done. On each other code: roll back. |
| `rollback --dry-run=false` | `0` every step is undone, or there was nothing to undo. `3` some steps could not be undone. `1` an error. | On `3` or `1`: the job has failed two times. Run the rollback again; it does the steps that are left. |

Use `verify --wait`, not a `sleep`. Directly after an apply, the rollout is
in progress and one look gives exit `4` for a migration that is correct.
With a wait time, verify looks until each workload has its pods in the
sandbox, Ready, and still Ready 10 seconds later. A pod in a crash loop is
Ready for a moment in each restart, and does not pass.

**Make the rollback its own step, with a condition.** The examples use
`if: failure() && steps.apply.outcome != 'skipped'` (GitHub) and
`when: on_failure` (GitLab). The step then runs for each failure of the
apply or the verify, and it does not run when a person cancels the job.

What the rollback step does, for each way that the run can go:

| What happened | The rollback |
| --- | --- |
| The apply was complete and verify failed (exit `4`). | Removes the RuntimeClass from every workload of the plan. The workloads go back to runc. |
| The apply failed on some steps (exit `3`). | Removes the RuntimeClass from the workloads that the apply changed, and leaves the others. |
| The preflight refused the apply (exit `5`). | Finds no record of the plan and changes nothing. Exit `0`. |
| The job was canceled, or the runner stopped, in the middle of the apply. | The step does not run. See the next section. |

The rollback is correct in these cases because `apply` writes a record on
the namespace before its first change there.
[`exit-codes.md`](exit-codes.md#rollback-after-each-way-that-apply-can-end)
has the full table and the limits.

`verify` exit `1` is "could not check" (the API server did not answer, for
example), and the examples roll back on it: after a change, a state that
cannot be confirmed is not a good state. To roll back only on exit `4`, read
the exit code of the verify step in the condition of the rollback step.

### After a cancel

A canceled run rolls nothing back, on purpose: a person who stops a pipeline
did not ask for a second change to the cluster. The cluster then has some or
all workloads of the plan in the sandbox, and the namespace has the record
of the plan.

Get the plan file of that run (the artifact `agentmoat-plan`) and decide:

```sh
agentmoat apply --plan plan.json --dry-run=false      # finish the apply, or
agentmoat rollback --plan plan.json                   # see what a rollback would undo
agentmoat rollback --plan plan.json --dry-run=false   # undo it
```

Keep the plan artifact for as long as you want to be able to roll that run
back. The record on the namespace is the hash of the plan, and only the plan
file with that hash can use it.

### One run for a namespace at a time

agentmoat records one plan on a namespace, and it has no lock. Two applies
in one namespace at the same time write over the record of each other. Give
the pipeline a limit of one run for a namespace. On GitHub that is
`concurrency` with the namespace in the group name (the example has it). On
GitLab, `resource_group` makes one job exclusive and not a full pipeline, so
it is a partial limit: keep to one migration pipeline for a namespace at a
time there.

One plan after the other is the normal case, and the newest apply has the
namespace:

- A rollback with the plan of an older run changes nothing. Its steps are
  `skipped` (the namespace has the record of a newer plan) or
  `already-applied` with a note on stderr (no record is left).
- To roll back that older plan: apply it again, then roll it back. For a
  workload that has the RuntimeClass, the apply changes nothing and starts
  no rollout.

### The audit log

`apply` and `rollback` append one JSON line for each step to
`~/.agentmoat/audit.jsonl`, dry-run steps too. In a job, the home directory
goes away with the runner. Set `AGENTMOAT_AUDIT_PATH` to a file in the
workspace and keep it as an artifact, also when the job fails (the examples
do this).

Each step that changes a workload also makes a Kubernetes Event on it, and
`kubectl describe` shows it.

### Workloads that a deploy tool owns

When a workload in the plan has the marker of a deploy tool (Argo CD, Flux,
Helm), the plan has the warning `source-managed`. `apply` changes the live
object only. The source that the tool deploys from (a git repository, a
chart) does not get the change, and what the tool does with a live change
that is not in its source depends on the tool and its settings.

For such a workload, add `runtimeClassName` to the source.
[The move through the repository](#3-the-move-through-the-repository) does
that with `agentmoat patch`, and the pull request check above is the gate
for the change. [`preflight.md`](preflight.md#source-managed) has the
markers that agentmoat reads.

## 3. The move through the repository

```
a branch         agentmoat patch -f <dir> --dry-run=false     one added line for each workload
pull request     the check of section 1 is the gate; a person reads the diff
merge            your deploy pipeline or deploy tool deploys the change
after the deploy agentmoat verify --plan <the plan from before> --wait 5m
                 (Deployment, StatefulSet, DaemonSet, CronJob; see below)
rollback         git revert
```

`patch` reads no cluster and needs no credentials. It reads the manifests as
`scan -f` does, and it changes files only.

### What `patch` changes

It selects the workloads that `plan` selects: compatible, and with no
RuntimeClass. `--include-review` adds the workloads that need review. For
each selected workload it adds one line to the pod spec:

```diff
     spec:
+      runtimeClassName: gvisor
       containers:
         - name: web
```

Nothing else in a file changes. Comments, the order of keys, quotes, the
indent, the line endings (LF or CRLF), and a missing line ending at the end
of the file stay as they are. `patch` does not write YAML again from the
objects: it finds the line of the pod spec and adds text after it.

It checks each edit before it uses it. It decodes the new text and requires
two things: each selected workload now has the class, and nothing else in the
file is different from the old text. When the edit of a file does not pass,
that file is not changed, and its workloads are reported as not edited with
the reason.

Differences from `plan` and `apply`:

- A Pod and a Job are selected too. The API refuses a live change to them,
  and a file has no such limit.
- A workload kind with a pod template that is not one of the six (an Argo
  `Rollout`, for example) is selected and edited, as `scan -f` reads it.
- There is no plan file and no record on a namespace. The diff is the plan,
  and the commit is the record.

By default `patch` is a dry-run: it prints the diff and writes no file.
`--dry-run=false` writes the files. A second run changes nothing, because
the workloads then have the class.

```sh
agentmoat patch -f k8s                        # the diff, no file written
agentmoat patch -f k8s --dry-run=false        # write the files
agentmoat scan -f k8s --fail-on sandboxed     # the same check as the pull request check
```

`-l <selector>` limits the patch to the workloads whose labels match, for a
move in steps. `-n <namespace>` is the default namespace for objects that
name none, as for `scan -f`; it is not a filter and it is not written.
`--runtime-class` sets the name to write.

### What `patch` does not edit

A selected workload in one of these forms is listed with the reason, and the
exit code is `3` (or `1` when no selected workload could be patched):

| Form | What to do |
| --- | --- |
| JSON | Add `"runtimeClassName"` to the pod spec by hand. |
| Flow-style YAML (`spec: {containers: [...]}`) | The same. |
| A pod spec behind a YAML anchor or alias | The same. An edit there would change each object that uses the anchor. |
| A pod spec that has a `runtimeClassName` key with no value | Set the value by hand. |

`patch` refuses these inputs as a whole (exit `1`, no file written):

- **A file that does not parse.** As for `scan -f`, one bad document is an
  error for the run.
- **A Helm template.** A template is not YAML before it is rendered, and the
  rendered output is not a file of your repository. Set the chart value for
  `runtimeClassName`, and check the result with
  `helm template <chart> | agentmoat scan -f - --fail-on sandboxed` (in a
  shell with `set -o pipefail`).
- **Standard input.** It has no file to edit.

`patch` does not replace another RuntimeClass. A workload that has one is
excluded, with the reason.

### Kustomize

`patch -f <dir>` edits the files that it is given. In a Kustomize tree those
are the bases: an overlay patch is a partial object (a workload kind with no
containers), and it is never selected.

Two things follow, and `patch` says so in its output (`not rendered`):

- **A change to a base changes each overlay that uses the base.** When only
  one environment must move, do not patch the base. Add `runtimeClassName`
  to the pod spec in a patch of that overlay.
- **The verdicts describe the files, not what gets deployed.** An overlay can
  add `hostNetwork: true` to a workload that is compatible in the base. After
  the edit, scan the rendered objects with the gate, for each overlay:

```sh
kustomize build overlays/prod > rendered.yaml
agentmoat scan -f rendered.yaml --fail-on sandboxed
```

The pull request check of section 1 does this with the `kustomize` input of
the action.

### The exit codes in a pipeline

| Exit | Meaning | A pipeline |
| --- | --- | --- |
| `0` | Each selected workload is patched, or no workload was selected. | Goes on. With no change to a file, there is nothing to commit. |
| `3` | Some selected workloads are patched. The others are in a form that is not edited, or their file could not be written. | The files of the patched workloads are written. Commit them, and put the report in the pull request: it lists what a person must do by hand. |
| `1` | An error, or no selected workload could be patched. | Stops. |

A dry-run has the exit code that the run with `--dry-run=false` has for the
same files, unless a write fails.

`-o markdown` is the report for a job summary or for the text of a pull
request. Its first line says what happened, and it has the diff:

```sh
rc=0
agentmoat patch -f k8s --dry-run=false -o markdown > patch.md || rc=$?
cat patch.md >> "$GITHUB_STEP_SUMMARY"      # the report, for each result
if [ "$rc" -ne 0 ] && [ "$rc" -ne 3 ]; then exit "$rc"; fi
```

On GitHub Actions, write the report to the job summary or to a file, as
above, and not to the step log. The report has names and lines of your
manifest files, and the runner reads a line of the log as a command when it
starts with `::` (spaces before it do not matter) or has `##[` in it. A
context line of a diff is a space and then a line of a file, so the default
`table` output in a step log has this problem too. When the manifests can
come from a person that you do not trust (a pull request from a fork), keep
the output of the command out of the log, or print it between
`::stop-commands::<token>` and `::<token>::` as the action does.

### Rollback

The rollback of this move is `git revert` of the commit, and a deploy of
the result. agentmoat has no part in it. `agentmoat rollback` does not apply
here: it acts only in a namespace that has the record of a plan that
`agentmoat apply` wrote.

### Check the result of a change that came through the source

`verify` checks the workloads of a plan, and a plan has a step only for a
workload that has no RuntimeClass at the time of the scan. So the plan for
this check must be from before the deploy:

```sh
# Before the deploy: the workloads do not have the class yet.
agentmoat scan --namespace shop --fail-on none --output json > scan.json
agentmoat plan --scan scan.json --output json > plan.json     # keep this file

# ... the deploy tool deploys the source that now has runtimeClassName ...

# After the deploy: do the workloads of that plan run in the sandbox?
agentmoat verify --plan plan.json --wait 5m
```

This check covers the kinds that a plan has a step for: Deployment,
StatefulSet, DaemonSet, and CronJob. `patch` also edits a Pod, a Job, and
other kinds with a pod template (an Argo `Rollout`, for example). A plan
from a cluster has no step for those, so `verify` does not look at them, and
its exit code says nothing about them. agentmoat has no check after the
deploy for those kinds. Look at the `runtimeClassName` of their pods.

Limit the scan to the workloads that the change moves, with `--namespace`
and `--selector` (a label selector). The plan has a step for each compatible
workload of the scan that has no RuntimeClass, and verify reports a mismatch
(exit `4`) for a workload of the plan that the change did not move.

A plan from a scan after the deploy cannot do this. The workloads then have
the class, that plan has no step for them, and verify checks nothing. It
says so (`nothing was verified`, on stderr and in the Markdown report) and
exits `0`.

## 4. What is tested, and what is not

| Part | Test |
| --- | --- |
| `scan -f`, the gate, the Markdown and SARIF reports | `make offline-smoke` in CI, on each pull request. |
| The script of the action: each exit code, each input, the limit on annotations, a render, the download with its checksum, hostile names | `make offline-smoke` in CI, with a model of the runner (`scripts/action-smoke.sh`). The download is tested against a local directory, not against github.com. |
| The action on a runner: `action.yml`, the inputs, the outputs, the step result | `.github/workflows/action-test.yml`, on Linux and macOS runners, when the action changes. It uses the binary of the commit. The download from a release is a job that a person starts. |
| `apply`, `verify --wait`, `rollback`, an apply that fails on one step and its rollback, the plan of a second run | `make e2e`, on a kind cluster with a real `runsc`. A person runs it; it is not in CI. |
| `patch`: the one added line, the forms that are not edited, the check of each edit, the diff | Unit tests in CI (`pkg/patcher`, `pkg/agentmoat`). `git apply` reads each diff in the tests and must make the new file from the old one. `make offline-smoke` runs the command on a copy of the fixtures: one added line for each selected workload, no removed line, the scan then finds the class, and a second run changes nothing. |
| `patch` on a Kustomize tree | `make offline-smoke`: the base is edited, an overlay patch is not, the output says that the input is not rendered, and the render of the overlay (`kubectl kustomize`) then has the class. One tree (a base, and an overlay with a name prefix, a namespace, and a patch). |
| A deploy of patched files by a deploy tool, and `verify` after it | Not tested. `verify` with a plan from before a change is tested for a live `apply` only (`make e2e`). |
| `examples/ci/github-migration.yml` as a workflow | Not run. Its commands are the ones in the row above. |
| `examples/ci/gitlab-ci.yml` | Not run. The keywords were checked against the GitLab documentation. |
| A deploy tool (Argo CD, Flux, Helm) with a live change | Not tested. The `source-managed` warning says only what agentmoat knows. |
