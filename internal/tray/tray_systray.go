//go:build windows || linux

package tray

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"

	"fyne.io/systray"

	"github.com/micookie2/share-clip/assets"
	"github.com/micookie2/share-clip/internal/logx"
)

// errRunTwice 见 Run 的文档：systray 的状态是进程级全局的，只能 Run 一次。
var errRunTwice = errors.New("托盘已启动过：tray.Run 在一个进程中只能调用一次")

// icon 按平台选择图标：Windows 的 Shell_NotifyIcon 只接受 .ico，
// Linux 的 StatusNotifierItem 用 image.Decode 解码 PNG。
var icon = func() []byte {
	if runtime.GOOS == "windows" {
		return assets.IconICO
	}
	return assets.Icon192
}()

// Available 报告本机是否有可用的托盘宿主。
//
//   - Windows：恒为 true（Shell_NotifyIcon 是系统组件）。
//   - Linux：仅当 D-Bus 会话总线看起来可达时为 true。StatusNotifierItem 必须
//     经由会话总线才能被桌面面板接管，没有总线的机器上图标不会出现在任何地方。
//   - 其它平台：false（见 tray_unsupported.go）。
func Available() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	return sessionBusReachable()
}

// sessionBusReachable 按 freedesktop 的约定做无副作用探测：先看
// DBUS_SESSION_BUS_ADDRESS，再退回 systemd 的 /run/user/<uid>/bus。
// 只 stat 不真的连接：探测阶段不应有副作用（也不该顺手启动 dbus-daemon），
// 真正的连接与失败日志交给 systray 自己处理。
func sessionBusReachable() bool {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return true
	}
	uid := os.Getuid() // Windows 上恒为 -1，不过上面已经返回了
	if uid < 0 {
		return false
	}
	_, err := os.Stat(filepath.Join("/run/user", strconv.Itoa(uid), "bus"))
	return err == nil
}

// Run 显示托盘图标并阻塞，直到 Quit 被调用。必须由 main goroutine 调用。
//
// 调用方应先检查 Available()，但即使托盘宿主缺失，Run 也不会 panic：
//   - Linux 上连不上 D-Bus 会话总线时，systray 的 nativeStart 只打一行日志，
//     nativeLoop 会一直阻塞到 Quit()（见 fyne.io/systray 的 systray_unix.go）；
//     此时图标不会出现在面板上，调用方必须另留退出手段（信号等）。
//   - 同一场景下退出时，systray 的 nativeEnd 会对保持 nil 的 *dbus.Conn 调用
//     Close 而 panic；本函数能识别出“退出流程已开始”的库内 panic 并转成返回的
//     错误，退出前的 panic 照常上抛。
//
// 一个进程只应调用一次 Run（重复调用返回 errRunTwice）：systray 的退出通道
// 与菜单注册表都是包级全局的。
func Run(cfg Config) (err error) {
	if !st.claimRun() {
		return errRunTwice
	}
	st.seed(cfg)

	gate := newExitGate(cfg.OnQuit)

	// Run 之前就有人调用过 Quit()：根本不启动 systray（也避免 systray.Quit()
	// 在 Register 之前执行 nil 退出回调而 panic），直接走退出路径尽快返回。
	// 若退出请求来得更晚，由 onReady 在菜单建好后调用 systray.Quit()。
	if st.quitRequested() {
		gate.fire()
		gate.waitFired()
		return nil
	}

	if !Available() {
		logx.Printf("托盘：未检测到 D-Bus 会话总线，图标可能不会显示；Run 会一直等到 Quit")
	}

	defer func() {
		if r := recover(); r != nil {
			// 退出回调已经开始 ⇒ panic 出在 systray 的收尾代码里（退出路径上的
			// nil conn 解引用就是这一类），吞掉并转成错误；否则是与托盘无关的
			// panic，原样上抛。
			if !gate.fired() {
				panic(r)
			}
			logx.Printf("托盘：systray 退出时报错，已忽略: %v", r)
			err = fmt.Errorf("托盘退出异常: %v", r)
		}
		// 等 cfg.OnQuit 跑完再返回：main 通常在 Run 返回后立刻退出进程。
		// quitSent 为真说明 systray.Quit() 已被调用过，库保证退出回调一定
		// 会执行，所以这里的等待不会卡死；否则只在回调已经开始时才等。
		if st.quitSentToSystray() {
			gate.waitFired()
			return
		}
		gate.wait()
	}()

	systray.Run(func() { setupMenu(cfg) }, gate.fire)
	return nil
}

