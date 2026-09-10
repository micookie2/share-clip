// Package tray 为 client 提供系统托盘图标与菜单：显示同步状态、打开设置与日志、
// 启用/暂停同步、退出。托盘本身只是一层显示，真正的功能由 main 通过 Config
// 注入的回调完成，所以本包不认识 HTTP 服务或同步逻辑，其状态模型也能脱离
// 托盘宿主单独测试。
//
// 平台实现：Windows 与 Linux 走 fyne.io/systray（见 tray_systray.go），
// 其它平台只有一个 Available() 恒为 false 的空实现（见 tray_unsupported.go）。
package tray

import (
	"strings"
	"sync"
)

// 菜单文案。复选框的文案在“启用同步”/“暂停同步”之间切换，始终描述当前状态。
const (
	labelOpen   = "打开设置与日志"
	labelEnable = "启用同步"
	labelPause  = "暂停同步"
	labelQuit   = "退出"

	// defaultStatus 是状态文本还没设置时的占位，否则菜单第一项会是空白行。
	defaultStatus = "状态未知"
)

// Config 描述托盘菜单及其回调。所有回调都是可选的；每个回调都在自己的
// goroutine 上执行（绝不在托盘事件循环上），所以回调里做阻塞操作也不会卡住菜单。
type Config struct {
	Tooltip  string // 鼠标悬停提示
	Status   string // 状态行文本（菜单第一项，禁用不可点）
	Enabled  bool   // “启用同步”的初始勾选状态
	OnOpen   func() // 点击“打开设置与日志”
	OnToggle func() // 点击“启用同步”/“暂停同步”（已切换为相反状态后调用）
	OnQuit   func() // 托盘循环结束、进程退出前调用
}

// menuModel 是菜单要显示的纯数据：不依赖 systray，可直接单测。
// 方法都按值返回新模型，因此不需要锁。
type menuModel struct {
	status  string
	tooltip string
	enabled bool
}

// newMenuModel 用 Config 里的初始值构造模型。
func newMenuModel(cfg Config) menuModel {
	return menuModel{status: cfg.Status, tooltip: cfg.Tooltip, enabled: cfg.Enabled}
}

func (m menuModel) withStatus(text string) menuModel  { m.status = text; return m }
func (m menuModel) withEnabled(on bool) menuModel     { m.enabled = on; return m }
func (m menuModel) withTooltip(text string) menuModel { m.tooltip = text; return m }

// statusTitle 是状态行的显示文本：空白时退化成占位文案，避免第一项看不见。
func (m menuModel) statusTitle() string {
	if strings.TrimSpace(m.status) == "" {
		return defaultStatus
	}
	return m.status
}

// toggleTitle 是复选框的文案：勾选（启用）时说“启用同步”，未勾选时说
// “暂停同步”，这样菜单文字永远是在描述当前状态。
func (m menuModel) toggleTitle() string { return toggleLabel(m.enabled) }

// toggleLabel 把勾选状态翻译成复选框文案。
func toggleLabel(enabled bool) string {
	if enabled {
		return labelEnable
	}
	return labelPause
}

// setFlags 记录哪些字段被 Set* 显式改过。Run 用 Config 填初始值时要跳过这些
// 字段：调用方可能在 Run 之前就先调了 SetStatus，那不应该被 Config 覆盖。
type setFlags struct{ status, enabled, tooltip bool }

// state 是本包唯一的共享可变状态（包级单例 st）。所有导出函数都可能被任意
// goroutine 调用，字段一律在 mu 下读写；onReady 也只取快照，不长时间持锁。
type state struct {
	mu    sync.Mutex
	model menuModel
	set   setFlags

	running  bool // Run 已进入过（systray 的全局状态不允许再 Run 一次）
	started  bool // onReady 已执行：systray 注册完成，此时才能安全调 systray.Quit()
	quitReq  bool // 有人请求过退出（可能发生在 Run 之前）
	quitSent bool // 已经调用过 systray.Quit()，保证最多调用一次
}

var st = &state{}

// claimRun 标记托盘循环已经开始；返回 false 表示本进程此前已经 Run 过。
// systray 的退出通道、菜单注册表都是包级全局的，重复 Run 只会得到未定义行为。
func (s *state) claimRun() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		return false
	}
	s.running = true
	return true
}

