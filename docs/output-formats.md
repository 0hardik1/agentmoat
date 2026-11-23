# Output formats

Every command takes `--output` (`-o`). There are five formats.

| Format | For | Commands |
| --- | --- | --- |
| `table` (default) | A person at a terminal. | All |
| `json` | Scripts, `jq`, files that a later command reads (`plan --scan`, `apply --plan`). Versioned schema `agentmoat.io/v1alpha1`. | All |
| `yaml` | The same fields as JSON, for review and diff. | All |
| `markdown` (or `md`) | A CI job summary, a pull-request comment, a merge-request note. | `scan`, `plan`, `patch`, `apply`, `rollback`, `verify`, `preflight`, `probe nvproxy` |
| `sarif` | A code scanning service (SARIF 2.1.0). | `scan` |

A command refuses a format that it cannot write, before it reads a file or a
cluster. `agentmoat apply --dry-run=false -o sarif` is an error and changes
nothing.

`table` output is for reading. Do not parse it, and do not give it to a later
command: use `json` for that.

## Markdown

```bash
agentmoat scan -f ./k8s --fail-on sandboxed -o markdown >> "$GITHUB_STEP_SUMMARY"
```

`$GITHUB_STEP_SUMMARY` is the file that GitHub Actions shows as the summary of
a job. The same text can go in a comment on a pull request or a merge request.
A comment can have a smaller size limit than a job summary, so a long report
possibly does not fit in one.

### The gate is in the report

A job summary shows the report and nothing else: no stderr, no exit code. So
the Markdown form of `scan` starts with the result of the gate (`--fail-on`).
There are five results, and only one of them is a pass:

| First line | Meaning | Exit code |
| --- | --- | --- |
| ✅ **Gate passed** | The gate looked at one or more workloads. None failed. | 0 |
| ❌ **Gate failed** | One or more workloads failed. With `--fail-on sandboxed` they are listed below the line. | 2 |
| ⚠️ **Gate checked nothing** | No workload was there to check. With `--fail-on sandboxed`: no workload asks for the RuntimeClass. If your sandbox RuntimeClass has another name, pass `--runtime-class`. | 0 |
| ❌ **Gate could not answer** | `--fail-on sandboxed` on input that is not rendered. See [exit codes](exit-codes.md). | 1 |
| ➖ **No gate** | `--fail-on none`. | 0 |

After that line: the counts, the incompatible workloads, the workloads that
need review, the compatible workloads (collapsed), what each rule means, and
what the scan did not classify.

The other commands follow the same order. The headline says the result
(`apply`: was the cluster changed; `verify`: do the pods run in the sandbox;
`preflight`: can the cluster host the RuntimeClass). The rows that need a
person come next: failed steps, mismatches, error findings. The rest is in a
collapsed block.

The headline of `verify --wait` says that the result is after a wait, and how
the wait ended: "3 of 8 workload(s) are not verified after a wait of 3m0s".
After one look, "not verified" can mean that the rollout is in progress.
After a wait it means that the workloads did not come up, and the reader of a
job summary cannot see the command line to tell the two apart. The table then
has a Ready column (the Ready pods of all pods of the step).

`probe nvproxy` writes the preflight report with the probe
result in the facts: the `runsc` release, the number of drivers that nvproxy
supports, and the support status of each group of GPU nodes.

### The report of `patch`

`patch` changes files, so its headline says what happened to the files:

| First line | Meaning | Exit code |
| --- | --- | --- |
| ➖ **Dry run.** | No file was changed. The line has the number of lines that a run with `--dry-run=false` adds. | 0 |
| ✅ **N line(s) added to M file(s).** | Each selected workload is patched. | 0 |
| ➖ **Nothing to change.** | No workload is compatible and without a RuntimeClass. | 0 |
| ⚠️ **K of N selected workload(s) were not patched.** (or, in a dry run, "cannot be patched") | Some workloads are in a form that `patch` does not edit, or a file could not be written. | 3 |
| ❌ **None of the N selected workload(s) could be patched.** | No file was changed. | 1 |

After that line: the counts, the workloads that were not patched with the
reason for each, the patched workloads with the file and the line of the new
line, the diff of all files in one `diff` block, and the excluded workloads
(collapsed).

The diff in the report is for a reader. It shows 400 lines at most, a line
is cut at 240 characters, and control characters are replaced. To apply a
diff with a tool, take it from the JSON output (`.spec.files[].diff`), which
has each diff in full. `git apply` reads it when `patch` got relative paths
and ran in the root of the repository: the names in the diff are then
`a/<path>` and `b/<path>`. For an absolute path the names are the path
itself, with no `a/` and `b/`.

