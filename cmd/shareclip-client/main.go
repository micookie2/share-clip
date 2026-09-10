// share-clip client: watches the local clipboard and shares text/image
// content through the share-clip server with every other running client.
//
// 两种运行方式：
//   - 桌面模式（不带参数双击运行）：常驻系统托盘 + 本地设置/日志界面，
//     服务器地址等设置保存在用户配置目录里；
//   - 控制台模式（显式给了 -s，或加了 --console）：日志打在终端，适合脚本。
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/micookie2/share-clip/internal/agent"
	"github.com/micookie2/share-clip/internal/appconfig"
	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/cli"
	"github.com/micookie2/share-clip/internal/clientsvc"
	"github.com/micookie2/share-clip/internal/clientui"
	"github.com/micookie2/share-clip/internal/logx"
	"github.com/micookie2/share-clip/internal/protocol"
	"github.com/micookie2/share-clip/internal/tray"
)

// defaultUIAddr 是桌面模式本地界面的首选地址。固定端口的好处是能被记住、
// 能加书签；被别的程序占用时 clientui 会自动退到随机端口。
const defaultUIAddr = "127.0.0.1:9210"

// logFileMaxBytes 是桌面模式日志文件的轮转阈值：客户端常在托盘里跑几周，
// 日志不能无限增长。
const logFileMaxBytes = 2 << 20 // 2 MiB

