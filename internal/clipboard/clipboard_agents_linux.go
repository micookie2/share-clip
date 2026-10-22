//go:build linux

package clipboard

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// knownClipboardAgentNames lists guest-side "clipboard integration" daemons
// that mirror the clipboard to another system (virtual-machine host, cloud
// desktop, remote session). They only understand plain text, and they take
// ownership of the local clipboard shortly after every copy; doing so drops
// the HTML rendition, so a rich-text copy made locally turns into plain text
// locally (while share-clip still transports the rich version to peers).
//
// The names are matched case-insensitively against /proc/<pid>/comm.
var knownClipboardAgentNames = []string{
	"spice-vdagent", // SPICE / QEMU cloud desktops
	"vmtoolsd",      // VMware Tools
	"vboxclient",    // VirtualBox Guest Additions
	"vmware-user",   // older VMware Tools session helper
}

// knownClipboardAgents reports the known clipboard agents currently running,
// by scanning comm files under procRoot (/proc in production). The result is
// sorted and de-duplicated. A missing/unreadable procRoot yields nil: this is
// a diagnostic, never a reason to fail startup.
func knownClipboardAgents(procRoot string) []string {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil
	}
	known := make(map[string]bool, len(knownClipboardAgentNames))
	for _, name := range knownClipboardAgentNames {
		known[name] = true
	}
	seen := map[string]bool{}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		comm, err := os.ReadFile(filepath.Join(procRoot, entry.Name(), "comm"))
		if err != nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(comm)))
		if !known[name] || seen[name] {
			continue
		}
		seen[name] = true
		found = append(found, name)
	}
	sort.Strings(found)
	return found
}

// warnKnownClipboardAgents explains, in the client log, why a locally copied
// rich text may lose its formatting. The remote clipboard is unaffected:
// share-clip ignores the plain-text downgrade instead of broadcasting it.
func warnKnownClipboardAgents() {
	agents := knownClipboardAgents("/proc")
	if len(agents) == 0 {
		return
	}
	warnf("clipboard: 检测到剪贴板代理进程 %s：虚拟机/云桌面代理（如 spice-vdagent）只同步纯文本，"+
		"会在复制后接管本机剪贴板、丢弃富文本格式，因此本机粘贴可能只剩纯文本；"+
		"对端不受影响（share-clip 会忽略这种降级，继续保留对端的富文本）。"+
		"如不需要宿主机与虚拟机之间复制粘贴，可停用该代理。", strings.Join(agents, ", "))
}
