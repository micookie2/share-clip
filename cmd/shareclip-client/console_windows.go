//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

// hideConsoleWindow 藏掉 Windows 为「控制台子系统」程序自动分配的黑窗口。
//
// 客户端默认以控制台子系统编译（这样 `shareclip-client -s ... -v` 在终端里
// 依旧有输出），双击运行时 Windows 会先弹一个控制台窗口；托盘应用挂着这么个
// 黑框很难看，所以桌面模式启动时把它藏起来。
//
// 关键判断：只有当这个控制台「只属于我们自己」时才藏。从 cmd/PowerShell 里
// 启动时，控制台是终端进程的，GetConsoleProcessList 会看到不止一个进程——
// 这时绝不能动，否则会把用户的终端一起藏了。
func hideConsoleWindow() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getConsoleProcessList := kernel32.NewProc("GetConsoleProcessList")
	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")

	var pids [8]uint32
	n, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n != 1 {
		return
	}
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	const swHide = 0
	user32 := syscall.NewLazyDLL("user32.dll")
	showWindow := user32.NewProc("ShowWindow")
	showWindow.Call(hwnd, swHide)
}
