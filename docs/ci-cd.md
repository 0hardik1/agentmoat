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
in the releases from `v0.3.0`. `patch` and `verify -f` (section 3) are in the
releases from `v0.4.0`. The release `v0.1.0` has none of them.

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

The check of the result is `agentmoat verify -f` after the deploy:
[Check the result of a change that came through the source](#check-the-result-of-a-change-that-came-through-the-source).

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
                 (with --kustomize <dir>: patch files for one overlay, and no source file changes)
pull request     the check of section 1 runs on the result; a person reads the diff
merge            your deploy pipeline or deploy tool deploys the change
after the deploy agentmoat verify -f <dir> -n <namespace> --wait 5m
                 (reads the cluster; the files say which workloads to check)
rollback         git revert
```

`patch` reads no cluster and needs no credentials. It reads the manifests as
`scan -f` does, and it changes files only.

The check of section 1 is a gate for this pull request only when the branch
requires it (a required status check). See
[the pull request from a pipeline](#the-pull-request-from-a-pipeline).

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
  shell with `set -o pipefail`). The rendered output can be the input of
  the Kustomize form: see [Helm](#helm).
- **Standard input.** It has no file to edit. It is input only with
  `--kustomize`, which edits no input.

`patch` does not replace another RuntimeClass. A workload that has one is
excluded, with the reason.

### Kustomize

There are two ways, for two cases.

| Case | Command | What changes |
| --- | --- | --- |
| Each overlay of a base must move | `patch -f <base>` | One added line for each workload in the files of the base. |
| One overlay must move first | `patch -f <base> --kustomize <dir>` | No source file. New files in `<dir>`, which is a Kustomize component. |

#### Each overlay: edit the base

`patch -f <dir>` edits the files that it is given. In a Kustomize tree those
are the bases: an overlay patch is a partial object (a workload kind with no
containers), and it is never selected. A change to a base changes each
overlay that uses the base.

#### One overlay first: `--kustomize <dir>`

`patch` then edits no source file. It writes the change into the directory
as a Kustomize component: one strategic merge patch file for each selected
workload, and a `kustomization.yaml` of kind `Component` that lists them.

```sh
agentmoat patch -f base --kustomize overlays/staging/gvisor                   # the diff, no file written
agentmoat patch -f base --kustomize overlays/staging/gvisor --dry-run=false   # write the files
```

```
overlays/staging/gvisor/
  kustomization.yaml                  kind: Component, with the list of the patches
  deployment.apps_web.yaml            one file for each workload
  cronjob.batch_batch_nightly.yaml    <kind>.<group>_<namespace>_<name>.yaml
```

A patch file names one object and sets one field:

```yaml
# Generated by `agentmoat patch --kustomize`.
# A strategic merge patch: it puts one workload in the RuntimeClass.
# To take the workload out again, remove this file and its line in kustomization.yaml.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
spec:
  template:
    spec:
      runtimeClassName: gvisor
```

Then add the directory to the kustomization that must move. `patch` does not
do this step: that file is yours, and `patch` does not know which
kustomization you want to move.

```yaml
# overlays/staging/kustomization.yaml
resources:
  - ../../base
components:
  - gvisor
```

The rules of this form:

- **The input is what that kustomization reads, and nothing more.** Kustomize
  finds the target of a patch by the names that the kustomization gets from
  its `resources:`, before its own `namePrefix` and `namespace`. So give
  `patch` the files of the base (`-f base`), or the output of the base
  (`kustomize build base | agentmoat patch -f - --kustomize <dir>`). The two
  give patches with different names, and the two work. Two other inputs do
  not work. For each one Kustomize stops the build of the overlay, with a
  message that names the patch:
  - Input with more objects than the kustomization reads (a second base, or a
    directory with files that the kustomization does not list).
  - The output of the overlay itself. It has the names after the prefix of
    the overlay.

  `patch` cannot check this rule. It does not know which kustomization will
  use the component, and it does not run Kustomize. Your build is the check,
  so build the overlay in the pull request (the commands below do).
- **A patch names a namespace only when the object in the input names one.**
  `-n` is the default for the verdicts, and it is not written to a patch.
- **The directory is new, empty, or the directory of an earlier run.** `patch`
  refuses a directory that has other files, and a `kustomization.yaml` that
  it did not write (exit `1`, no file written). A run only adds: patch files
  that are not there, and entries at the end of its own list. It changes no
  file that is there. A patch file that is there with other content is
  reported as not edited. A second run changes nothing.
- **Each form of input gets a patch**, also JSON, flow-style YAML, and a pod
  spec behind an anchor, because no input is edited. Two objects get none and
  are listed with the reason: an object with `generateName` and no name (a
  patch finds its target by name), and an item of a typed list that has no
  `apiVersion` or `kind` of its own.
- **A patch for an object that is gone stops the build.** When you remove a
  workload from the base, remove its patch file and its line in the list.
- **To take a workload out of the sandbox again**, remove its patch file and
  its line in the list. To take all of them out, remove the directory from
  `components:`. A later run with the same input makes the patch again for a
  workload that is compatible and has no class.

#### Scan what gets deployed

For the two ways, the verdicts of `patch` describe the files, not what gets
deployed, and `patch` says so in its output (`not rendered`). An overlay can
add `hostNetwork: true` to a workload that is compatible in the base. After
the change, build each overlay and scan the result with the gate:

```sh
kustomize build overlays/staging > rendered.yaml    # stops on a patch that has no target
agentmoat scan -f rendered.yaml --fail-on sandboxed
```

The pull request check of section 1 does this with the `kustomize` input of
the action.

#### Helm

`patch` does not edit a template. There are two ways to move a chart:

- Set the chart value for `runtimeClassName`, when the chart has one.
- Give the rendered output to the Kustomize form:
  `helm template <release> <chart> | agentmoat patch -f - --kustomize <dir>`.
  The patches name the rendered objects. They are for a kustomization that
  has the rendered output in its `resources:` and the directory in its
  `components:`. `make offline-smoke` builds that case when Helm is
  installed. This project did not run the patches through a Helm
  post-renderer, or through the Helm support of Kustomize, Argo CD, or Flux.

### The pull request from a pipeline

[`examples/ci/github-patch-pr.yml`](../examples/ci/github-patch-pr.yml) is a
workflow that a person starts. It runs `patch`, writes the report to the job
summary, commits the change to a new branch, and opens a pull request. It
has no access to a cluster and needs no secret of yours.

Is the check of section 1 a gate for that pull request? Two facts about
GitHub decide it:

- A pull request that a workflow opens with the `GITHUB_TOKEN` of its run
  gets its `pull_request` workflow runs in a state that needs an approval.
  Before a person approves the runs, the check did not run. A check that did
  not run is not a pass, but it does not stop a merge either, unless the
  check is a required status check of the branch. So make the check required.
- With the token of a GitHub App installation, or a personal access token,
  in place of the `GITHUB_TOKEN`, the runs start with no approval.

With the input `kustomize_dir` the workflow runs the Kustomize form. The
pull request then has only the new directory, and no kustomization uses it
yet: a merge of that pull request moves nothing. A person adds the directory
to `components:` of the overlay, as a commit in the same pull request. Only
after that commit does the check say something about the change, and only
when it renders that overlay (the `kustomize` input of the action). Before
that commit the check has nothing of the change to look at, so a pass is
not a result. The text of the pull request says this.

The workflow also needs the repository setting "Allow GitHub Actions to
create and approve pull requests". This project did not run the workflow:
the header of the file says which parts are from the GitHub documentation
only.

### The exit codes in a pipeline

| Exit | Meaning | A pipeline |
| --- | --- | --- |
| `0` | Each selected workload is patched, or no workload was selected. | Goes on. With no change to a file, there is nothing to commit. |
| `3` | Some selected workloads are patched. The others are in a form that is not edited (with `--kustomize`: an object that a patch cannot name), or their file could not be written. | The files of the patched workloads are written. Commit them, and keep the report: it lists what a person must do by hand. |
| `1` | An error, or no selected workload could be patched. | Stops. |

A dry-run has the exit code that the run with `--dry-run=false` has for the
same files, unless a write fails.

`-o markdown` is the report for a job summary. Its first line says what
happened, and it has the diff:

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

With a Kustomize component there is a second way: remove the directory from
`components:` of the overlay. The patch files stay in the repository and do
nothing.

### Check the result of a change that came through the source

A move through the repository has no plan. The manifest files say which
workloads must run in the sandbox, so `verify -f` takes the list from the
files and the live state from the cluster:

```sh
# After the deploy of the source that now has runtimeClassName:
agentmoat verify -f ./k8s -n shop --wait 5m

# Kustomize and Helm: give the objects as they were deployed.
kustomize build overlays/prod | agentmoat verify -f - --wait 5m
helm template shop ./chart -f values-prod.yaml --no-hooks | agentmoat verify -f - -n shop --wait 5m
```

`helm template` also prints the hooks of a chart: a Job that Helm can delete
when it succeeds, and the test pods, which an install does not make. With
`--no-hooks` they are not in the output (checked with Helm 4.3), so verify
does not look at them. Without the flag, verify lists each hook object that
is not in the cluster as not checked (see the table of kinds below).

verify reads the files as `scan -f` does. Then it checks, in the cluster,
each workload whose pod spec asks for the RuntimeClass (`--runtime-class`,
default `gvisor`). It does not look at the other workloads of the input;
the report has their count (`summary.excluded`).

Use `--wait` directly after a deploy, for the reason in
[Apply, verify, roll back](#apply-verify-roll-back): a workload is ok only
when all its pods have the class, are Ready, and stayed Ready.

| Exit code | Meaning | What the pipeline does |
| --- | --- | --- |
| `0` | Each workload that asks for the class runs with it (with `--wait`: and its pods stayed Ready). | Done. |
| `4` | The cluster shows that one workload or more does not: a pod with no class, a Deployment (or another controller) that is not there, pods that are not Ready after the wait time, a Job that failed (with `--wait`). | The deploy did not give the sandbox. Revert, or find the cause. |
| `1` | verify has no answer: an error, input that it refuses, or a workload that it could not check. | Not a pass, and not a reason for a revert. Read the message. |

The rule for exit `4`: it comes only from what the cluster shows. When the
files do not name what is in the cluster, verify does not guess. It refuses
the run, or it lists the workload as not checked. Each of the two is exit
`1`.

#### What the input must be

- **The objects as they were deployed.** Input with a kustomization file or
  an overlay patch is refused: a name in such files is not always the name
  in the cluster (an overlay adds a name prefix and a namespace). Render it,
  and give `-` for standard input.
- **`-n <namespace>` when the files name no namespace.** It is the namespace
  that the objects were deployed to, and it is not a filter. verify refuses
  a workload that has no namespace. It does not use `default`.
- **The files after the change.** When no workload of the input asks for
  the class, verify refuses the run (`there is nothing to verify`, exit
  `1`). The usual cause is the wrong input: the base and not the overlay, or
  the branch before the merge. This is different from `verify --plan` with a
  plan that has no steps, which is exit `0` with a note.

agentmoat does not know what your deploy tool deployed. You give it the
files, so give it the same commit and the same render.

#### Which kinds it can check

| Kind in the files | How verify finds the pods | Result |
| --- | --- | --- |
| Deployment, StatefulSet, DaemonSet, CronJob | It reads the object from the cluster, and then its pods. This is the check of `verify --plan`. An object that is not in the cluster is an `error`: nothing removes these kinds. | `ok`, `mismatch`, or `error` |
| Job, Pod (in the cluster) | The same. A pod that ran to its end (phase `Succeeded`) is checked on its `runtimeClassName` only: it is never Ready again. A Job that is finished is checked on its pod template. With `--wait`, a Job that failed is a `mismatch`, with the reason from the Job; verify looks at it again until the time ends, because a deploy tool can replace the Job. | `ok` or `mismatch` |
| Job, Pod (not in the cluster) | After a deploy that worked, these can be gone: a Job with `ttlSecondsAfterFinished`, a hook that its tool deletes, a test pod that an install does not make. verify cannot tell that from an object that was not deployed. With `--wait` it looks again until the time ends. | Not checked |
| Another kind with a pod template and a `spec.selector` (an Argo `Rollout`, a ReplicaSet) | It has no reader for the object. It lists the pods that the selector of the manifest selects, in the namespace of the object. | `ok` or `mismatch`, with `basis: selector` |
| The same, when the selector selects no pods | It cannot tell "scaled to zero" from "not there". With `--wait` it looks again until the time ends. | Not checked |
| A kind with no selector in the manifest (a Knative `Service`), and an object with `generateName` and no name | It cannot find the pods from the file. | Not checked |

A result with `basis: selector` is about pods, not about the object. Pods of
another workload that have the same labels count too. So do the pods of an
earlier revision of the same object: an Argo `Rollout` that is paused on a
canary step has old pods with no class, and it is a `mismatch` until the
rollout is complete.

A Job from before the change keeps its pod template (the template of a Job
cannot change). When the deploy did not replace such a Job, the Job in the
cluster has no class, and the result is a `mismatch`.

A workload that is not checked has no result. The report lists it with the
reason (`spec.notChecked`, and a block at the top of the table and of the
Markdown report), and the exit code is `1` when no other workload failed.
For such a workload, look at the `runtimeClassName` of its pods.

#### Permissions

The read-only role
([`deploy/clusterrole-readonly.yaml`](../deploy/clusterrole-readonly.yaml))
is sufficient. For a kind that verify has no reader for, it lists pods only,
so the role needs no rule for that kind.

#### What this check does not tell you

- Nothing about a workload that does not ask for the class. When a change
  removes `runtimeClassName` from a workload, verify does not look at that
  workload. The pull request check of section 1 is the gate for the content
  of the files.
- Nothing about an object that is in the cluster and not in the files.

## 4. What is tested, and what is not

| Part | Test |
| --- | --- |
| `scan -f`, the gate, the Markdown and SARIF reports | `make offline-smoke` in CI, on each pull request. |
| The script of the action: each exit code, each input, the limit on annotations, a render, the download with its checksum, hostile names | `make offline-smoke` in CI, with a model of the runner (`scripts/action-smoke.sh`). The download is tested against a local directory, not against github.com. |
| The action on a runner: `action.yml`, the inputs, the outputs, the step result | `.github/workflows/action-test.yml`, on Linux and macOS runners, when the action changes. It uses the binary of the commit. The download from a release is a job that a person starts. |
| `apply`, `verify --wait`, `rollback`, an apply that fails on one step and its rollback, the plan of a second run | `make e2e`, on a kind cluster with a real `runsc`. A person runs it; it is not in CI. |
| `patch`: the one added line, the forms that are not edited, the check of each edit, the diff | Unit tests in CI (`pkg/patcher`, `pkg/agentmoat`). `git apply` reads each diff in the tests and must make the new file from the old one. `make offline-smoke` runs the command on a copy of the fixtures: one added line for each selected workload, no removed line, the scan then finds the class, and a second run changes nothing. |
| `patch` on a Kustomize tree | `make offline-smoke`: the base is edited, an overlay patch is not, the output says that the input is not rendered, and the render of the overlay (`kubectl kustomize`) then has the class. One tree (a base, and an overlay with a name prefix, a namespace, and a patch). |
| `patch --kustomize` | Unit tests in CI (`pkg/patcher`, `pkg/agentmoat`): the patch of each kind, the directory rules, a run that only adds, a second run, a file that cannot be written. `make offline-smoke` builds real trees with `kubectl kustomize` (it passed with Kustomize 5.5 and 5.8): a base with a name prefix and a namespace, and two overlays. The overlay that uses the component has the class, the other overlay does not, and the scan of the render passes the gate. Input as the files of the base, as the output of the base, and as the output of `helm template`. The two inputs that do not work stop the build. `git apply` makes the same files from the diff of the dry-run. |
| `examples/ci/github-patch-pr.yml` as a workflow | Not run. Its `patch` command is the one in the rows above. The `git` and `gh` commands, the `permissions:` keys, and the repository setting are from the GitHub documentation only. |
| `verify -f` | Unit tests in CI (`pkg/verifier`, `pkg/agentmoat`): each kind, each refusal before a cluster read, a selector with no pods, a Job and a Pod in each state (not there, at its end, failed), the exit codes. `make e2e`, on a kind cluster with a real `runsc`: `patch` writes the class into a copy of the manifest file, `kubectl apply` deploys the copy to a second namespace, and `verify -f --wait` directly after it exits `0`. The run has 11 workloads: the 8 of the file, a ReplicaSet that is checked through its selector, and a Job and a bare Pod that run to their end. `--in-pod-probe` then finds gVisor in each pod that runs. Before the deploy the same file gives exit `4` against the namespace that has no class, and a Job that is not in the cluster gives exit `1`. A person runs `make e2e`; it is not in CI. |
| A deploy of patched files by a deploy tool (Argo CD, Flux, Helm), and `verify -f` after it | Not tested. The e2e deploys with `kubectl apply`. An Argo `Rollout` is tested with a fake cluster only; the e2e uses a ReplicaSet for the selector path. A hook that Helm or Argo CD deletes is tested as "a Job that is not in the cluster" only. `helm template --no-hooks` was run with Helm 4.3 on a chart with one hook Job and one test pod: the output had neither. |
| `examples/ci/github-migration.yml` as a workflow | Not run. Its commands are the ones in the row above. |
| `examples/ci/gitlab-ci.yml` | Not run. The keywords were checked against the GitLab documentation. |
| A deploy tool (Argo CD, Flux, Helm) with a live change | Not tested. The `source-managed` warning says only what agentmoat knows. |
