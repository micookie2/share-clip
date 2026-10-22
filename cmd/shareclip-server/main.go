// share-clip server: relays clipboard broadcasts between clients, persists a
// searchable history in SQLite and serves a small web UI.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/cli"
	"github.com/micookie2/share-clip/internal/keyfile"
	"github.com/micookie2/share-clip/internal/logx"
	"github.com/micookie2/share-clip/internal/prompt"
	"github.com/micookie2/share-clip/internal/protocol"
	"github.com/micookie2/share-clip/internal/server"
	"github.com/micookie2/share-clip/internal/systemd"
)

// installIntro 是 `shareclip-server install -h` 的说明。
const installIntro = `share-clip server 安装为 systemd 服务（默认交互式提问）

用法: sudo shareclip-server install          # 系统级（/etc/systemd/system，开机自启）
      shareclip-server install --user        # 用户级（~/.config/systemd/user，无需 root）
      sudo shareclip-server install --yes    # 不提问，按命令行参数与默认值直接安装
      shareclip-server install --dry-run     # 只打印单元文件与命令，不落盘

交互模式下逐项询问安装位置、监听端口、数据目录、历史条数、鉴权方式等设置，
[方括号] 里是默认值，直接回车即采用；命令行上已经显式给出的选项不再提问。
标准输入不是终端（管道、CI 环境）时自动跳过全部提问，效果等同 --yes。

安装会把当前正在运行的可执行文件复制到安装位置（默认 /opt/share-clip/，
用户级默认 ~/.local/bin），单元里的 ExecStart 指向该副本；日后换构建
产物重新跑一次 install 即完成升级，无需手工维护路径。

启动参数：-a/-d/-l/-m 就是写进单元 ExecStart 的服务器参数；服务器上
其它参数（以及将来新增的参数）用可重复的 --exec-arg 追加，例如：
  shareclip-server install --exec-arg=-q
鉴权默认开启：安装时自动生成一个固定 key，保存到与可执行文件同目录的
shareclip.key（权限 0600），此后每次启动都用同一个 key；单元通过
EnvironmentFile= 注入，key 不会出现在单元文件、journal 或 ps 里。
查看或更换 key 用 shareclip-server key（-r 重新生成、--set 指定）；
安装时就想要指定值用 -k <key>（写入 key 文件）；彻底关闭鉴权用 --no-auth。
需要更复杂的改动（环境变量、资源限制等）时，用 systemd 原生的覆盖：
  sudo systemctl edit <单元名>   # 写 [Service] 段，改 ExecStart 前先写一行空的 ExecStart=

安装后查看状态: systemctl [--user] status <单元名>
跟踪日志:       journalctl [--user] -u <单元名> -f
查看访问 key:   shareclip-server key
卸载服务:       shareclip-server uninstall [--user] [--purge]`

// uninstallIntro 是 `shareclip-server uninstall -h` 的说明。
const uninstallIntro = `share-clip server 从 systemd 卸载

用法: sudo shareclip-server uninstall       # 卸载系统级服务
      shareclip-server uninstall --user     # 卸载用户级服务
      shareclip-server uninstall --purge    # 同时删除数据目录、可执行文件、数据库与 key 文件

卸载只停服务并移除单元文件；install 复制到安装位置的可执行文件、数据库与
key 文件默认保留（下次 install 会沿用 key），加 --purge 一并删除，不可恢复。

卸载后查看状态: systemctl [--user] status <单元名>
跟踪日志:       journalctl [--user] -u <单元名> -f`

// keyIntro 是 `shareclip-server key -h` 的说明。
const keyIntro = `share-clip 访问 key 的查看与更新

用法: sudo shareclip-server key              # 显示当前 key 与文件位置
      sudo shareclip-server key -r           # 生成新的随机 key（写入并重启服务）
      sudo shareclip-server key --set <key>  # 设置为指定 key
      shareclip-server key --user            # 按用户级单元定位

key 文件在安装时自动生成，与可执行文件同目录（默认 /opt/share-clip/shareclip.key，
权限 0600），systemd 单元通过 EnvironmentFile= 把它注入服务进程；重新 install
会沿用现有 key。定位 key 文件的顺序：--key-file 指定 > 已安装单元里的
EnvironmentFile > 当前程序所在目录的 shareclip.key。

更新 key 后默认自动重启对应服务（--no-restart 关闭）；换 key 意味着所有客户端
与管理页都要改用新 key 才能连上。`