func main() {
	build := buildinfo.Get()
	fs := cli.New("shareclip-client")
	var (
		serverAddr = fs.String("s", "server", "", "share-clip server 地址，如 192.168.1.10:9000；不带参数双击运行时在界面里设置")
		name       = fs.String("n", "name", "", "本机显示名（默认使用主机名）")
		pollMs     = fs.Int("p", "poll", appconfig.DefaultPollMs, "剪贴板监听兜底轮询间隔（毫秒）：X11 默认由 XFixes 复制事件驱动，仅事件不可用时按此轮询；Wayland/GNOME 等无事件通道的平台按此轮询")
		maxPayload = fs.Int("m", "max-payload", protocol.DefaultMaxPayload, "单条剪贴板内容最大字节数")
		quiet      = fs.Bool("q", "quiet", false, "减少日志输出")
		gui        = fs.Bool("g", "gui", false, "强制以桌面模式运行：系统托盘 + 本地设置/日志界面")
		console    = fs.Bool("c", "console", false, "强制以控制台模式运行：不开托盘、不开界面，日志打到终端")
		uiAddr     = fs.String("", "ui-addr", defaultUIAddr, "桌面模式本地界面的监听地址（只监听本机）")
		noOpen     = fs.Bool("", "no-open", false, "桌面模式启动后不自动打开浏览器")
		showVer    = fs.Bool("v", "version", false, "显示版本号与构建信息后退出")
	)
	fs.SetIntro(fmt.Sprintf(`share-clip client %s
用法: shareclip-client                      # 双击运行：托盘常驻 + 本地设置/日志界面
     shareclip-client -s 192.168.1.10:9000  # 控制台模式（不托盘），适合脚本/开机自启
     shareclip-client --console             # 控制台模式，服务器地址取自保存的配置

没给 -s 时进入桌面模式：图标常驻系统托盘，同时用浏览器打开本地界面
（默认 %s），在那里配置服务器地址、查看实时日志。关掉浏览器页面
不影响运行；退出请用托盘菜单或界面上的「退出客户端」。

启动后本机每次复制文本或图片都会广播到 server 上的其它客户端；
收到的广播会自动写入本机剪贴板。文件复制暂不支持，会被忽略。
每个选项都有等价的长写法（-s 即 --server），见下面的列表。`, build.Version, defaultUIAddr))
	fs.Parse()
	if *showVer {
		fmt.Println(build.Summary())
		return
	}

	// 没有 -s，又没被显式要求进控制台，就是「双击运行」：进桌面模式。
	desktop := !*console && (*gui || !fs.Changed("s", "server"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if desktop {
		err := runDesktop(ctx, build, desktopFlags{
			Server:     *serverAddr,
			Name:       *name,
			PollMs:     *pollMs,
			MaxPayload: *maxPayload,
			Quiet:      *quiet,
			UIAddr:     *uiAddr,
			NoOpen:     *noOpen,
			ServerSet:  fs.Changed("s", "server"),
			PollSet:    fs.Changed("p", "poll"),
			MaxSet:     fs.Changed("m", "max-payload"),
		})
		if err != nil {
			logx.Fatalf("client 退出: %v", err)
		}
		return
	}
	if err := runConsole(ctx, build, *serverAddr, *name, *pollMs, *maxPayload, *quiet); err != nil {
		logx.Fatalf("client 退出: %v", err)
	}
}

type desktopFlags struct {
	Server     string
	Name       string
	PollMs     int
	MaxPayload int
	Quiet      bool
	UIAddr     string
	NoOpen     bool
	ServerSet  bool // 命令行是否显式给了 -s，是则覆盖已保存的配置
	PollSet    bool
	MaxSet     bool
}

// runDesktop 是托盘 + 本地界面的运行方式，也是双击启动时的默认行为：
// 先读保存的配置（缺省值兜底），再拉起 agent、本地界面与托盘。
func runDesktop(parent context.Context, build buildinfo.Info, f desktopFlags) error {
	// 桌面模式没有可见的控制台：藏掉双击时系统为控制台程序分配的黑窗口。
	// 从已有终端里启动时会检测到共享控制台，不会把用户的终端一起藏了。
	hideConsoleWindow()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	cfg, loadErr := appconfig.Load()
	if loadErr != nil {
		// 配置坏了也要能进界面：界面里能重新填地址，比直接退出有用得多。
		logx.Printf("[client] 读取配置失败，将使用默认配置: %v", loadErr)
	}
	cfg = applyFlagOverrides(cfg, f)
	if cfg.Server != "" {
		// 配置文件可能是手改过的：地址能修就修（补端口、去掉前缀），修不了就
		// 明确提示，别让 agent 拿着一个明显不成立的地址反复重试。
		if normalized, err := cfg.Normalized(); err == nil {
			cfg = normalized
		} else {
			logx.Printf("[client] 配置里的 server 地址无法识别（%v），请在界面里重新填写", err)
		}
	}
	if f.Quiet {
		// 桌面模式的日志是界面的一部分，界面要有完整记录才有用。
		logx.Printf("[client] 桌面模式的界面日志保持完整，已忽略 -q")
	}

	logPath, err := appconfig.LogPath()
	if err != nil {
		logx.Printf("[client] 无法确定日志文件路径: %v", err)
	}
	if logPath != "" {
		closeLog, err := logx.AttachFile(logPath, logFileMaxBytes)
		if err != nil {
			logx.Printf("[client] 日志文件不可用，日志只显示在界面里: %v", err)
			logPath = ""
		} else {
			defer closeLog()
		}
	}

	svc := clientsvc.New(cfg, build)
	svc.Start()
	defer svc.Stop()

	var quitOnce sync.Once
	quit := func() { quitOnce.Do(cancel) }

	trayOK := tray.Available()
	ui := clientui.New(svc, clientui.Options{
		LogFile:     logPath,
		TrayEnabled: trayOK,
		Quit:        quit,
	})

	// 已经有一个在跑的客户端（双击了第二次）：不再起一个进程，直接把它的
	// 界面打开——两个托盘图标、两份日志只会让人困惑。
	if clientui.Probe(f.UIAddr) {
		url := "http://" + f.UIAddr
		logx.Printf("[client] 已有一个客户端在运行，打开它的界面: %s", url)
		if !f.NoOpen {
			if err := clientui.OpenBrowser(url); err != nil {
				logx.Printf("[client] 打开浏览器失败: %v（请手动访问 %s）", err, url)
			}
		}
		return nil
	}

	url, err := ui.Listen(f.UIAddr)
	if err != nil {
		return err
	}
	defer ui.Close()

	logx.Printf("share-clip client %s（%s）已启动，server: %s（commit %s，构建于 %s）",
		build.Version, svc.DisplayName(), displayServer(cfg.Server), build.Commit, build.BuiltLocal())
	logx.Printf("[client] 本地界面: %s（日志文件: %s）", url, displayServer(logPath))
	if !f.NoOpen {
		if err := clientui.OpenBrowser(url); err != nil {
			logx.Printf("[client] 自动打开浏览器失败: %v（请手动访问 %s）", err, url)
		}
	}

	if !trayOK {
		logx.Printf("[client] 未检测到可用的系统托盘，客户端将在后台运行；退出请用界面上的「退出客户端」按钮")
		<-ctx.Done()
		return nil
	}

	// 托盘菜单里的操作都直接落到服务上，托盘不保存第二份状态。
	go syncTray(ctx, svc)
	go func() {
		<-ctx.Done()
		tray.Quit()
	}()
	if err := tray.Run(tray.Config{
		Tooltip: fmt.Sprintf("share-clip 客户端 %s", build.Version),
		Status:  trayStatusText(svc.Status()),
		Enabled: !svc.Paused(),
		OnOpen: func() {
			if err := clientui.OpenBrowser(ui.URL()); err != nil {
				logx.Printf("[client] 打开界面失败: %v", err)
			}
		},
		OnToggle: func() {
			// 以服务里的暂停状态为准：托盘自己的勾选状态只是展示。
			svc.SetPaused(!svc.Paused())
			tray.SetEnabled(!svc.Paused())
		},
		OnQuit: func() { quit() },
	}); err != nil {
		// 托盘已结束，这里只是收尾诊断（例如 Linux 无会话总线时库内的退出异常）。
		logx.Printf("[client] 托盘退出时报错: %v", err)
	}
	logx.Printf("[client] share-clip client 已退出")
	return nil
}

// syncTray 把服务状态变化同步到托盘菜单，让托盘上的状态行始终保持最新。
func syncTray(ctx context.Context, svc *clientsvc.Service) {
	events, unsubscribe := svc.Subscribe()
	defer unsubscribe()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if ev.Kind != "status" || ev.Status == nil {
				continue
			}
			st := *ev.Status
			tray.SetStatus(trayStatusText(st))
			tray.SetEnabled(!st.Paused)
			tray.SetTooltip(trayTooltip(st))
		}
	}
}

