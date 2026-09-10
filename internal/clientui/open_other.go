//go:build !linux && !windows

package clientui

import (
	"errors"
	"os/exec"
)

// OpenBrowser 在本平台尝试用 open 打开 url；share-clip 的剪贴板后端也只在
// Windows 与 Linux 上可用，这里只是让其它平台上的编译与测试不至于中断。
func OpenBrowser(url string) error {
	if _, err := exec.LookPath("open"); err != nil {
		return errors.New("本平台没有可用的浏览器打开方式，请手动打开上面的地址")
	}
	cmd := exec.Command("open", url)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