// live 保存 onReady 建好的菜单项。对菜单项的写入（SetTitle/Check/Uncheck）都在
// 它的锁下串行化：systray 的 MenuItem 字段本身不带锁，而 Set* 允许从任意
// goroutine 调用，必须自己保证不会并发修改同一个 item。
var live struct {
	mu     sync.Mutex
	ready  bool // onReady 已建好菜单，可以安全更新界面
	status *systray.MenuItem
	toggle *systray.MenuItem
}

// setupMenu 在 systray 的 onReady 中构建菜单。onReady 由 systray 在 Register
// 完成后调用（Linux 在 nativeStart 开头，Windows 在 registerSystray 末尾），
// 所以只有在这里调用 SetIcon/SetTooltip/AddMenuItem 才是安全的。
func setupMenu(cfg Config) {
	model := st.snapshot()

	systray.SetIcon(icon)
	systray.SetTooltip(model.tooltip)
	// Linux 的 StatusNotifierItem 把左键单击作为 Activate 发过来（菜单在右键），
	// 顺手让它等于「打开设置与日志」；Windows 上左键本来就是弹菜单，这里无影响。
	if cfg.OnOpen != nil {
		systray.SetOnTapped(cfg.OnOpen)
	}

	statusItem := systray.AddMenuItem(model.statusTitle(), "")
	statusItem.Disable() // 状态行只用于显示
	openItem := systray.AddMenuItem(labelOpen, "")
	toggleItem := systray.AddMenuItemCheckbox(model.toggleTitle(), "", model.enabled)
	systray.AddSeparator()
	quitItem := systray.AddMenuItem(labelQuit, "")

	live.mu.Lock()
	live.ready, live.status, live.toggle = true, statusItem, toggleItem
	live.mu.Unlock()

	// 发布后再按最新状态回写一次：取快照与发布之间可能有并发的 Set*，
	// 回写能保证那次更新不丢（重复设置是幂等的）。
	applyStatus()
	applyEnabled()
	applyTooltip()

	// ClickedCh 是非阻塞发送（见 systray.go 的 systrayMenuItemSelected），
	// 每个菜单项都要有常驻 goroutine 及时取走点击，否则点击会被静默丢弃；
	// 用户回调因此在独立 goroutine 上执行，不会占用托盘事件循环。
	go watchClicks(openItem, cfg.OnOpen)
	go watchClicks(quitItem, Quit)
	go watchClicks(toggleItem, func() {
		// 先切到相反状态并刷新菜单，再通知调用方（OnToggle 的语义是“已切换后调用”）。
		st.toggle()
		applyEnabled()
		if cfg.OnToggle != nil {
			cfg.OnToggle()
		}
	})

	// 菜单建好后才允许调用 systray.Quit()。若退出请求早就到了，这里立刻结束循环。
	if st.markReady() {
		systray.Quit()
	}
}

// watchClicks 在独立 goroutine 中消费 item 的点击并调用 fn；fn 为 nil 时仍然
// 消费，保持行为一致。item 被 Remove 关闭通道后 goroutine 自行退出。
func watchClicks(item *systray.MenuItem, fn func()) {
	go func() {
		for range item.ClickedCh {
			if fn != nil {
				fn()
			}
		}
	}()
}

// applyStatus 把最新状态文本写到菜单第一项；菜单还没建好时只需更新状态，
// onReady 会读到最新快照，所以直接返回。
func applyStatus() {
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.status == nil {
		return
	}
	live.status.SetTitle(st.snapshot().statusTitle())
}

// applyEnabled 同步复选框的文案与勾选状态。
func applyEnabled() {
	live.mu.Lock()
	defer live.mu.Unlock()
	if live.toggle == nil {
		return
	}
	model := st.snapshot()
	live.toggle.SetTitle(model.toggleTitle())
	if model.enabled {
		live.toggle.Check()
	} else {
		live.toggle.Uncheck()
	}
}

// applyTooltip 更新图标悬停提示；必须等 onReady 之后（Windows 上托盘未就绪时
// 设置只会让 systray 打一行错误日志）。
func applyTooltip() {
	live.mu.Lock()
	defer live.mu.Unlock()
	if !live.ready {
		return
	}
	systray.SetTooltip(st.snapshot().tooltip)
}

// Quit 请求退出托盘循环；可重复调用，也可在 Run 之前调用。
func Quit() {
	// systray.Quit() 在 systray.Register/Run 之前调用会执行 nil 的退出回调
	// （Windows 的 quit() 直接调 runSystrayExit）而 panic，所以这里只记录请求，
	// 等 onReady 标记就绪后才真正调用，并且只调用一次。
	if st.requestQuit() {
		systray.Quit()
	}
}
