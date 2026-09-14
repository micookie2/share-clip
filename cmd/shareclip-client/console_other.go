//go:build !windows

package main

// prepareConsole 在非 Windows 平台上什么都不做：进程一启动就有可用的标准流，
// Linux 的桌面启动器（.desktop）也不会给程序开终端窗口。
func prepareConsole() {}

// ensureConsole 同上：非 Windows 平台不需要为控制台模式另开控制台。
func ensureConsole() {}
