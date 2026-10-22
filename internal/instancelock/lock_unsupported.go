//go:build !unix && !windows

package instancelock

import (
	"fmt"
	"os"
	"runtime"
)

// acquireLock 在这些平台上没有可用的文件锁实现（例如 plan9、js/wasm）。
// 返回普通错误，调用方据此降级为「不加锁继续运行」，而不是拒绝启动。
func acquireLock(path string) (*os.File, error) {
	return nil, fmt.Errorf("instancelock: %s 平台暂不支持单实例锁（%s）", runtime.GOOS, path)
}

func releaseLock(*os.File) error { return nil }
