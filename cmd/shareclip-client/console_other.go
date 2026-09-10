//go:build !windows

package main

// hideConsoleWindow 在非 Windows 平台上什么都不做：Linux 的桌面启动器
// （.desktop）本来就不会给程序开终端窗口。
func hideConsoleWindow() {}
