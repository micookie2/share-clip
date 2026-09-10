package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// fileTimeLayout 是写入文件时使用的时间格式，与控制台的标准库前缀不同：
// 控制台用 "2006/01/02 15:04:05"，这里选了带连字符的写法，与 README、Web 页面
// 上的时间展示保持一致，便于日志与界面截图对照。
const fileTimeLayout = "2006-01-02 15:04:05"

// AttachFile 让日志同时追加写入 path，返回注销并关闭文件的函数（可重复调用）。
//
// 桌面模式的客户端没有可见的控制台，出问题时这个文件就是唯一的现场：它记录了
// 与界面日志视图完全相同的记录。maxBytes > 0 时，若文件在打开时已超过该大小，
// 会先改名为 <path>.old 再重新开始——客户端长期驻留托盘，日志不能无限增长。
//
// 写文件失败不会反过来写日志（那会递归调用自己），只在首次失败时静默放弃该行。
func AttachFile(path string, maxBytes int64) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建日志目录失败: %w", err)
	}
	if maxBytes > 0 {
		if fi, err := os.Stat(path); err == nil && fi.Size() > maxBytes {
			// 轮转失败不算致命：继续追加写总比没有日志强。
			_ = os.Rename(path, path+".old")
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件失败: %w", err)
	}

	var mu sync.Mutex // 日志来自多个 goroutine，写文件必须串行
	unsubscribe := Subscribe(func(e Entry) {
		line := e.At.Format(fileTimeLayout) + " " + e.Text + "\n"
		mu.Lock()
		defer mu.Unlock()
		if _, err := f.WriteString(line); err != nil {
			return
		}
	})

	var once sync.Once
	return func() {
		once.Do(func() {
			unsubscribe()
			mu.Lock()
			_ = f.Close()
			mu.Unlock()
		})
	}, nil
}
