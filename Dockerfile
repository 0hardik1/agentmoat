# ---------------------------------------------------------------------------
# agentmoat: default container image (ghcr.io/0hardik1/agentmoat:<tag>)
# ---------------------------------------------------------------------------
#
# This file is NOT a build-from-source Dockerfile. GoReleaser builds the two
# static binaries first and then runs `docker buildx build` in a temporary
# directory that holds only those binaries, one sub-directory for each
# platform:
#
#   linux/amd64/agentmoat      linux/amd64/agentmoat-mcp
#   linux/arm64/agentmoat      linux/arm64/agentmoat-mcp
#
# buildx sets TARGETPLATFORM ("linux/amd64", "linux/arm64") for each
# platform it builds, so the COPY lines below pick the correct pair. See the
# `dockers_v2` block in .goreleaser.yaml. To build it by hand, use
# `goreleaser release --snapshot --clean`.
#
# Base image: distroless "static". It has CA certificates, /etc/passwd and
# tzdata, and nothing else: no shell and no package manager. The binaries
# are built with CGO_ENABLED=0, so they need no libc. The `nonroot` tag runs
# as uid 65532 with HOME=/home/nonroot, which is where the audit log goes
# (~/.agentmoat/audit.jsonl) unless AGENTMOAT_AUDIT_PATH is set.
#
# No shell means a CI system that runs a script inside the job image
# (GitLab CI, Jenkins, Tekton `script`) cannot use this image. Use the
# `-alpine` tag there (Dockerfile.alpine).
#
# There is no RUN line on purpose: an image made of COPY lines only builds
# for linux/arm64 on an amd64 runner without QEMU emulation.
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian13:nonroot

ARG TARGETPLATFORM

COPY ${TARGETPLATFORM}/agentmoat     /usr/local/bin/agentmoat
COPY ${TARGETPLATFORM}/agentmoat-mcp /usr/local/bin/agentmoat-mcp

# `docker run ghcr.io/0hardik1/agentmoat scan -f /work` runs the CLI. The MCP
# server is in the same image: `--entrypoint /usr/local/bin/agentmoat-mcp`.
ENTRYPOINT ["/usr/local/bin/agentmoat"]
