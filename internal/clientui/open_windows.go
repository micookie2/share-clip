//go:build windows

package clientui

import (
	"os/exec"
	"syscall"
)

// OpenBrowser 用默认浏览器打开 url。
//
// rundll32 的 FileProtocolHandler 是 Windows 上「用关联程序打开 URL」最省事
// 的入口：不经过 cmd.exe，因此不会闪一个黑窗口。
func OpenBrowser(url string) error {
	if err := startDetached("rundll32.exe", "url.dll,FileProtocolHandler", url); err == nil {
		return nil
	}
	return startDetached("cmd.exe", "/c", "start", "", url)
}

// startDetached 起一个与客户端生命周期无关的进程（不等待、不共享控制台）。
func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
