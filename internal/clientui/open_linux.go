//go:build linux

package clientui

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

// OpenBrowser 用默认浏览器打开 url。
//
// 优先用 $BROWSER（用户在桌面环境里显式指定的偏好），否则用 xdg-open——
// 它是 XDG 桌面上的标准入口，不需要我们去猜 Firefox 还是 Chromium。
func OpenBrowser(url string) error {
	if b := strings.TrimSpace(os.Getenv("BROWSER")); b != "" {
		// $BROWSER 允许写成带参数的命令行，取第一个词即可。
		fields := strings.Fields(b)
		if len(fields) > 0 {
			if err := start(fields[0], append(fields[1:], url)...); err == nil {
				return nil
			}
		}
	}
	if _, err := exec.LookPath("xdg-open"); err != nil {
		return errors.New("没找到 xdg-open，请手动在浏览器里打开上面的地址")
	}
	return start("xdg-open", url)
}

// start 起一个不等待的进程：浏览器活得比这次调用久，父进程也不该被它拖住。
func start(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