// seed 用 Config 的初始值填充状态，但不覆盖 Set* 显式设置过的字段，
// 保证“先 SetStatus 再 Run”时调用方的设置仍然生效。
func (s *state) seed(cfg Config) {
	want := newMenuModel(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.set.status {
		s.model = s.model.withStatus(want.status)
	}
	if !s.set.enabled {
		s.model = s.model.withEnabled(want.enabled)
	}
	if !s.set.tooltip {
		s.model = s.model.withTooltip(want.tooltip)
	}
}

// snapshot 返回状态的一份拷贝，供菜单构建与刷新读取。
func (s *state) snapshot() menuModel {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.model
}

func (s *state) setStatus(text string) {
	s.mu.Lock()
	s.model = s.model.withStatus(text)
	s.set.status = true
	s.mu.Unlock()
}

func (s *state) setEnabled(on bool) {
	s.mu.Lock()
	s.model = s.model.withEnabled(on)
	s.set.enabled = true
	s.mu.Unlock()
}

func (s *state) setTooltip(text string) {
	s.mu.Lock()
	s.model = s.model.withTooltip(text)
	s.set.tooltip = true
	s.mu.Unlock()
}

// toggle 翻转勾选状态并返回新值：点击处理先翻转状态、再回调 OnToggle，
// 符合“OnToggle 在已切换为相反状态后调用”的约定。
func (s *state) toggle() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model = s.model.withEnabled(!s.model.enabled)
	s.set.enabled = true
	return s.model.enabled
}

// requestQuit 记录退出请求；返回 true 表示现在可以安全调用 systray.Quit()。
func (s *state) requestQuit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quitReq = true
	return s.claimQuitLocked()
}

// markReady 由 onReady 在菜单建好后调用，标记 systray 已注册完成
// （在那之前调用 systray.Quit() 会 panic）。返回 true 表示退出请求早就到了，
// 调用方应立刻调用 systray.Quit() 结束循环。
func (s *state) markReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.started = true
	return s.claimQuitLocked()
}

// claimQuitLocked 在“systray 已就绪 + 有退出请求 + 还没发过”时认领一次
// systray.Quit()，保证它只被调用一次。
func (s *state) claimQuitLocked() bool {
	if !s.started || !s.quitReq || s.quitSent {
		return false
	}
	s.quitSent = true
	return true
}

func (s *state) quitRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quitReq
}

func (s *state) quitSentToSystray() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.quitSent
}

// exitGate 保证退出回调只被启动一次，并让 Run 能等到它真正跑完。
// 回调放在独立 goroutine 上是为了不占用托盘事件循环；Run 又要等它结束，
// 因为 main 通常在 Run 返回后立刻退出进程，不等就会截断清理逻辑。
type exitGate struct {
	once    sync.Once
	cb      func()
	started chan struct{} // fire 的同步部分关闭，表示回调已开始
	done    chan struct{} // 回调返回后关闭
}

func newExitGate(cb func()) *exitGate {
	return &exitGate{cb: cb, started: make(chan struct{}), done: make(chan struct{})}
}

// fire 启动退出回调（在独立 goroutine 上）；可重复调用，回调只跑一次。
func (g *exitGate) fire() {
	g.once.Do(func() {
		close(g.started)
		go func() {
			defer close(g.done)
			if g.cb != nil {
				g.cb()
			}
		}()
	})
}

// waitFired 阻塞到退出回调执行完毕；只在确定 fire 已被调用时使用。
func (g *exitGate) waitFired() {
	<-g.started
	<-g.done
}

// wait 若 fire 从未发生则立即返回，否则等回调跑完。用于“事件循环自己结束、
// 退出回调是否被调用过不确定”的场景，避免死等。
func (g *exitGate) wait() {
	select {
	case <-g.started:
		<-g.done
	default:
	}
}

// fired 报告退出回调是否已经开始（started 在 fire 的同步部分就被关闭）。
// Run 用它区分“systray 收尾阶段的库内 panic”和与托盘无关的 panic。
func (g *exitGate) fired() bool {
	select {
	case <-g.started:
		return true
	default:
		return false
	}
}

// SetStatus 更新状态行文本（可从任意 goroutine 调用，Run 之前调用也要生效）。
func SetStatus(text string) {
	st.setStatus(text)
	applyStatus()
}

// SetEnabled 更新“启用同步”的勾选状态（同上，任意时机安全）。
func SetEnabled(on bool) {
	st.setEnabled(on)
	applyEnabled()
}

// SetTooltip 更新悬停提示（同上，任意时机安全）。
func SetTooltip(text string) {
	st.setTooltip(text)
	applyTooltip()
}
