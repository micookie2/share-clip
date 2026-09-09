// Package buildinfo exposes the version metadata of a share-clip binary.
//
// The Makefile stamps four values in at link time:
//
//	go build -ldflags "-X github.com/micookie2/share-clip/internal/buildinfo.Version=v0.2.0 \
//	  -X ...buildinfo.Commit=0be9e27 -X ...buildinfo.BuildTime=2026-09-09T12:50:03Z"
//
// A plain `go build` injects nothing, so Get() falls back to the VCS
// information the Go toolchain stamps into every binary built from a git
// checkout (vcs.revision / vcs.time). That way `Commit` and `BuildTime` are
// still meaningful during development; only the exact build instant is
// replaced by the commit instant.
package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"time"
)

// Unknown is printed when neither the linker nor the VCS could tell us a
// value (e.g. `go build` from a tarball without git metadata).
const Unknown = "unknown"

// DefaultVersion is the version of the sources in this tree. Release builds
// override Version with the git tag; a build that passes an empty -X value
// must not erase it, so resolve() falls back to this constant.
const DefaultVersion = "0.1.0"

// Stamped in by -ldflags -X; see the Makefile.
var (
	Version   = DefaultVersion
	Commit    = "" // short SHA of the tree this binary was built from
	BuildTime = "" // RFC3339 UTC instant of the link step
	Dirty     = "" // "true" when the working tree had local changes
)

// Info is a resolved snapshot of the build metadata.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"buildTime,omitempty"` // RFC3339 UTC, or "unknown"
	GoVersion string `json:"goVersion"`
	Platform  string `json:"platform"` // e.g. linux/amd64
	Modified  bool   `json:"modified,omitempty"`
}

// Get returns the build metadata of the running binary, filling in whatever
// the linker did not set from the Go toolchain's own VCS stamping.
func Get() Info {
	if Commit != "" && BuildTime != "" {
		// Fully stamped by the Makefile: no need to read build info at all.
		return resolve(Version, Commit, BuildTime, Dirty == "true", nil)
	}
	return resolve(Version, Commit, BuildTime, Dirty == "true", vcsSettings())
}

// resolve fills the gaps in linker-stamped values with the VCS settings Go
// embeds in binaries built from a git checkout, then normalises the result.
// settings is the raw vcs.* list (nil to skip the fallback), which keeps this
// function testable without rebuilding the binary.
func resolve(version, commit, buildTime string, modified bool, settings []debug.BuildSetting) Info {
	// Only an unstamped field may be answered by the VCS metadata: a build the
	// Makefile stamped describes itself, working tree included.
	if commit == "" || buildTime == "" {
		for _, s := range settings {
			switch s.Key {
			case "vcs.revision":
				if commit == "" {
					commit = shortRev(s.Value)
				}
			case "vcs.time":
				if buildTime == "" {
					buildTime = normalizeTime(s.Value)
				}
			case "vcs.modified":
				modified = modified || s.Value == "true"
			}
		}
	}
	if commit == "" {
		commit = Unknown
	}
	if buildTime == "" {
		buildTime = Unknown
	}
	if version == "" {
		version = DefaultVersion
	}
	return Info{
		Version:   version,
		Commit:    commit,
		BuildTime: buildTime,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
		Modified:  modified,
	}
}

// BuiltLocal renders BuildTime in the machine's local timezone, e.g.
// "2026-09-09 20:50:03 +0800". Unparseable or unknown stamps degrade to the
// raw value, so nothing is silently invented.
func (i Info) BuiltLocal() string {
	t, err := time.Parse(time.RFC3339, i.BuildTime)
	if err != nil {
		return i.BuildTime
	}
	return t.Local().Format("2006-01-02 15:04:05 -0700")
}

// Summary is the one-line form used by -v and the startup logs:
//
//	0.1.0 (commit 0be9e27, built 2026-09-09 20:50:03 +0800, go1.25.0 linux/amd64)
func (i Info) Summary() string {
	v := i.Version
	if i.Modified {
		v += "+dirty"
	}
	return v + " (commit " + i.Commit + ", built " + i.BuiltLocal() +
		", " + i.GoVersion + " " + i.Platform + ")"
}

// String lets Info be printed directly by %v / slog.
func (i Info) String() string { return i.Summary() }

// vcsSettings returns the VCS stamps embedded in this binary, if any
// (vendor builds, tarballs and non-main-module builds have none).
func vcsSettings() []debug.BuildSetting {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}
	return bi.Settings
}

// normalizeTime renders an RFC3339 timestamp in UTC; values we cannot parse
// are passed through unchanged rather than dropped.
func normalizeTime(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC().Format(time.RFC3339)
	}
	return v
}

// shortRev trims a full SHA to the 7 characters git shows by default.
func shortRev(rev string) string {
	rev = strings.TrimSpace(rev)
	if len(rev) > 7 {
		return rev[:7]
	}
	if rev == "" {
		return ""
	}
	return rev
}
