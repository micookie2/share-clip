//go:build windows

package main

import (
	"log"
	"os"
	"syscall"
	"unsafe"
)

// Windows 上客户端按 GUI 子系统链接（见 Makefile 里的 -H=windowsgui）：GUI 子系统
// 的进程根本不会被分配控制台，双击运行时不会再冒出（哪怕是闪一下）黑窗口。
// 控制台子系统的构建做不到这一点——Windows 会先建好控制台，之后无论怎么隐藏，
// 在 Windows Terminal 作为默认终端的 Windows 11 上都可能留下一个可见的终端页签。
//
// 代价是进程默认没有任何标准流，因此这里补两件事：
//
//   - prepareConsole 在解析参数之前尝试附着到父进程的控制台：从 cmd/PowerShell
//     里运行时（以及控制台子系统的构建，例如不走 Makefile 直接 go build），
//     -h/-v 与启动日志照旧落在那个终端里；
//   - ensureConsole 在确定要跑控制台模式（-s / --console）却没有可用控制台时
//     自己开一个，免得日志无处可去。

// attachParentProcess 是 AttachConsole 的 ATTACH_PARENT_PROCESS，即 (DWORD)-1。
const attachParentProcess = ^uintptr(0)

var (
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	user32                    = syscall.NewLazyDLL("user32.dll")
	procAttachConsole         = kernel32.NewProc("AttachConsole")
	procAllocConsole          = kernel32.NewProc("AllocConsole")
	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	procShowWindow            = user32.NewProc("ShowWindow")
)

// consoleBound 记录标准流是否已经指向一个真实控制台，ensureConsole 据此决定
// 还要不要再开一个。
var consoleBound bool

// prepareConsole 在解析命令行参数之前调用：参数帮助（-h）、解析报错和版本号
// （-v）都走标准流，得先把它们接到父进程的控制台上才有地方可打印。
func prepareConsole() {
	if attachParentConsole() {
		consoleBound = true
		return
	}
	// 附不上父控制台：要么本来就在控制台子系统的构建里（直接 go build 出来的），
	// 要么是双击启动、压根没有控制台。前者若这个控制台只属于自己，就把黑窗口藏掉。
	hideOwnConsoleWindow()
}

// ensureConsole 保证有一个控制台可写：GUI 子系统的构建双击运行时没有父控制台
// （prepareConsole 也就没得附着），此时若用户显式要控制台模式，就自己开一个
// （窗口会一直留着，这正是控制台模式该有的样子）。
func ensureConsole() {
	if consoleBound {
		return
	}
	// 已经有控制台的构建（绕过 Makefile 直接 go build 的那种）在这里会失败，
	// 那就继续用原来那个，什么都不用做。
	if r, _, _ := procAllocConsole.Call(); r == 0 {
		return
	}
	consoleBound = bindStdHandles()
}

// attachParentConsole 附着到父进程的控制台并接管标准流。
func attachParentConsole() bool {
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r == 0 {
		return false
	}
	return bindStdHandles()
}

// bindStdHandles 把 Go 的标准流接到刚附上或新开的控制台上。标准库 log 在包
// 初始化时就抓住了当时的 os.Stderr，所以这里还得显式改一次它的输出目标。
func bindStdHandles() bool {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		in.Close()
		return false
	}
	os.Stdin, os.Stdout, os.Stderr = in, out, out
	log.SetOutput(out)
	return true
}

// hideOwnConsoleWindow 藏掉 Windows 为「控制台子系统」程序自动分配的黑窗口。
// GUI 子系统的构建不会有这个窗口，所以它只是绕过 Makefile 直接 go build 时的兜底。
//
// 关键判断：只有当这个控制台「只属于我们自己」时才藏。从 cmd/PowerShell 里
// 启动时，控制台是终端进程的，GetConsoleProcessList 会看到不止一个进程——
// 这时绝不能动，否则会把用户的终端一起藏了。
func hideOwnConsoleWindow() {
	var pids [8]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	if n != 1 {
		return
	}
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	const swHide = 0
	procShowWindow.Call(hwnd, swHide)
}
