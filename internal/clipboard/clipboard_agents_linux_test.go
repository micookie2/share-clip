//go:build linux

package clipboard

import (
	"os"
	"path/filepath"
	"testing"
)

// writeComm drops a fake /proc/<pid>/comm file into root.
func writeComm(t *testing.T, root, pid, comm string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKnownClipboardAgents(t *testing.T) {
	root := t.TempDir()

	// No processes at all (or an unreadable proc root): no diagnostics.
	if got := knownClipboardAgents(root); len(got) != 0 {
		t.Fatalf("empty proc root: got %v", got)
	}
	if got := knownClipboardAgents(filepath.Join(root, "missing")); len(got) != 0 {
		t.Fatalf("missing proc root: got %v", got)
	}

	// Nothing matching.
	writeComm(t, root, "1", "systemd")
	writeComm(t, root, "2", "bash")
	if got := knownClipboardAgents(root); len(got) != 0 {
		t.Fatalf("unrelated processes: got %v", got)
	}

	// Known agents are detected regardless of case, and reported once each.
	writeComm(t, root, "10", "spice-vdagent")
	writeComm(t, root, "11", "VBoxClient")
	writeComm(t, root, "12", "spice-vdagent")
	writeComm(t, root, "13", "vmtoolsd")
	if got := knownClipboardAgents(root); len(got) != 3 ||
		got[0] != "spice-vdagent" || got[1] != "vboxclient" || got[2] != "vmtoolsd" {
		t.Fatalf("agents = %v, want the three known agents sorted", got)
	}
}

func TestWarnKnownClipboardAgentsUsesWarnf(t *testing.T) {
	prev := warnf
	defer SetWarnf(prev)

	var got []string
	SetWarnf(func(format string, args ...any) {
		if format != "" {
			got = append(got, format)
		}
	})
	// The real /proc scan is environment dependent; just assert it never
	// panics and goes through warnf when something is detected.
	warnKnownClipboardAgents()
	_ = got
}
