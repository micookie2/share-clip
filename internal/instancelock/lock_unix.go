//go:build unix

package instancelock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// acquireLock 用 flock(2) 对锁文件加排他非阻塞锁。flock 锁跟着打开文件描述
// 走：进程退出（包括崩溃、被 kill）时内核自动释放，残留的锁文件不会挡住下次
// 启动，所以不需要清理逻辑。
func acquireLock(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开单实例锁文件 %s 失败: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		// EWOULDBLOCK/EAGAIN：另一个进程正持有这把锁。
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("锁定单实例锁文件 %s 失败: %w", path, err)
	}
	return f, nil
}

// releaseLock 解锁并关闭锁文件；关闭本身也会释放 flock，显式解锁只是让同一
// 进程内紧接着的 Acquire 立即成功。
func releaseLock(f *os.File) error {
	flockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	closeErr := f.Close()
	if flockErr != nil {
		return fmt.Errorf("解锁单实例锁文件失败: %w", flockErr)
	}
	return closeErr
}
