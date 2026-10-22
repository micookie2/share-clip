// Package instancelock 提供按用户生效的单实例锁：同一用户在同一台机器上只能
// 运行一个 share-clip 客户端。两个客户端会同时监听同一块系统剪贴板，并把对方
// 的广播再写回剪贴板，表现为重复发送和格式丢失（Windows 上尤其明显），所以
// 客户端启动前必须先拿到这把锁。
//
// 锁是进程级的：进程正常退出时显式释放，崩溃时由内核自动释放（Unix 是
// flock，Windows 是 LockFileEx），残留的锁文件本身不会阻止下一次启动。
package instancelock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrAlreadyRunning 表示锁已被另一个存活的客户端进程持有。调用方用
// errors.Is 判断它，避免依赖错误文案。
var ErrAlreadyRunning = errors.New("已有另一个 share-clip 客户端实例在运行")

// Acquire 取得 path 上的排他单实例锁，成功时返回一个幂等的 release 函数。
//
// 锁在进程生命周期内有效：可以显式调用 release（退出流程里立刻释放），进程
// 退出或崩溃时内核也会自动释放，因此陈旧的锁文件不会阻止下次启动。
//
// 如果同一个 path 上已有另一个进程持锁，返回 ErrAlreadyRunning。同一个进程
// 对同一个 path 再调用一次同样会返回它：第二次 open 得到的是独立的打开文件
// 描述，内核按它判重（不是按进程判重），测试里正是利用这一点验证排他性。
func Acquire(path string) (release func(), err error) {
	if path == "" {
		return nil, errors.New("instancelock: 锁文件路径为空")
	}
	// 配置目录可能还不存在（首次运行、还没保存过配置）：锁文件自己建目录，
	// 权限与配置目录保持一致。
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("创建单实例锁目录 %s 失败: %w", dir, err)
		}
	}
	f, err := acquireLock(path)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = releaseLock(f)
		})
	}, nil
}