// trayStatusText 把状态压成托盘菜单里那一行文字（菜单项很窄，只放最关键的）。
func trayStatusText(st clientsvc.Status) string {
	phase := map[clientsvc.Phase]string{
		clientsvc.PhaseIdle:       "未运行",
		clientsvc.PhaseConnecting: "连接中",
		clientsvc.PhaseConnected:  "已连接",
		clientsvc.PhaseRetrying:   "重连中",
		clientsvc.PhaseError:      "本地环境问题",
	}[st.Phase]
	if st.Paused {
		phase = "已暂停"
	}
	if st.Server == "" {
		return phase + "：未配置服务器"
	}
	return phase + "：" + st.Server
}

func trayTooltip(st clientsvc.Status) string {
	text := "share-clip 客户端 · " + trayStatusText(st)
	if st.Detail != "" {
		text += "\n" + st.Detail
	}
	return text
}

// applyFlagOverrides 让命令行显式给出的选项覆盖已保存的配置；没给的保持原样。
func applyFlagOverrides(cfg appconfig.Config, f desktopFlags) appconfig.Config {
	if f.ServerSet && strings.TrimSpace(f.Server) != "" {
		addr, err := appconfig.NormalizeServer(f.Server)
		if err != nil {
			logx.Printf("[client] 命令行里的 -s 无法识别（%v），改用已保存的地址", err)
		} else {
			cfg.Server = addr
		}
	}
	if strings.TrimSpace(f.Name) != "" {
		cfg.Name = strings.TrimSpace(f.Name)
	}
	if f.PollSet {
		cfg.PollMs = f.PollMs
	}
	if f.MaxSet {
		cfg.MaxPayload = f.MaxPayload
	}
	return cfg.WithDefaults()
}

// runConsole 是原来的命令行运行方式：不托盘、不开界面，日志直接打到终端。
// 没给 -s 时回退到桌面模式保存下来的配置，这样开机自启脚本可以只写
// `shareclip-client --console`。
func runConsole(ctx context.Context, build buildinfo.Info, serverAddr, name string, pollMs, maxPayload int, quiet bool) error {
	if serverAddr == "" {
		cfg, err := appconfig.Load()
		if err == nil && cfg.Server != "" {
			serverAddr = cfg.Server
			logx.Printf("[client] 使用配置文件中保存的 server 地址: %s", serverAddr)
		}
	}
	if serverAddr == "" {
		return errors.New("请用 -s 指定 server 地址，例如 shareclip-client -s 192.168.1.10:9000；或双击运行后在界面里设置")
	}
	addr, err := appconfig.NormalizeServer(serverAddr)
	if err != nil {
		return err
	}

	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	displayName := strings.TrimSpace(name)
	if displayName == "" {
		displayName = host
	}

	cfg := agent.Config{
		ServerAddr:     addr,
		ClientID:       fmt.Sprintf("%s-%d-%s", host, os.Getpid(), randHex(4)),
		Name:           displayName,
		PollIntervalMs: pollMs,
		MaxPayload:     maxPayload,
		Quiet:          quiet,
	}

	logx.Printf("share-clip client %s（%s）启动，server: %s（commit %s，构建于 %s）",
		build.Version, displayName, addr, build.Commit, build.BuiltLocal())
	return agent.Run(ctx, cfg)
}

// displayServer 在日志里给空值一个明确的说法：桌面模式首次启动时确实还没有
// 服务器地址，写「未配置」比写空更能说明问题。
func displayServer(s string) string {
	if strings.TrimSpace(s) == "" {
		return "（未配置）"
	}
	return s
}

// randHex 生成 n 字节的随机十六进制串，用于区分同一台机器上的多个客户端实例。
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
