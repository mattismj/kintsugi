package main

import (
	"regexp"
	"runtime/debug"
	"strings"
	"testing"
)

// TestVersionIsResolved is the M12-B release stamp: `kintsugi version` must never
// print an empty string, whatever the build shape was.
func TestVersionIsResolved(t *testing.T) {
	if strings.TrimSpace(version) == "" {
		t.Fatalf("version is empty — the stamp was never resolved")
	}
}

// TestVersionMatchesTheReleaseShapes pins the three shapes versionFrom may
// produce. Anything else means the resolver drifted: a release is a bare
// semver, a dev build is the milestone stamp, and an unstamped dev build in a
// vcs checkout appends the revision it was built from.
func TestVersionMatchesTheReleaseShapes(t *testing.T) {
	shapes := []*regexp.Regexp{
		regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`),                        // release: 1.4.0, 1.4.0-rc1
		regexp.MustCompile(`^\d+\.\d+\.\d+-dev \(M\d+[a-z]?\)$`),                       // dev, no vcs: 0.8.0-dev (M13a)
		regexp.MustCompile(`^\d+\.\d+\.\d+-dev \(M\d+[a-z]?\) [0-9a-f]{12}(-dirty)?$`), // dev + vcs: … df884f9f1270-dirty
	}
	for _, want := range shapes {
		if want.MatchString(version) {
			return
		}
	}
	t.Fatalf("version %q matches none of the release shapes:\n  %s\n  %s\n  %s",
		version, shapes[0], shapes[1], shapes[2])
}

// TestVersionFromBuildInfo is the resolver itself, driven by synthesized build
// info so all three paths are covered regardless of the checkout this runs in.
func TestVersionFromBuildInfo(t *testing.T) {
	rev := "df884f9f127026130609ae5c737fcf5dcb68db57"
	cases := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{
			name: "tagged release reports the semver without the v",
			info: &debug.BuildInfo{Main: debug.Module{Path: "kintsugi", Version: "v1.4.0"}},
			want: "1.4.0",
		},
		{
			name: "a pre-release tag keeps its suffix",
			info: &debug.BuildInfo{Main: debug.Module{Path: "kintsugi", Version: "v1.4.0-rc1"}},
			want: "1.4.0-rc1",
		},
		{
			name: "a pseudo-version is a dev build, not a release",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "kintsugi", Version: "v0.0.0-20261021025354-" + rev[:12]},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: rev},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			want: devVersion + " " + rev[:12],
		},
		{
			name: "an uncommitted tree says so",
			info: &debug.BuildInfo{
				Main: debug.Module{Path: "kintsugi", Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: rev},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			want: devVersion + " " + rev[:12] + "-dirty",
		},
		{
			name: "no vcs at all falls back to the milestone stamp",
			info: &debug.BuildInfo{Main: debug.Module{Path: "kintsugi", Version: "(devel)"}},
			want: devVersion,
		},
		{
			name: "a truncated revision is not a stamp",
			info: &debug.BuildInfo{
				Main:     debug.Module{Path: "kintsugi", Version: "(devel)"},
				Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "df884f"}},
			},
			want: devVersion,
		},
		{
			name: "missing build info falls back to the milestone stamp",
			info: nil,
			want: devVersion,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := versionFrom(tc.info); got != tc.want {
				t.Fatalf("versionFrom() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDevVersionKeepsTheMilestoneFormat: the fallback is a contract other tooling
// reads (`kintsugi version`, the roadmap's Mxx numbering). Renaming the milestone
// without keeping the shape would silently break it.
func TestDevVersionKeepsTheMilestoneFormat(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+-dev \(M\d+[a-z]?\)$`).MatchString(devVersion) {
		t.Fatalf("devVersion %q is not in the X.Y.Z-dev (Mxx) format", devVersion)
	}
}
