// share-clip server: relays clipboard broadcasts between clients, persists a
// searchable history in SQLite and serves a small web UI.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/micookie2/share-clip/internal/cli"
	"github.com/micookie2/share-clip/internal/protocol"
	"github.com/micookie2/share-clip/internal/server"
)

const version = "0.1.0"

func main() {
	fs := cli.New("shareclip-server")
	var (
		addr         = fs.String("a", "addr", server.DefaultAddr, "监听地址，如 :9000 或 0.0.0.0:9000")
		dbPath       = fs.String("d", "db", server.DefaultDBPath, "SQLite 数据库文件路径（当前目录下）")
		historyLimit = fs.Int("l", "history-limit", server.DefaultHistory, "历史记录保留条数（超出自动清理最旧）")
		maxPayload   = fs.Int("m", "max-payload", protocol.DefaultMaxPayload, "单条剪贴板内容最大字节数")
		quiet        = fs.Bool("q", "quiet", false, "减少日志输出")
		showVersion  = fs.Bool("v", "version", false, "显示版本并退出")
	)
	fs.SetIntro(fmt.Sprintf(`share-clip server %s
用法: shareclip-server            # 全用默认值即可启动
     shareclip-server -a :9000 -l 500

WebSocket 端点: ws://<host>%s/ws
Web 管理页:     http://<host>%s/
每个选项都有等价的长写法（-a 即 --addr），见下面的列表。`, version,
		server.DefaultAddr, server.DefaultAddr))
	fs.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	s, err := server.New(server.Config{
		Addr:         *addr,
		DBPath:       *dbPath,
		HistoryLimit: *historyLimit,
		MaxPayload:   *maxPayload,
		Quiet:        *quiet,
	})
	if err != nil {
		log.Fatalf("启动失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("share-clip server %s 启动", version)
	log.Printf("历史数据库: %s（保留最近 %d 条）", *dbPath, *historyLimit)
	if err := s.ListenAndServe(ctx); err != nil {
		log.Fatalf("server 退出: %v", err)
	}
	log.Printf("server 已停止")
}
