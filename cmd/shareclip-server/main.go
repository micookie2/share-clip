// share-clip server: relays clipboard broadcasts between clients, persists a
// searchable history in SQLite and serves a small web UI.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/cli"
	"github.com/micookie2/share-clip/internal/logx"
	"github.com/micookie2/share-clip/internal/protocol"
	"github.com/micookie2/share-clip/internal/server"
	"github.com/micookie2/share-clip/internal/systemd"
)

// installIntro 是 `shareclip-server install -h` 的说明。
const installIntro = `share-clip server 安装为 systemd 服务

用法: sudo shareclip-server install        # 系统级（/etc/systemd/system，开机自启）
      shareclip-server install --user      # 用户级（~/.config/systemd/user，无需 root）
      shareclip-server install --dry-run   # 只打印单元文件与命令，不落盘

安装的是当前正在运行的可执行文件本身：请先 go build 到固定路径，
不要直接用 go run 的临时产物，那个文件重启后会被清理。

启动参数：-a/-d/-l/-m 就是写进单元 ExecStart 的服务器参数；服务器上
其它参数（以及将来新增的参数）用可重复的 --exec-arg 追加，例如：
  shareclip-server install --exec-arg=-q
鉴权默认开启，key 每次启动随机生成并写进 journal（用下面的 journalctl
命令查看，搜「访问 key」）；要固定 key 用 --exec-arg=-k --exec-arg=<key>，
要彻底关闭鉴权用 --no-auth。
需要更复杂的改动（环境变量、资源限制等）时，用 systemd 原生的覆盖：
  sudo systemctl edit <单元名>   # 写 [Service] 段，改 ExecStart 前先写一行空的 ExecStart=

安装后查看状态: systemctl [--user] status <单元名>
跟踪日志:       journalctl [--user] -u <单元名> -f
卸载服务:       shareclip-server uninstall [--user]`

// uninstallIntro 是 `shareclip-server uninstall -h` 的说明。
const uninstallIntro = `share-clip server 从 systemd 卸载

用法: sudo shareclip-server uninstall       # 卸载系统级服务
      shareclip-server uninstall --user     # 卸载用户级服务
      shareclip-server uninstall --purge    # 同时删除数据目录（数据库）

卸载后查看状态: systemctl [--user] status <单元名>
跟踪日志:       journalctl [--user] -u <单元名> -f`

func main() {
	// 子命令必须在普通选项解析之前分派：只有 os.Args[1] 恰好是 install 或
	// uninstall 时才走 systemd 路径，其余参数组合（含无参数、-v、-h）的行为
	// 与从前完全一致。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			runInstall(os.Args[2:])
			return
		case "uninstall":
			runUninstall(os.Args[2:])
			return
		}
	}
	runServer()
}

