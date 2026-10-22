//go:build unix

package instancelock

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// tempLockPath 返回一个位于尚不存在的子目录下的锁文件路径，顺带覆盖
// Acquire 自动创建配置目录的行为。
func tempLockPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "share-clip", "client.lock")
}

func TestAcquireExcludesSecondInstance(t *testing.T) {
	path := tempLockPath(t)
	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("首次 Acquire: %v", err)
	}
	defer release()

	// flock(2) 按「打开文件描述」而不是按进程判重：同一进程里再 open 一次
	// 得到独立的描述，第二次 LOCK_EX|LOCK_NB 会失败，所以这个用例能在同一个
	// 测试进程里真实验证排他性。
	release2, err := Acquire(path)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("第二次 Acquire 错误 = %v，期望 ErrAlreadyRunning", err)
	}
	if release2 != nil {
		t.Fatal("Acquire 失败时不应返回 release 函数")
	}
}

func TestReleaseThenReacquire(t *testing.T) {
	path := tempLockPath(t)
	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("首次 Acquire: %v", err)
	}
	release()
	release() // 幂等：重复调用不能 panic

	release2, err := Acquire(path)
	if err != nil {
		t.Fatalf("释放后再次 Acquire: %v", err)
	}
	release2()
}

func TestLockFilePermissions(t *testing.T) {
	path := tempLockPath(t)
	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("锁文件不存在: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Fatalf("锁文件权限 = %o，期望 0600", got)
	}
}

func TestAcquireDifferentPaths(t *testing.T) {
	releaseA, err := Acquire(tempLockPath(t))
	if err != nil {
		t.Fatalf("Acquire A: %v", err)
	}
	defer releaseA()
	releaseB, err := Acquire(tempLockPath(t))
	if err != nil {
		t.Fatalf("Acquire B: %v", err)
	}
	defer releaseB()
}

func TestAcquireIgnoresStaleLockFile(t *testing.T) {
	path := tempLockPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	// 上次崩溃留下的锁文件（没有进程持锁）：不应阻止启动。
	if err := os.WriteFile(path, []byte("stale\n"), 0o600); err != nil {
		t.Fatalf("写旧锁文件: %v", err)
	}
	release, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire 旧锁文件: %v", err)
	}
	release()
}

func TestAcquireEmptyPath(t *testing.T) {
	if _, err := Acquire(""); err == nil {
		t.Fatal("空路径应返回错误")
	}
}
