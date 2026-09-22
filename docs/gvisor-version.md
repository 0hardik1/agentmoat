# The gVisor release pin

agentmoat installs a specific gVisor release onto the nodes it builds. This
page explains where that pin lives, why it is a pin and not `latest`, how it
is kept current, and how to bump it by hand.

## Where the pin lives

The same `yyyymmdd.N` tag appears in three load-bearing files:

| File | Field |
| --- | --- |
| `packer/eks-gvisor-al2023.pkr.hcl` | `variable "gvisor_version" { default = ... }` |
| `kind/Dockerfile.gvisor-node` | `ARG GVISOR_VERSION=...` |
| `scripts/build-gvisor-node.sh` | `GVISOR_VERSION="${GVISOR_VERSION:-...}"` |

The current pin is `20260914.0`. `make check-gvisor-version` fails when the
three disagree.

gVisor tags look like `release-20260914.0` on GitHub and in `runsc --version`.
The download URLs drop the `release-` prefix, so the pin is written without it.

## Why a pin, and not the apt repository or `latest`

gVisor's install guide recommends its Debian package where possible. Neither
node image can use it:

- The EKS AMI is Amazon Linux 2023, which uses `dnf` and RPM. gVisor publishes
  no RPM.
- The kind node image is Debian-based, but it is a Docker build that must be
  reproducible. Pulling `latest` at build time would make two builds of the
  same commit install different binaries.

Both images therefore download the release tarball `gvisor.tar.bz2` and its
`gvisor.tar.bz2.sha512` from
`https://storage.googleapis.com/gvisor/releases/release/<tag>/<arch>/` and
verify the checksum. A pin plus automation to keep it fresh gives
reproducibility and currency at the same time.

## What the release tarball holds

From release `20260831.0` on, gVisor publishes one tarball per architecture
(`gvisor.tar.bz2` and `gvisor.tar.zstd`, each with a `.sha512`). It no longer
publishes `runsc` and `containerd-shim-runsc-v1` as separate files. The
tarball holds:

| Path | Installed as |
| --- | --- |
| `runsc` | `/usr/local/bin/runsc` |
| `containerd-shim-runsc-v1` | `/usr/local/bin/containerd-shim-runsc-v1` |
| `gvisor-bin/` (`gvisor_sentry`, `checkpointgofer`, `runsc-fd-parking`, ...) | `/usr/local/bin/gvisor-bin/` |

`gvisor-bin/` holds runsc's *sidecar* binaries. runsc looks for the directory
next to its own binary. Older releases fell back to copies embedded in
`runsc` when the directory was missing. From `20260831.0` the default
`--sidecar-usage-policy` refuses that fallback, so an install without
`gvisor-bin/` cannot start a sandbox. Both images install it.

The kind image extracts the tarball in a separate build stage, because
`kindest/node` has no `bzip2`. The Packer template installs `bzip2` with
`dnf` only when the base AMI lacks it.

Releases up to `20260817.0` publish the tarball as well as the separate
files, so the checker works for pins on either side of the change.

## How the pin is kept current

Two workflows share `scripts/check-gvisor-version.sh`:

- **CI on every PR** (`.github/workflows/ci.yml`, job `gvisor-pin`) checks that
  the three pins agree and that the tarball and its checksum exist for both
  architectures. It does not fail on drift, so a PR never turns red because
  gVisor shipped that morning.
- **Weekly drift check** (`.github/workflows/gvisor-drift.yml`, Mondays 06:17
  UTC, also on manual dispatch) runs the checker with `--latest`. That mode
  asks the GitHub tags API for the newest release whose tarball is in the
  bucket. A tag can exist before its artifacts are uploaded, and some tags
  never get any (`20260824.0` has none), so "latest" means "newest
  published", not "newest tagged". When the pin is behind, the workflow
  rewrites it with `scripts/bump-gvisor-version.sh`, pushes a
  `chore/gvisor-<tag>` branch, and opens a pull request. One open PR per
  target release.

Every tag the checker skips prints a warning, which shows as an annotation on
the workflow run. It used to be a quiet note, and that hid a real failure: when
gVisor stopped publishing the separate `runsc` files, the checker read each
new release as "not uploaded yet" and skipped `20260824.0` through
`20260914.0`. The drift runs of 2026-09-07, -14 and -21 stayed green while the
pin fell four releases behind. A skip that repeats on the same tag for more
than a week means the bucket layout changed again. Check
`gsutil ls gs://gvisor/releases/release/<tag>/<arch>/` (or the
[bucket listing API](https://storage.googleapis.com/storage/v1/b/gvisor/o?prefix=releases/release/))
and update `GV_ARTIFACTS` in `scripts/lib/gvisor-version.sh`, the
Dockerfile and the Packer template together.

The drift workflow needs two one-time repository settings:

1. Settings, Actions, General, Workflow permissions: enable "Allow GitHub
   Actions to create and approve pull requests". Without it `gh pr create`
   is rejected.
2. Optional: a fine-grained personal access token with `contents: write` and
   `pull-requests: write`, stored as the `GVISOR_BUMP_TOKEN` secret. Pushes
   made with the default `GITHUB_TOKEN` do not trigger other workflows, so a
   bot PR opened without the PAT will not run CI until a person pushes to the
   branch or closes and reopens the PR.

Dependabot (`.github/dependabot.yml`) covers GitHub Actions and Go modules.
It cannot see this pin, which is why the drift workflow exists.

## Bumping by hand

```bash
# Am I behind? Exit 2 means yes; the output names the newest published tag.
make check-gvisor-latest

# Rewrite the pin everywhere and re-run the checks.
./scripts/bump-gvisor-version.sh 2026MMDD.N

# The kind image is cached by tag. The build script rebuilds when the baked
# gVisor version differs, but an existing cluster keeps its old node image.
make kind-down && make e2e

# Rebuild and roll the EKS AMI when you are ready (see docs/eks-deployment.md).
```

Every mention of the tag that `bump-gvisor-version.sh` does not know about is
reported as a warning. Add new files to its `FILES` list when that happens.

Two things the script cannot bump for you, both in the release notes of the
new tag:

- the NVIDIA card list nvproxy supports (`NvproxySupportedProducts` in
  `pkg/preflight/gpu.go`; see [`gpu-nvproxy.md`](gpu-nvproxy.md));
- the supported driver list, which is compiled into `runsc` and which
  `agentmoat probe nvproxy` reads from the nodes, so it needs no code change
  but does need the nodes re-imaged before the probe reports the new list.