func main() {
	// 子命令必须在普通选项解析之前分派：只有 os.Args[1] 恰好是 install、
	// uninstall 或 key 时才走对应路径，其余参数组合（含无参数、-v、-h）的
	// 行为与从前完全一致。
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install":
			runInstall(os.Args[2:])
			return
		case "uninstall":
			runUninstall(os.Args[2:])
			return
		case "key":
			runKey(os.Args[2:])
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
		addr         = fs.String("a", "addr", server.DefaultAddr, "监听地址，如 :9090 或 0.0.0.0:9090")
		dbPath       = fs.String("d", "db", server.DefaultDBPath, "SQLite 数据库文件路径（当前目录下）")
		historyLimit = fs.Int("l", "history-limit", server.DefaultHistory, "历史记录保留条数（超出自动清理最旧）")
		maxPayload   = fs.Int("m", "max-payload", protocol.DefaultMaxPayload, "单条剪贴板内容最大字节数")
		key          = fs.String("k", "key", "", "固定访问 key；留空则按 -K/环境变量/随机的顺序取")
		keyFileFlag  = fs.String("K", "key-file", "", "从文件读取访问 key（install 生成的 shareclip.key 即此格式）")
		noAuth       = fs.Bool("", "no-auth", false, "关闭访问鉴权（仅限完全可信的内网，不建议）")
		quiet        = fs.Bool("q", "quiet", false, "减少日志输出")
		showVersion  = fs.Bool("v", "version", false, "显示版本号与构建信息后退出")
	)
	fs.SetIntro(fmt.Sprintf(`share-clip server %s
 用法: shareclip-server            # 全用默认值即可启动
      shareclip-server -a :9090 -l 500

WebSocket 端点: ws://<host>%s/ws
Web 管理页:     http://<host>%s/

鉴权 key 的取值顺序：-k 指定 > -K/--key-file 文件 > 环境变量 SHARECLIP_KEY >
每次启动随机生成并打印在下面几行日志里。客户端要用同一个 key 才能连接，
浏览器首次打开管理页会要求填写它。install 子命令会生成并固定使用
与程序同目录的 shareclip.key（查看/更换：shareclip-server key）。
完全不要鉴权用 --no-auth。每个选项都有等价的长写法（-a 即 --addr），见下面的列表。

子命令:
  shareclip-server install [选项]    安装为 systemd 服务（交互式，开机自启）
  shareclip-server uninstall [选项]  卸载 systemd 服务
  shareclip-server key [-r|--set k]  查看/更新固定访问 key
  三个子命令都用 -h 查看各自选项；--dry-run 只打印计划，不落盘、不调 systemctl。`, build.Version,
		server.DefaultAddr, server.DefaultAddr))
	fs.Parse()
	if *showVersion {
		fmt.Println(build.Summary())
		return
	}
	if *key != "" && *keyFileFlag != "" {
		logx.Fatalf("-k 与 -K/--key-file 不能同时指定")
	}

	// key 解析顺序：-k > -K 文件 > SHARECLIP_KEY 环境变量（systemd 注入）> 随机。
	keyValue, keyFrom := *key, ""
	switch {
	case keyValue != "":
		keyFrom = "--key"
	case *keyFileFlag != "":
		v, err := keyfile.Read(expandTilde(*keyFileFlag))
		if err != nil {
			logx.Fatalf("读取 key 文件失败 %s: %v", *keyFileFlag, err)
		}
		keyValue, keyFrom = v, "-K "+*keyFileFlag
	default:
		if v := strings.TrimSpace(os.Getenv("SHARECLIP_KEY")); v != "" {
			keyValue, keyFrom = v, "环境变量 SHARECLIP_KEY"
		}
	}

	s, err := server.New(server.Config{
		Addr:         *addr,
		DBPath:       *dbPath,
		HistoryLimit: *historyLimit,
		MaxPayload:   *maxPayload,
		Key:          keyValue,
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
	printAuthInfo(s, *noAuth, *addr, keyFrom)
	if err := s.ListenAndServe(ctx); err != nil {
		logx.Fatalf("server 退出: %v", err)
	}
	logx.Printf("server 已停止")
}

// printAuthInfo 在启动日志里说明访问 key 的来源。随机生成的 key 必须打印出来
// （那是用户拿到它的唯一途径）；-k 指定的照旧打印；从 key 文件/环境变量注入
// 的不打印值——它已经持久化在可读的文件里，日志（journal）反而是更多人都
// 能看到的地方。
func printAuthInfo(s *server.Server, noAuth bool, addr, keyFrom string) {
	if noAuth {
		logx.Printf("警告: 已用 --no-auth 关闭鉴权，任何能访问该端口的人都能连接、查看历史")
		return
	}
	switch {
	case s.KeyGenerated():
		logx.Printf("访问 key（本次启动随机生成，重启后会变）: %s", s.AccessKey())
	case keyFrom == "--key":
		logx.Printf("访问 key（由 --key 指定）: %s", s.AccessKey())
	default:
		logx.Printf("访问 key（来源: %s，值不打印到日志）", keyFrom)
	}
	logx.Printf("客户端连接与管理页都需要这个 key；浏览器首次打开 http://<host>%s/ 会要求填写", addr)
}

// runInstall 处理 `shareclip-server install [选项]`。
//
// 默认逐项提问（安装位置、端口、数据目录、历史条数、鉴权与 key、运行用户、
// 单元名、启动策略），每项都带默认值，回车即采用；显式给过的命令行选项不再
// 重复提问。--yes 或非终端输入下跳过全部提问。确认后才落盘：复制可执行文件
// 到安装位置、生成/沿用 key 文件、准备数据目录、写单元并 systemctl enable。
func runInstall(args []string) {
	fs := cli.New("shareclip-server install")
	var (
		addr         = fs.String("a", "addr", server.DefaultAddr, "监听地址，如 :9090 或 0.0.0.0:9090，写进单元的 ExecStart")
		installDir   = fs.String("", "dir", "", "安装位置（可执行文件所在目录）；默认 /opt/share-clip，用户级默认 ~/.local/bin")
		dbPath       = fs.String("d", "db", "", "SQLite 数据库文件完整路径；留空按数据目录推导（<数据目录>/shareclip.db）")
		historyLimit = fs.Int("l", "history-limit", server.DefaultHistory, "历史记录保留条数")
		maxPayload   = fs.Int("m", "max-payload", protocol.DefaultMaxPayload, "单条剪贴板内容最大字节数")
		key          = fs.String("k", "key", "", "指定访问 key 并写入 key 文件（覆盖已有）；留空=自动生成/沿用")
		user         = fs.Bool("u", "user", false, "安装为用户级服务（~/.config/systemd/user，无需 root）")
		name         = fs.String("n", "name", systemd.DefaultUnitName(), "单元名（不含 .service）")
		runAs        = fs.String("", "run-as", "", "系统级服务以该用户运行；留空则 DynamicUser=yes")
		noAuth       = fs.Bool("", "no-auth", false, "写进单元的 ExecStart：关闭 server 的访问鉴权")
		extraArgs    = fs.Strings("", "exec-arg", "追加到 ExecStart 的额外启动参数，如 --exec-arg=-q")
		unitDir      = fs.String("", "unit-dir", "", "覆盖单元文件目录（默认按作用域）")
		noStart      = fs.Bool("", "no-start", false, "只 enable，不立即启动")
		force        = fs.Bool("f", "force", false, "覆盖已存在的单元文件")
		yes          = fs.Bool("y", "yes", false, "非交互安装：不提问，直接用命令行参数与各项默认值")
		dryRun       = fs.Bool("", "dry-run", false, "只打印单元文件与将执行的命令")
	)
	fs.SetIntro(installIntro)
	fs.ParseArgs(args)

	src := executablePath()
	u := prompt.New(os.Stdin, os.Stdout, !*yes && prompt.StdinInteractive())
	if !u.Active() && !*yes {
		fmt.Println("提示: 标准输入不是终端，跳过提问，按命令行参数与默认值执行。")
	}

	// 1) 服务类型。必须先定，因为它决定安装位置与数据目录的默认值。
	if u.Active() && !fs.Changed("u", "user") {
		*user = u.Confirm("安装为用户级服务（~/.config/systemd/user，无需 root）", false)
	}
	scope := scopeOf(*user)
	if scope == systemd.ScopeSystem && os.Geteuid() != 0 {
		logx.Fatalf("系统级安装需要 root 权限: 请用 sudo 重新运行，或选择用户级服务（加 --user）")
	}

	// 2) 安装位置。
	defDir, err := systemd.DefaultInstallDir(scope)
	if err != nil {
		logx.Fatalf("确定默认安装位置失败: %v", err)
	}
	dir := *installDir
	switch {
	case u.Active() && !fs.Changed("", "dir"):
		dir = u.Text("安装位置（存放可执行文件的目录）", defDir)
	case dir == "":
		dir = defDir
	}
	dir = filepath.Clean(expandTilde(dir))
	if !filepath.IsAbs(dir) {
		abs, err := filepath.Abs(dir)
		if err != nil {
			logx.Fatalf("解析安装位置失败 %s: %v", dir, err)
		}
		dir = abs
	}
	target := filepath.Join(dir, systemd.DefaultExecName())
	keyFile := keyfile.PathFor(dir) // key 文件与可执行文件同目录
	if _, err := os.Stat(target); err == nil && !sameFile(src, target) && u.Active() {
		if !u.Confirm("该位置已存在 "+systemd.DefaultExecName()+"，覆盖升级", true) {
			logx.Fatalf("已取消安装；如需保留现有文件请换一个安装位置（--dir）")
		}
	}

	// 3) 监听地址与端口。
	if u.Active() && !fs.Changed("a", "addr") {
		v, err := u.TextFunc("监听地址与端口（只填端口=监听全部网卡；仅本机可访问填 127.0.0.1:9090）", *addr, normalizeAddr)
		if err != nil {
			logx.Fatalf("监听地址无效: %v", err)
		}
		*addr = v
	} else {
		v, err := normalizeAddr(*addr)
		if err != nil {
			logx.Fatalf("--addr 无效: %v", err)
		}
		*addr = v
	}

	// 4) 数据目录（数据库文件的所在目录）。
	if *dbPath == "" && u.Active() && !fs.Changed("d", "db") {
		if defData := systemd.StateDir(systemd.Options{Scope: scope}); defData != "" {
			ans := u.Text("数据目录（数据库文件所在目录）", defData)
			*dbPath = filepath.Join(filepath.Clean(expandTilde(ans)), systemd.DefaultDBName())
		}
	}

	// 5) 服务器参数。
	if u.Active() && !fs.Changed("l", "history-limit") {
		*historyLimit = u.Int("历史记录保留条数", *historyLimit, 1, 0)
	}
	if u.Active() && !fs.Changed("m", "max-payload") {
		mb := u.Int("单条剪贴板内容大小上限（MB）", *maxPayload/(1<<20), 1, 0)
		*maxPayload = mb << 20
	}

	// 6) 鉴权与 key。key 只在安装时生成一次，保存到与可执行文件同目录的
	//    key 文件里，之后（含升级重装）一直沿用；pendingKey 非空表示本次
	//    要新建/覆盖 key 文件。
	if u.Active() && !fs.Changed("", "no-auth") {
		*noAuth = u.Confirm("关闭访问鉴权（仅限完全可信的内网，不建议）", false)
	}
	if *noAuth && *key != "" {
		logx.Printf("提示: 已关闭鉴权，-k 指定的 key 被忽略")
	}
	pendingKey := ""
	if !*noAuth {
		switch existing, err := keyfile.Read(keyFile); {
		case err == nil && existing != "":
			if fs.Changed("k", "key") && *key != "" {
				pendingKey = *key // 显式 -k：覆盖写入文件
			} else if u.Active() {
				if u.Confirm("检测到已有访问 key（"+keyFile+"），沿用（选否则重新生成）", true) {
					logx.Printf("沿用现有 key")
				} else {
					pendingKey = mustGenerateKey()
				}
			}
			// 非交互且没有 -k：沿用现有文件，不改动
		default:
			if fs.Changed("k", "key") && *key != "" {
				pendingKey = *key
			} else if u.Active() {
				// 空答案合法（=自动生成）；非空才走格式校验。
				v, err := u.TextFunc("访问 key（回车=自动生成并保存）", "", func(s string) (string, error) {
					if s == "" {
						return "", nil
					}
					return s, keyfile.Valid(s)
				})
				if err != nil {
					logx.Fatalf("访问 key 无效: %v", err)
				}
				pendingKey = v
				if pendingKey == "" {
					pendingKey = mustGenerateKey()
				}
			} else {
				pendingKey = mustGenerateKey() // 非交互：自动生成
			}
		}
	}

	// 7) systemd 侧参数。
	if scope == systemd.ScopeSystem && u.Active() && !fs.Changed("", "run-as") {
		*runAs = u.Text("以哪个系统用户运行服务（留空=systemd 动态用户 DynamicUser）", "")
	}
	if u.Active() && !fs.Changed("n", "name") {
		*name = u.Text("systemd 单元名（不含 .service）", *name)
	}
	if u.Active() && !fs.Changed("", "no-start") && !*noStart {
		*noStart = !u.Confirm("安装后立即启动并设置开机自启", true)
	}

	o := systemd.Options{
		Scope:        scope,
		Unit:         *name,
		ExecPath:     target,
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
	if pendingKey != "" || !*noAuth && keyFileExists(keyFile) {
		o.KeyFile = keyFile
	}
	if o.Scope == systemd.ScopeUser && o.RunAs != "" {
		logx.Printf("提示: 用户级服务不使用 --run-as，已忽略 %q", o.RunAs)
		o.RunAs = ""
	}
	if strings.Contains(src, "go-build") {
		logx.Printf("提示: 正在从 go run 的临时产物安装，服务将使用复制到 %s 的副本", o.ExecPath)
	}
	if unitPath, err := systemd.UnitPath(o); err == nil && u.Active() && !o.Force {
		if _, err := os.Stat(unitPath); err == nil && u.Confirm("单元文件已存在（"+unitPath+"），覆盖", false) {
			o.Force = true
		}
	}

	printInstallSummary(o, src)
	if u.Active() && !u.Confirm("确认执行安装", true) {
		fmt.Println("已取消安装，未改动任何文件。")
		return
	}

	if *dryRun {
		switch {
		case pendingKey != "":
			logx.Printf("dry-run: 将写入访问 key 文件 %s（权限 0600）", keyFile)
		case o.KeyFile != "":
			logx.Printf("dry-run: 沿用现有 key 文件 %s", keyFile)
		}
		logx.Printf("dry-run: 将复制可执行文件 %s → %s", src, o.ExecPath)
		printPlan(o, systemd.ActionInstall)
		return
	}
	if _, err := systemd.InstallBinary(src, dir, os.Stdout); err != nil {
		logx.Fatalf("安装可执行文件失败: %v", err)
	}
	if pendingKey != "" {
		if err := keyfile.Write(keyFile, pendingKey); err != nil {
			logx.Fatalf("写入 key 文件失败: %v", err)
		}
		logx.Printf("已写入访问 key 文件: %s（权限 0600）", keyFile)
	}
	if err := systemd.PrepareData(o, os.Stdout); err != nil {
		logx.Fatalf("准备数据目录失败: %v", err)
	}
	if err := systemd.Install(context.Background(), o, os.Stdout); err != nil {
		logx.Fatalf("安装 systemd 服务失败: %v", err)
	}
	reportInstall(o)
}

// runKey 处理 `shareclip-server key [选项]`：查看、轮换或设定固定访问 key。
func runKey(args []string) {
	fs := cli.New("shareclip-server key")
	var (
		set       = fs.String("", "set", "", "把 key 设置为指定值并写入 key 文件")
		rotate    = fs.Bool("r", "rotate", false, "生成新的随机 key 并写入 key 文件")
		pathFlag  = fs.String("K", "key-file", "", "直接操作这个 key 文件（默认定位：已安装单元 > 程序同目录）")
		user      = fs.Bool("u", "user", false, "按用户级单元定位 key 文件与服务")
		name      = fs.String("n", "name", systemd.DefaultUnitName(), "单元名（不含 .service）")
		unitDir   = fs.String("", "unit-dir", "", "覆盖单元文件目录（默认按作用域）")
		noRestart = fs.Bool("", "no-restart", false, "更新 key 后不自动重启服务")
	)
	fs.SetIntro(keyIntro)
	fs.ParseArgs(args)

	if *set != "" && *rotate {
		logx.Fatalf("--set 与 -r/--rotate 不能同时使用")
	}
	if *set != "" {
		if err := keyfile.Valid(*set); err != nil {
			logx.Fatalf("--set 的值不合格: %v", err)
		}
	}

	path, scope, located := locateKeyFile(*pathFlag, *user, *name, *unitDir)
	switch {
	case *rotate || *set != "":
		newKey := *set
		if *rotate {
			newKey = mustGenerateKey()
		}
		if err := keyfile.Write(path, newKey); err != nil {
			logx.Fatalf("写入 key 文件失败: %v", err)
		}
		logx.Printf("已更新访问 key 文件: %s（权限 0600）", path)
		logx.Printf("新访问 key: %s", newKey)
		logx.Printf("提醒: 所有客户端与管理页都要改用这个新 key 才能连上")
		restartService(path, scope, located, *noRestart, *name)
	default:
		cur, err := keyfile.Read(path)
		if err != nil {
			logx.Fatalf("读取 key 失败 %s: %v（安装时会自动生成；也可以 shareclip-server key -r 新建一个）", path, err)
		}
		logx.Printf("访问 key: %s", cur)
		logx.Printf("key 文件: %s", path)
		logx.Printf("客户端: shareclip-client -s <host>:<port> -k '%s'；管理页首次访问时填入同一 key", cur)
	}
}

// locateKeyFile 按约定顺序定位 key 文件：--key-file 显式指定 > 已安装 systemd
// 单元的 EnvironmentFile（默认先找系统级、再找用户级）> 当前程序同目录的
// shareclip.key。第三个返回值表示是否从单元里找到（决定了能否自动重启服务）。
func locateKeyFile(flagPath string, userOnly bool, name, unitDir string) (string, systemd.Scope, bool) {
	if flagPath != "" {
		return filepath.Clean(expandTilde(flagPath)), systemd.ScopeSystem, false
	}
	scopes := []systemd.Scope{systemd.ScopeSystem}
	if userOnly {
		scopes = []systemd.Scope{systemd.ScopeUser}
	} else {
		scopes = append(scopes, systemd.ScopeUser)
	}
	for _, sc := range scopes {
		if ef := systemd.EnvironmentFileOf(systemd.Options{Scope: sc, Unit: name, UnitDir: unitDir}); ef != "" {
			return ef, sc, true
		}
	}
	exe, err := os.Executable()
	if err != nil {
		logx.Fatalf("无法确定当前可执行文件路径: %v", err)
	}
	abs, err := filepath.Abs(filepath.Dir(exe))
	if err != nil {
		logx.Fatalf("无法解析程序目录: %v", err)
	}
	return keyfile.PathFor(abs), systemd.ScopeSystem, false
}

// restartService 在 key 更新后尽量让服务马上用上新值：能定位到已安装单元时
// 直接 systemctl restart；否则只打印提示。
func restartService(path string, scope systemd.Scope, located, noRestart bool, unit string) {
	if !located {
		fmt.Fprintf(os.Stderr, "未找到已安装的 systemd 单元；如 server 正在运行请手动重启，使新 key 生效。key 文件: %s\n", path)
		return
	}
	if noRestart {
		argv := systemctlArgv(systemd.Options{Scope: scope}, "restart", unit)
		logx.Printf("已跳过重启；稍后执行生效: %s", strings.Join(argv, " "))
		return
	}
	argv := systemctlArgv(systemd.Options{Scope: scope}, "restart", unit)
	logx.Printf("执行: %s", strings.Join(argv, " "))
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logx.Printf("警告: 服务重启失败（%v），key 文件已更新，请手动重试: %s", err, strings.Join(argv, " "))
		return
	}
	logx.Printf("服务已重启，新 key 即刻生效")
}

// runUninstall 处理 `shareclip-server uninstall [选项]`。
func runUninstall(args []string) {
	fs := cli.New("shareclip-server uninstall")
	var (
		user    = fs.Bool("u", "user", false, "卸载用户级服务（~/.config/systemd/user）")
		name    = fs.String("n", "name", systemd.DefaultUnitName(), "单元名（不含 .service）")
		unitDir = fs.String("", "unit-dir", "", "覆盖单元文件目录（默认按作用域）")
		purge   = fs.Bool("", "purge", false, "同时删除数据目录、可执行文件、数据库与 key 文件")
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
		if *purge {
			exe, db, key := systemd.PurgeTargets(o)
			if exe != "" {
				logx.Printf("dry-run: 将删除安装的可执行文件 %s", exe)
			}
			if db != "" {
				logx.Printf("dry-run: 将删除数据库 %s（含 -wal/-shm 伴生文件）", db)
			}
			if key != "" {
				logx.Printf("dry-run: 将删除 key 文件 %s", key)
			}
		}
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
		logx.Printf("已清除数据目录 %s 与单元引用的安装文件、key 文件（如有）", systemd.StateDir(o))
	}
}

// scopeOf 把 --user 开关翻译成作用域。
func scopeOf(user bool) systemd.Scope {
	if user {
		return systemd.ScopeUser
	}
	return systemd.ScopeSystem
}

// executablePath 返回当前可执行文件的绝对路径，供复制到安装位置时作为源。
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

// mustGenerateKey 生成随机访问 key，失败即退出（熵源异常时不该继续安装）。
func mustGenerateKey() string {
	k, err := keyfile.Generate()
	if err != nil {
		logx.Fatalf("生成访问 key 失败: %v", err)
	}
	return k
}

// keyFileExists 报告 key 文件是否已存在（用于「沿用现有文件」的判断）。
func keyFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// normalizeAddr 校验并规范化监听地址：纯端口号补成 ":端口"，host:port 原样
// 保留，端口必须在 1..65535。
func normalizeAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("监听地址不能为空")
	}
	if _, err := strconv.Atoi(s); err == nil {
		s = ":" + s // 只填了端口：视为监听全部网卡
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("格式应为 host:port 或纯端口号（如 127.0.0.1:9090），收到 %q", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("端口不合法: %s", port)
	}
	if host == "" {
		return ":" + port, nil
	}
	return net.JoinHostPort(host, port), nil
}

