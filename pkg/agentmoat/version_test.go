package agentmoat

import (
	"runtime/debug"
	"testing"
)

// TestResolveBuildInfo covers the three sources in priority order: -ldflags,
// then the build record, then the placeholders. Every case passes a
// hand-made build record, so the result does not depend on how the test
// binary was built.
func TestResolveBuildInfo(t *testing.T) {
	mainModule := func(version string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main:     debug.Module{Path: modulePath, Version: version},
			Settings: settings,
		}
	}
	const fullSHA = "d498f5ee7532a1b2c3d4e5f60718293a4b5c6d7e"

	tests := []struct {
		name        string
		ldVersion   string
		ldSHA       string
		info        *debug.BuildInfo
		wantVersion string
		wantSHA     string
	}{
		{
			name:        "ldflags win over the build record",
			ldVersion:   "0.2.0",
			ldSHA:       "abc1234",
			info:        mainModule("v9.9.9", debug.BuildSetting{Key: "vcs.revision", Value: fullSHA}),
			wantVersion: "0.2.0",
			wantSHA:     "abc1234",
		},
		{
			name:        "make build passes dev explicitly and keeps it",
			ldVersion:   "dev",
			ldSHA:       "abc1234",
			info:        mainModule("v0.2.0"),
			wantVersion: "dev",
			wantSHA:     "abc1234",
		},
		{
			name:        "go install of a tag: module version, no VCS settings",
			info:        mainModule("v0.2.0"),
			wantVersion: "0.2.0",
			wantSHA:     "unknown",
		},
		{
			name: "go build in a clean checkout: pseudo-version and short revision",
			info: mainModule("v0.1.1-0.20261110115824-d498f5ee7532",
				debug.BuildSetting{Key: "vcs.revision", Value: fullSHA},
				debug.BuildSetting{Key: "vcs.modified", Value: "false"}),
			wantVersion: "0.1.1-0.20261110115824-d498f5ee7532",
			wantSHA:     "d498f5e",
		},
		{
			name: "go build in a dirty checkout marks the revision",
			info: mainModule("v0.1.1-0.20261110115824-d498f5ee7532+dirty",
				debug.BuildSetting{Key: "vcs.revision", Value: fullSHA},
				debug.BuildSetting{Key: "vcs.modified", Value: "true"}),
			wantVersion: "0.1.1-0.20261110115824-d498f5ee7532+dirty",
			wantSHA:     "d498f5e-dirty",
		},
		{
			name:        "devel build with no VCS record falls back to the placeholders",
			info:        mainModule("(devel)"),
			wantVersion: "dev",
			wantSHA:     "unknown",
		},
		{
			name:        "no build record at all",
			info:        nil,
			wantVersion: "dev",
			wantSHA:     "unknown",
		},
		{
			name: "imported as a library: our version comes from Deps, the revision is not ours",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/embedder", Version: "v7.0.0"},
				Deps: []*debug.Module{
					{Path: "example.com/other", Version: "v1.0.0"},
					{Path: modulePath, Version: "v0.3.1"},
				},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: fullSHA}},
			},
			wantVersion: "0.3.1",
			wantSHA:     "unknown",
		},
		{
			name: "imported as a library but absent from Deps",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "example.com/embedder", Version: "v7.0.0"},
			},
			wantVersion: "dev",
			wantSHA:     "unknown",
		},
		{
			name:        "a version that does not start with v<digit> is kept as is",
			info:        mainModule("vendor-build"),
			wantVersion: "vendor-build",
			wantSHA:     "unknown",
		},
		{
			name:        "a revision shorter than the short length is kept whole",
			info:        mainModule("v0.2.0", debug.BuildSetting{Key: "vcs.revision", Value: "abc12"}),
			wantVersion: "0.2.0",
			wantSHA:     "abc12",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotVersion, gotSHA := resolveBuildInfo(tc.ldVersion, tc.ldSHA, tc.info)
			if gotVersion != tc.wantVersion || gotSHA != tc.wantSHA {
				t.Errorf("resolveBuildInfo(%q, %q) = (%q, %q), want (%q, %q)",
					tc.ldVersion, tc.ldSHA, gotVersion, gotSHA, tc.wantVersion, tc.wantSHA)
			}
		})
	}
}

// TestResolveBuildInfoSetsPackageVersion pins the side effect that the two
// binaries rely on: after the call, every report carries the resolved
// version. The MCP binary did not set Version before this function existed,
// so its reports always said "dev".
func TestResolveBuildInfoSetsPackageVersion(t *testing.T) {
	saved := Version
	t.Cleanup(func() { Version = saved })

	Version = "stale"
	version, sha := ResolveBuildInfo("1.2.3", "abc1234")
	if version != "1.2.3" || sha != "abc1234" {
		t.Fatalf("ResolveBuildInfo = (%q, %q), want (1.2.3, abc1234)", version, sha)
	}
	if Version != "1.2.3" {
		t.Errorf("package Version = %q after ResolveBuildInfo, want 1.2.3", Version)
	}
}
