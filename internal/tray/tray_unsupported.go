//go:build !windows && !linux

package tray

// 本文件给 Windows/Linux 之外的平台（macOS、BSD 等）提供空实现，让 client 的
// main 可以无条件引用本包。macOS 的 fyne.io/systray 需要 cgo，本仓库暂不支持，
// 所以一并不实现。

import "errors"

// Available 报告本机是否有可用的托盘宿主；这些平台恒为 false。
func Available() bool { return false }

// Run 在这些平台不阻塞，直接返回清晰的错误；调用方应回退到无托盘模式。
func Run(Config) error {
	return errors.New("当前平台暂不支持系统托盘（仅 Windows 和 Linux 提供），请改用前台运行或信号退出")
}

// Quit 在这些平台是空操作，与托盘版的“可重复、可在 Run 之前调用”保持一致。
func Quit() {}

// 没有界面可更新，apply* 保持空实现。
func applyStatus()  {}
func applyEnabled() {}
func applyTooltip() {}