// expandTilde 把开头的 ~ 或 ~/ 展开为当前用户家目录。
func expandTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil && h != "" {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// sameFile 判断两个路径是否指向同一个文件（尽量跟随符号链接）。
func sameFile(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// printInstallSummary 把最终配置打在提问之后、落盘之前，给用户最后一次
// 核对机会（确认环节展示的就是这份摘要）。
func printInstallSummary(o systemd.Options, src string) {
	scopeText := "系统级（/etc/systemd/system，开机自启）"
	if o.Scope == systemd.ScopeUser {
		scopeText = "用户级（~/.config/systemd/user）"
	}
	auth := "固定 key（已保存/将保存到 key 文件，重启与升级都沿用）"
	if o.NoAuth {
		auth = "已关闭（任何能访问该端口的人都能连接、查看历史）"
	}
	runAs := "systemd 动态用户（DynamicUser）"
	if o.RunAs != "" {
		runAs = o.RunAs
	}
	start := "立即启动并开机自启"
	if o.NoStart {
		start = "只 enable，不立即启动"
	}
	db := o.DBPath
	if db == "" {
		db = systemd.DefaultDBPath(o)
	}
	fmt.Println("即将安装：")
	fmt.Printf("  服务类型:   %s，单元名 %s.service\n", scopeText, unitName(o))
	fmt.Printf("  安装位置:   %s（从 %s 复制）\n", o.ExecPath, src)
	fmt.Printf("  监听地址:   %s（Web 管理页与 WebSocket 共用）\n", o.Addr)
	fmt.Printf("  数据库:     %s\n", db)
	fmt.Printf("  历史/单条:  保留 %d 条，单条上限 %d MB\n", o.HistoryLimit, o.MaxPayload/(1<<20))
	fmt.Printf("  鉴权:       %s\n", auth)
	if o.KeyFile != "" {
		fmt.Printf("  key 文件:   %s\n", o.KeyFile)
	}
	if o.Scope == systemd.ScopeSystem {
		fmt.Printf("  运行用户:   %s\n", runAs)
	}
	fmt.Printf("  启动策略:   %s\n", start)
	if len(o.ExtraArgs) > 0 {
		fmt.Printf("  额外参数:   %s\n", strings.Join(o.ExtraArgs, " "))
	}
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
	logx.Printf("可执行文件 %s", o.ExecPath)
	logx.Printf("数据目录 %s，数据库 %s", systemd.StateDir(o), db)
	if o.KeyFile != "" {
		logx.Printf("访问 key 文件 %s（本次生成/沿用的 key 后续启动一直使用）", o.KeyFile)
		logx.Printf("查看 key: sudo %s key；更换: sudo %s key -r（都会同步更新该文件并重启服务）", o.ExecPath, o.ExecPath)
	}
	logx.Printf("客户端连接: shareclip-client -s <host>%s -k <访问key>", o.Addr)
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