The `table` output has the diff too, with the colors of `git diff`. It
replaces control characters in the lines of the diff, so that a line of a
manifest cannot write over the report on a terminal. That is all that it
does to a line. In the step log of GitHub Actions a line can also be a
command for the runner (it starts with `::`, or has `##[` in it), and the
table does not prevent that: see
[the exit codes in a pipeline](ci-cd.md#the-exit-codes-in-a-pipeline).

### Limits

Each table and each list shows 50 entries at most, and then says how many it
left out. A long value is cut at 120 characters, and a long sentence at 600
(in the report of `patch`: a reason at 300, and the diff as said above).
One row of a workload table names 6 rules at most; the list of rules below the
tables has all of them. This keeps the output small: GitHub accepts 1 MiB of
job summary for each step, and a test keeps a report with the longest strings
in every row below half of that. The JSON output always has every row.

### Text from manifests

`scan -f` and `patch` read files that a pull request controls, and they do
not check that a name in them is a valid Kubernetes name. A name could hold Markdown, an
HTML tag, or a line break. The Markdown output makes all of that inert:

- A name, a kind, a namespace, a file path, and a rule ID are in a code span.
- A sentence (a rule description, a gate summary, an error) has its
  punctuation escaped. A bare address (`http://...`, `www.`), an email
  address, and an `@name` in a sentence are put in a code span, because a
  renderer makes a link or a mention of them with no Markdown syntax.
- Line breaks and other control characters are replaced.
- The diff of `patch` is in a fenced block, where nothing is read as
  Markdown. The fence is longer than the longest run of backticks in the
  diff, so a line of a file cannot close it.

A manifest cannot add a link, an image, a heading, a table row, a mention, or
a "Gate passed" line to the report of the change that it is part of. The
tests check this on the Markdown text, and the documents that they render
were checked with cmark-gfm, the parser that GitHub uses.

One thing is not in scope: a reference that the hosting service adds by its
own rules to plain text. On GitHub, `#123` in a sentence can become a link to
an issue of the same repository.

## SARIF

```bash
agentmoat scan -f ./k8s --fail-on sandboxed -o sarif > agentmoat.sarif
```

Run the scan from the root of the repository and scan files, not standard
input. The reasons are in "Locations" below.

### What is in the file

- One result for each finding with severity `error` or `warn`. An `info`
  finding is a hint on a workload that is compatible, and is left out. The
  other formats have it.
- One rule entry for each rule that has a result. Its text is the title and
  the explanation from [`docs/explanations/`](explanations/), and
  it is the same in every report.
- The message of a result names the workload and says why the rule matched.
- `properties` on a result has the workload fields: `kind`, `namespace`,
  `name`, `runtimeClassName`, `compatibility`, `severity`.

### The level follows the gate

| Finding | Level |
| --- | --- |
| Severity `error`, on a workload that makes the gate fail | `error` |
| Severity `error`, on a workload that does not make the gate fail | `warning` |
| Severity `warn` | `warning` |
| Severity `info` | Not in the file |

GitHub code scanning uses the level of a result as the severity of the alert.
Its pull-request check fails, by default, on a new alert with the severity
`error`, `critical`, or `high`. (This is from the GitHub documents. This
project does not test an upload.) If every incompatible workload were an
`error`, a SARIF upload would be a second gate, and it would fail on the CNI
DaemonSet that `--fail-on sandboxed` exists to ignore. With this rule the two
agree:

- `--fail-on incompatible` (default): every `error` finding has the level
  `error`.
- `--fail-on sandboxed`: only the findings on workloads that ask for the
  RuntimeClass and cannot run in it.
- `--fail-on none`: no result has the level `error`.

### Locations

A result from a scan of files has the file and a line. The file is the path
that you gave to scan, with the path below it for a file in a directory.
GitHub reads a relative path as relative to the root of the repository. So
give scan a relative path, and run it from that root. An absolute path
becomes a `file://` URI, which is correct on the machine that ran the scan
and on no other.

The line is the first line of the object in the file. It is not the line of
the field that the rule matched (the `hostNetwork: true` line): the report
does not record that. One effect on GitHub: a pull request shows an alert as
an annotation only when the first line of the alert is in the diff. A change
to a field in the middle of an object that exists gives an alert in the code
scanning list, and no annotation in the pull request. The gate (the exit
code) does not have this limit, so use the gate to stop a pull request and
SARIF to track the findings.

A result has no file when the workload did not come from a file: a cluster
scan, or standard input. Such a result is in the SARIF file with a logical
location only (`namespace/Kind/name`). The GitHub documents say that code
scanning needs a file location to show a result, so do not expect such a
result in the list of alerts. `scan` prints a note on stderr when one or more
results have no file.

`helm template` output on standard input is a special case. Each document has
a `# Source: <chart>/templates/<file>` comment, and scan uses that path as
the file, with no line. The path starts at the chart name. It is the path in
the repository only when the chart directory has that name and is in the
directory that the scan runs in.

### Fingerprints

Each result has a fingerprint with the key `agentmoatFinding/v1`: a hash of
the rule ID and the workload kind, namespace, and name. It stays the same
when the file is edited or the object moves in it. GitHub code scanning uses
its own key (`primaryLocationLineHash`). Its upload action computes that key
from the file content when the SARIF file does not have it, and keeps other
keys.
