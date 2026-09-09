package buildinfo

import (
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func vcs(rev, when string, modified bool) []debug.BuildSetting {
	return []debug.BuildSetting{
		{Key: "vcs.revision", Value: rev},
		{Key: "vcs.time", Value: when},
		{Key: "vcs.modified", Value: map[bool]string{true: "true", false: "false"}[modified]},
	}
}

func TestResolvePrefersLinkerValues(t *testing.T) {
	full := "0123456789abcdef0123456789abcdef01234567"
	got := resolve("v0.2.0", "0be9e27", "2026-09-09T12:50:03Z", false, vcs(full, "2020-01-01T00:00:00Z", true))
	if got.Version != "v0.2.0" || got.Commit != "0be9e27" || got.BuildTime != "2026-09-09T12:50:03Z" {
		t.Errorf("linker values were overridden by VCS: %+v", got)
	}
	if got.Modified {
		t.Error("vcs.modified must not leak into an explicitly stamped build")
	}
}

func TestResolveFallsBackToVCS(t *testing.T) {
	got := resolve("0.1.0", "", "", false,
		vcs("0123456789abcdef0123456789abcdef01234567", "2026-09-08T18:30:00+02:00", true))
	if got.Commit != "0123456" {
		t.Errorf("Commit = %q, want the 7-char short SHA", got.Commit)
	}
	if got.BuildTime != "2026-09-08T16:30:00Z" {
		t.Errorf("BuildTime = %q, want UTC RFC3339", got.BuildTime)
	}
	if !got.Modified {
		t.Error("Modified = false, want true for a dirty tree")
	}
}

func TestResolveWithoutAnyMetadata(t *testing.T) {
	got := resolve("", "", "", false, nil)
	if got.Commit != Unknown || got.BuildTime != Unknown {
		t.Errorf("resolve empty = %+v, want unknown commit/time", got)
	}
	if got.Version != DefaultVersion {
		t.Errorf("Version = %q, want the source default %q (an empty -X must not erase it)",
			got.Version, DefaultVersion)
	}
	if !strings.Contains(got.Summary(), "commit unknown") {
		t.Errorf("Summary = %q", got.Summary())
	}
}

func TestSummaryShowsDirtyTree(t *testing.T) {
	info := Info{Version: "v0.3.0", Commit: "abc1234", BuildTime: "2026-09-09T12:50:03Z",
		GoVersion: "go1.25.0", Platform: "linux/amd64", Modified: true}
	s := info.Summary()
	if !strings.HasPrefix(s, "v0.3.0+dirty (commit abc1234, built ") {
		t.Errorf("Summary = %q", s)
	}
	if !strings.HasSuffix(s, "go1.25.0 linux/amd64)") {
		t.Errorf("Summary = %q", s)
	}
}

func TestBuiltLocalPassesUnknownThrough(t *testing.T) {
	if got := (Info{BuildTime: Unknown}).BuiltLocal(); got != Unknown {
		t.Errorf("BuiltLocal() = %q, want %q", got, Unknown)
	}
	if got := (Info{BuildTime: "2026-09-09T12:50:03Z"}).BuiltLocal(); !strings.Contains(got, "2026-09-09 ") {
		t.Errorf("BuiltLocal() = %q, want a local date", got)
	}
}

// TestGetMatchesRuntime proves the real (non-stamped) build still reports
// sane values: version from source, commit/time from VCS or unknown.
func TestGetMatchesRuntime(t *testing.T) {
	got := Get()
	if got.Version == "" || got.GoVersion != runtime.Version() {
		t.Errorf("Get() = %+v", got)
	}
	if !strings.Contains(got.Platform, "/") {
		t.Errorf("Platform = %q", got.Platform)
	}
}
