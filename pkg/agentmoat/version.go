// Package agentmoat: build provenance.
//
// A release binary gets its version from -ldflags (see the Makefile and
// .goreleaser.yaml). A binary built with a plain `go install ...@v0.2.0` or
// `go build` has no ldflags, so without a fallback it reports "dev
// (unknown)" even though the Go toolchain recorded the module version and
// the VCS revision in the binary. ResolveBuildInfo reads that record.
//
// Both binaries (cmd/agentmoat and cmd/agentmoat-mcp) call ResolveBuildInfo
// once at start-up. One call site per binary, one function: the two cannot
// report different versions for the same build.
package agentmoat

import (
	"runtime/debug"
	"strings"
)

// modulePath is the Go module path of this repository. ResolveBuildInfo
// looks it up in the build record to find the module version.
const modulePath = "github.com/0hardik1/agentmoat"

// Placeholders used when neither -ldflags nor the build record has a value.
const (
	versionUnknown = "dev"
	shaUnknown     = "unknown"
)

// shortSHALen is the length `git rev-parse --short` prints by default, and
// what the Makefile and goreleaser inject. The fallback uses the same length
// so the two sources look alike.
const shortSHALen = 7

// ResolveBuildInfo returns the version and commit that this binary reports.
//
// The -ldflags values win when they are not empty. An empty value falls
// back to the build record that the Go toolchain embeds (the module version
// and the vcs.revision setting). When neither source has a value, the result
// is "dev" and "unknown".
//
// It also stores the version in the package-level Version variable, so every
// report (ScanReport, MigrationPlan, ...) carries the same string that the
// `version` command prints.
func ResolveBuildInfo(ldflagsVersion, ldflagsSHA string) (version, sha string) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		info = nil
	}
	version, sha = resolveBuildInfo(ldflagsVersion, ldflagsSHA, info)
	Version = version
	return version, sha
}

// resolveBuildInfo is the pure core of ResolveBuildInfo. It takes the build
// record as an argument so a test can pass a hand-made one.
func resolveBuildInfo(ldflagsVersion, ldflagsSHA string, info *debug.BuildInfo) (version, sha string) {
	version, sha = ldflagsVersion, ldflagsSHA

	if version == "" {
		version = moduleVersion(info)
	}
	if sha == "" {
		sha = vcsRevision(info)
	}
	if version == "" {
		version = versionUnknown
	}
	if sha == "" {
		sha = shaUnknown
	}
	return version, sha
}

// moduleVersion finds the version of this module in the build record.
//
// The module is the main module when the binary is cmd/agentmoat or
// cmd/agentmoat-mcp. It is a dependency when another program imports
// pkg/agentmoat; in that case the main module's version belongs to that
// program, not to agentmoat, so we must not report it.
//
// The leading "v" is removed ("v0.2.0" becomes "0.2.0") because goreleaser
// injects the version without it. The result is "" for a build with no
// version: the toolchain writes "(devel)" there.
func moduleVersion(info *debug.BuildInfo) string {
	if info == nil {
		return ""
	}
	raw := ""
	if info.Main.Path == modulePath {
		raw = info.Main.Version
	} else {
		for _, dep := range info.Deps {
			if dep != nil && dep.Path == modulePath {
				raw = dep.Version
				break
			}
		}
	}
	if raw == "" || raw == "(devel)" {
		return ""
	}
	if len(raw) > 1 && raw[0] == 'v' && raw[1] >= '0' && raw[1] <= '9' {
		raw = raw[1:]
	}
	return raw
}

// vcsRevision returns the short commit hash from the build record, with a
// "-dirty" suffix when the working tree had uncommitted changes.
//
// The vcs.* settings describe the main module only. When agentmoat is a
// dependency of another program, they describe that program's repository,
// so the result is "".
func vcsRevision(info *debug.BuildInfo) string {
	if info == nil || info.Main.Path != modulePath {
		return ""
	}
	revision, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return ""
	}
	if len(revision) > shortSHALen {
		revision = revision[:shortSHALen]
	}
	if modified {
		revision += "-dirty"
	}
	return revision
}