// runServer 保持原有的启动行为：解析普通选项、打印版本或直接起服务。
func runServer() {
	build := buildinfo.Get()
	fs := cli.New("shareclip-server")
	var (
		addr         = fs.String("a", "addr", server.DefaultAddr, "监听地址，如 :9000 或 0.0.0.0:9000")
		dbPath       = fs.String("d", "db", server.DefaultDBPath, "SQLite 数据库文件路径（当前目录下）")
		historyLimit = fs.Int("l", "history-limit", server.DefaultHistory, "历史记录保留条数（超出自动清理最旧）")
		maxPayload   = fs.Int("m", "max-payload", protocol.DefaultMaxPayload, "单条剪贴板内容最大字节数")
		key          = fs.String("k", "key", "", "固定访问 key；留空则每次启动随机生成并打印到日志")
		noAuth       = fs.Bool("", "no-auth", false, "关闭访问鉴权（仅限完全可信的内网，不建议）")
		quiet        = fs.Bool("q", "quiet", false, "减少日志输出")
		showVersion  = fs.Bool("v", "version", false, "显示版本号与构建信息后退出")
	)
	fs.SetIntro(fmt.Sprintf(`share-clip server %s
用法: shareclip-server            # 全用默认值即可启动
     shareclip-server -a :9000 -l 500

WebSocket 端点: ws://<host>%s/ws
Web 管理页:     http://<host>%s/

鉴权: 默认每次启动随机生成一个访问 key 并打印在下面几行日志里。
客户端要用同一个 key 才能连接，浏览器首次打开管理页会要求填写它。
若要在内网固定 key（便于脚本/开机自启），用 -k 指定；完全不要鉴权用 --no-auth。
每个选项都有等价的长写法（-a 即 --addr），见下面的列表。

子命令:
  shareclip-server install [选项]    安装为 systemd 服务（开机自启）
  shareclip-server uninstall [选项]  卸载 systemd 服务
  两个子命令都用 -h 查看各自选项；--dry-run 只打印计划，不落盘、不调 systemctl。`, build.Version,
		server.DefaultAddr, server.DefaultAddr))
	fs.Parse()
	if *showVersion {
		fmt.Println(build.Summary())
		return
	}

	s, err := server.New(server.Config{
		Addr:         *addr,
		DBPath:       *dbPath,
		HistoryLimit: *historyLimit,
		MaxPayload:   *maxPayload,
		Key:          *key,
		NoAuth:       *noAuth,
		Quiet:        *quiet,
	})
	if err != nil {
		logx.Fatalf("启动失败: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logx.Printf("share-clip server %s 启动（commit %s，构建于 %s）",
		build.Version, build.Commit, build.BuiltLocal())
	logx.Printf("历史数据库: %s（保留最近 %d 条）", *dbPath, *historyLimit)
	printAuthInfo(s, *noAuth, *addr)
	if err := s.ListenAndServe(ctx); err != nil {
		logx.Fatalf("server 退出: %v", err)
	}
	logx.Printf("server 已停止")
}

// printAuthInfo 在启动日志里把访问 key 明确打出来——这是用户拿到 key 的唯一
// 正常途径（server 不落盘、界面也要先有 key 才能进）。
func printAuthInfo(s *server.Server, noAuth bool, addr string) {
	if noAuth {
		logx.Printf("警告: 已用 --no-auth 关闭鉴权，任何能访问该端口的人都能连接、查看历史")
		return
	}
	if s.KeyGenerated() {
		logx.Printf("访问 key（本次启动随机生成，重启后会变）: %s", s.AccessKey())
	} else {
		logx.Printf("访问 key（由 --key 指定）: %s", s.AccessKey())
	}
	logx.Printf("客户端连接与管理页都需要这个 key；浏览器首次打开 http://<host>%s/ 会要求填写", addr)
}

// runInstall 处理 `shareclip-server install [选项]`。
func runInstall(args []string) {
	fs := cli.New("shareclip-server install")
	var (
		addr         = fs.String("a", "addr", server.DefaultAddr, "监听地址，写进单元的 ExecStart")
		dbPath       = fs.String("d", "db", "", "SQLite 数据库路径；留空按作用域推导")
		historyLimit = fs.Int("l", "history-limit", server.DefaultHistory, "历史记录保留条数")
		maxPayload   = fs.Int("m", "max-payload", protocol.DefaultMaxPayload, "单条剪贴板内容最大字节数")
		user         = fs.Bool("u", "user", false, "安装为用户级服务（~/.config/systemd/user，无需 root）")
		name         = fs.String("n", "name", systemd.DefaultUnitName(), "单元名（不含 .service）")
		runAs        = fs.String("", "run-as", "", "系统级服务以该用户运行；留空则 DynamicUser=yes")
		noAuth       = fs.Bool("", "no-auth", false, "写进单元的 ExecStart：关闭 server 的访问鉴权")
		extraArgs    = fs.Strings("", "exec-arg", "追加到 ExecStart 的额外启动参数，如 --exec-arg=-q")
		unitDir      = fs.String("", "unit-dir", "", "覆盖单元文件目录（默认按作用域）")
		noStart      = fs.Bool("", "no-start", false, "只 enable，不立即启动")
		force        = fs.Bool("f", "force", false, "覆盖已存在的单元文件")
		dryRun       = fs.Bool("", "dry-run", false, "只打印单元文件与将执行的命令")
	)
	fs.SetIntro(installIntro)
	fs.ParseArgs(args)

	o := systemd.Options{
		Scope:        scopeOf(*user),
		Unit:         *name,
		Addr:         *addr,
		DBPath:       *dbPath,
		HistoryLimit: *historyLimit,
		MaxPayload:   *maxPayload,
		RunAs:        *runAs,
		NoAuth:       *noAuth,
		ExtraArgs:    *extraArgs,
		UnitDir:      *unitDir,
		NoStart:      *noStart,
		Force:        *force,
	}
	o.ExecPath = executablePath()
	if o.Scope == systemd.ScopeUser && o.RunAs != "" {
		logx.Printf("提示: 用户级服务不使用 --run-as，已忽略 %q", o.RunAs)
		o.RunAs = ""
	}
	if strings.Contains(o.ExecPath, "go-build") {
		logx.Printf("警告: 当前可执行文件在 go run 临时目录（%s），重启后会失效，请先 go build 到固定路径", o.ExecPath)
	}

	if *dryRun {
		printPlan(o, systemd.ActionInstall)
		return
	}
	if err := systemd.Install(context.Background(), o, os.Stdout); err != nil {
		logx.Fatalf("安装 systemd 服务失败: %v", err)
	}
	reportInstall(o)
}

// runUninstall 处理 `shareclip-server uninstall [选项]`。
func runUninstall(args []string) {
	fs := cli.New("shareclip-server uninstall")
	var (
		user    = fs.Bool("u", "user", false, "卸载用户级服务（~/.config/systemd/user）")
		name    = fs.String("n", "name", systemd.DefaultUnitName(), "单元名（不含 .service）")
		unitDir = fs.String("", "unit-dir", "", "覆盖单元文件目录（默认按作用域）")
		purge   = fs.Bool("", "purge", false, "同时删除数据目录（数据库）")
		dryRun  = fs.Bool("", "dry-run", false, "只打印将删除的文件与将执行的命令")
	)
	fs.SetIntro(uninstallIntro)
	fs.ParseArgs(args)

	o := systemd.Options{
		Scope:   scopeOf(*user),
		Unit:    *name,
		UnitDir: *unitDir,
		Purge:   *purge,
	}
	if *dryRun {
		printPlan(o, systemd.ActionUninstall)
		return
	}
	if err := systemd.Uninstall(context.Background(), o, os.Stdout); err != nil {
		logx.Fatalf("卸载 systemd 服务失败: %v", err)
	}
	if unitPath, err := systemd.UnitPath(o); err == nil {
		logx.Printf("卸载完成: 已移除单元 %s", unitPath)
	}
	if *purge {
		logx.Printf("已请求删除数据目录: %s", systemd.StateDir(o))
	}
}

// scopeOf 把 --user 开关翻译成作用域。
func scopeOf(user bool) systemd.Scope {
	if user {
		return systemd.ScopeUser
	}
	return systemd.ScopeSystem
}

// executablePath 返回当前可执行文件的绝对路径，供单元里的 ExecStart 使用。
func executablePath() string {
	exe, err := os.Executable()
	if err != nil {
		logx.Fatalf("无法确定当前可执行文件路径: %v", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		logx.Fatalf("无法解析可执行文件的绝对路径: %v", err)
	}
	return abs
}

// printPlan 打印 --dry-run 计划：install 会打印单元文件全文，两者都打印将要
// 执行的命令；全程不写文件、不调用 systemctl。
func printPlan(o systemd.Options, action systemd.Action) {
	unitPath, unitText, cmds, err := systemd.Plan(o, action)
	if err != nil {
		logx.Fatalf("生成 systemd 安装计划失败: %v", err)
	}
	if action == systemd.ActionUninstall {
		logx.Printf("dry-run: 将删除单元 %s", unitPath)
		if o.Purge {
			logx.Printf("dry-run: 将删除数据目录 %s", systemd.StateDir(o))
		}
	} else {
		logx.Printf("dry-run: 将写入单元 %s", unitPath)
		fmt.Print(unitText)
	}
	logx.Printf("dry-run: 将执行以下命令")
	for _, argv := range cmds {
		logx.Printf("  %s", strings.Join(argv, " "))
	}
	logx.Printf("dry-run: 未写入任何文件，也未调用 systemctl")
}

// reportInstall 打印安装成功后的单元路径、数据路径与常用排障命令。
func reportInstall(o systemd.Options) {
	unitPath, err := systemd.UnitPath(o)
	if err != nil {
		return
	}
	db := o.DBPath
	if db == "" {
		db = systemd.DefaultDBPath(o)
	}
	logx.Printf("安装完成: 单元文件 %s", unitPath)
	logx.Printf("数据目录 %s，数据库 %s", systemd.StateDir(o), db)
	logx.Printf("查看状态: %s", strings.Join(systemctlArgv(o, "status", unitName(o)), " "))
	logx.Printf("查看日志: %s", strings.Join(journalArgv(o, unitName(o)), " "))
}

// unitName 返回本次使用的单元名（不含 .service）。
func unitName(o systemd.Options) string {
	if o.Unit != "" {
		return o.Unit
	}
	return systemd.DefaultUnitName()
}

// systemctlArgv 拼出面向用户展示的 systemctl 命令。
func systemctlArgv(o systemd.Options, args ...string) []string {
	argv := []string{"systemctl"}
	if o.Scope == systemd.ScopeUser {
		argv = append(argv, "--user")
	}
	return append(argv, args...)
}

// journalArgv 拼出面向用户展示的 journalctl 命令。
func journalArgv(o systemd.Options, unit string) []string {
	argv := []string{"journalctl"}
	if o.Scope == systemd.ScopeUser {
		argv = append(argv, "--user")
	}
	return append(argv, "-u", unit, "-f")
}
