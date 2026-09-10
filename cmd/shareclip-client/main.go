// share-clip client: watches the local clipboard and shares text/image
// content through the share-clip server with every other running client.
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/micookie2/share-clip/internal/agent"
	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/cli"
	"github.com/micookie2/share-clip/internal/logx"
	"github.com/micookie2/share-clip/internal/protocol"
)

func main() {
	build := buildinfo.Get()
	fs := cli.New("shareclip-client")
	var (
		serverAddr = fs.String("s", "server", "", "share-clip server 地址，如 192.168.1.10:9000（必填）")
		name       = fs.String("n", "name", "", "本机显示名（默认使用主机名）")
		pollMs     = fs.Int("p", "poll", 1000, "剪贴板监听兜底轮询间隔（毫秒）：X11 默认由 XFixes 复制事件驱动，仅事件不可用时按此轮询；Wayland/GNOME 等无事件通道的平台按此轮询")
		maxPayload = fs.Int("m", "max-payload", protocol.DefaultMaxPayload, "单条剪贴板内容最大字节数")
		quiet      = fs.Bool("q", "quiet", false, "减少日志输出")
		showVer    = fs.Bool("v", "version", false, "显示版本号与构建信息后退出")
	)
	fs.SetIntro(fmt.Sprintf(`share-clip client %s
用法: shareclip-client -s <host:port>

启动后本机每次复制文本或图片都会广播到 server 上的其它客户端；
收到的广播会自动写入本机剪贴板。文件复制暂不支持，会被忽略。
每个选项都有等价的长写法（-s 即 --server），见下面的列表。`, build.Version))
	fs.Parse()
	if *showVer {
		fmt.Println(build.Summary())
		return
	}
	if *serverAddr == "" {
		logx.Fatal("请用 -s 指定 server 地址，例如 shareclip-client -s 192.168.1.10:9000")
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	displayName := *name
	if displayName == "" {
		displayName = host
	}

	cfg := agent.Config{
		ServerAddr:     *serverAddr,
		ClientID:       fmt.Sprintf("%s-%d-%s", host, os.Getpid(), randHex(4)),
		Name:           displayName,
		PollIntervalMs: *pollMs,
		MaxPayload:     *maxPayload,
		Quiet:          *quiet,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	logx.Printf("share-clip client %s（%s）启动，server: %s（commit %s，构建于 %s）",
		build.Version, displayName, *serverAddr, build.Commit, build.BuiltLocal())
	if err := agent.Run(ctx, cfg); err != nil {
		logx.Fatalf("client 退出: %v", err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "randfail"
	}
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0x0f]
	}
	return string(out)
}
