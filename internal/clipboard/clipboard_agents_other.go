//go:build !linux

package clipboard

// warnKnownClipboardAgents is a no-op outside Linux: the known clipboard
// agents this diagnostic covers (spice-vdagent, VMware Tools, VirtualBox Guest
// Additions) are Linux guest integrations. Windows remote-desktop clipboard
// helpers are out of scope for now.
func warnKnownClipboardAgents() {}
