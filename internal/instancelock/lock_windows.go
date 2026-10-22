//go:build windows

package instancelock

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// 与 internal/clipboard 的 Win32 调用风格一致：只用 syscall.NewLazyDLL 按需
// 解析 kernel32 的导出函数，不引入新的依赖。
var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY
	lockFileExFlags = 0x00000002 | 0x00000001
	// ERROR_LOCK_VIOLATION：请求的字节范围已被另一个进程锁住。
	errorLockViolation = syscall.Errno(33)
)

// overlapped 对应 Win32 的 OVERLAPPED 结构，用来指定要锁定的字节范围（这里
// 只锁文件第一个字节）。字段布局必须与 Windows 头文件一致，64 位与 32 位
// 下都是 4 个指针宽度的字段组合。
type overlapped struct {
	internal     uintptr
	internalHigh uintptr
	offset       uint32
	offsetHigh   uint32
	hEvent       syscall.Handle
}

// acquireLock 用 LockFileEx 对锁文件加排他非阻塞锁。锁跟着文件句柄走：进程
// 退出或崩溃时内核自动释放，残留的锁文件不会挡住下次启动。
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开单实例锁文件 %s 失败: %w", path, err)
	}
	var ol overlapped
	r1, _, e1 := procLockFileEx.Call(
		f.Fd(),
		uintptr(lockFileExFlags),
		0, // dwReserved
		1, // nNumberOfBytesToLockLow：锁住第一个字节即可
		0, // nNumberOfBytesToLockHigh
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		_ = f.Close()
		if e1 == errorLockViolation {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("锁定单实例锁文件 %s 失败: %w", path, e1)
	}
	return f, nil
}

// releaseLock 解锁并关闭锁文件；关闭句柄本身也会释放文件锁。
func releaseLock(f *os.File) error {
	var ol overlapped
	r1, _, e1 := procUnlockFileEx.Call(
		f.Fd(),
		0, // dwReserved
		1, // nNumberOfBytesToUnlockLow
		0, // nNumberOfBytesToUnlockHigh
		uintptr(unsafe.Pointer(&ol)),
	)
	closeErr := f.Close()
	if r1 == 0 {
		return fmt.Errorf("解锁单实例锁文件失败: %w", e1)
	}
	return closeErr
}
