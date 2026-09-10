package tray

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 测试只用与平台无关的状态模型、exitGate 和 Available/Quit：CI 既没有
// 会话总线也没有显示器，绝不能真的启动托盘循环。

func TestNewMenuModelFromConfig(t *testing.T) {
	m := newMenuModel(Config{Status: "同步中", Enabled: true, Tooltip: "share-clip"})
	if got := m.statusTitle(); got != "同步中" {
		t.Errorf("statusTitle() = %q, want %q", got, "同步中")
	}
	if got := m.toggleTitle(); got != labelEnable {
		t.Errorf("toggleTitle() = %q, want %q", got, labelEnable)
	}
	if !m.enabled {
		t.Error("enabled 应为 true")
	}
	if m.tooltip != "share-clip" {
		t.Errorf("tooltip = %q, want %q", m.tooltip, "share-clip")
	}
}

// with* 返回新模型而不是就地修改，状态快照之间才不会互相污染。
func TestMenuModelWithHelpersAreValueCopies(t *testing.T) {
	base := newMenuModel(Config{Status: "a", Enabled: true, Tooltip: "t"})
	derived := base.withStatus("b").withEnabled(false).withTooltip("t2")

	if base.status != "a" || !base.enabled || base.tooltip != "t" {
		t.Fatalf("原模型被改动: %+v", base)
	}
	if derived.status != "b" || derived.enabled || derived.tooltip != "t2" {
		t.Fatalf("派生模型 = %+v, want {b false t2}", derived)
	}
}

func TestStatusTitleFallsBackWhenBlank(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := newMenuModel(Config{Status: in}).statusTitle(); got != defaultStatus {
			t.Errorf("statusTitle(%q) = %q, want %q", in, got, defaultStatus)
		}
	}
}

func TestToggleLabelSwitching(t *testing.T) {
	cases := []struct {
		enabled bool
		want    string
	}{
		{true, labelEnable},
		{false, labelPause},
	}
	for _, c := range cases {
		if got := toggleLabel(c.enabled); got != c.want {
			t.Errorf("toggleLabel(%v) = %q, want %q", c.enabled, got, c.want)
		}
		if got := newMenuModel(Config{Enabled: c.enabled}).toggleTitle(); got != c.want {
			t.Errorf("toggleTitle(enabled=%v) = %q, want %q", c.enabled, got, c.want)
		}
	}
}

// Set* 在 Run 之前只更新状态，onReady 之后才可能读到；这里验证状态本身。
func TestStateSettersBeforeRun(t *testing.T) {
	s := &state{}
	s.setStatus("同步中")
	s.setEnabled(true)
	s.setTooltip("share-clip")

	got := s.snapshot()
	if got.statusTitle() != "同步中" {
		t.Errorf("statusTitle() = %q, want %q", got.statusTitle(), "同步中")
	}
	if got.toggleTitle() != labelEnable {
		t.Errorf("toggleTitle() = %q, want %q", got.toggleTitle(), labelEnable)
	}
	if got.tooltip != "share-clip" {
		t.Errorf("tooltip = %q, want %q", got.tooltip, "share-clip")
	}
}

func TestStateSeedUsesConfigForUntouchedFields(t *testing.T) {
	s := &state{}
	s.seed(Config{Status: "启动中", Enabled: true, Tooltip: "tip"})

	got := s.snapshot()
	if got.status != "启动中" || !got.enabled || got.tooltip != "tip" {
		t.Fatalf("seed 后 snapshot = %+v, want Config 的初始值", got)
	}
}

// 先 Set* 再 Run 时，Config 不能覆盖调用方已经设置的值。
func TestStateSeedKeepsExplicitSetterValues(t *testing.T) {
	s := &state{}
	s.setStatus("下载中")
	s.setEnabled(false)
	s.setTooltip("自定义提示")

	s.seed(Config{Status: "启动中", Enabled: true, Tooltip: "cfg-tip"})

	got := s.snapshot()
	if got.status != "下载中" {
		t.Errorf("status = %q, want %q（显式 SetStatus 应优先于 Config）", got.status, "下载中")
	}
	if got.enabled {
		t.Error("enabled 应保持 SetEnabled(false)")
	}
	if got.tooltip != "自定义提示" {
		t.Errorf("tooltip = %q, want %q", got.tooltip, "自定义提示")
	}
}

func TestStateToggleFlipsAndReports(t *testing.T) {
	s := &state{}
	if on := s.toggle(); !on {
		t.Error("第一次 toggle 应为 true")
	}
	if on := s.toggle(); on {
		t.Error("第二次 toggle 应为 false")
	}
	if got := s.snapshot().toggleTitle(); got != labelPause {
		t.Errorf("toggle 两次后 toggleTitle() = %q, want %q", got, labelPause)
	}
}

// Available 只做本机探测，任何平台上都不应 panic。
func TestAvailableDoesNotPanic(t *testing.T) {
	_ = Available()
	_ = Available()
}

// Run 之前请求退出只应被记录下来，绝不能认领 systray.Quit()——
// systray 在 Register 之前执行退出回调会 panic。
func TestQuitBeforeReadyIsOnlyRecorded(t *testing.T) {
	s := &state{}
	if s.requestQuit() {
		t.Fatal("onReady 之前 requestQuit 不应返回 true")
	}
	if !s.quitRequested() {
		t.Error("退出请求应被记录")
	}
	if s.quitSentToSystray() {
		t.Error("不应标记为已调用 systray.Quit()")
	}
}

// onReady 标记就绪时，若退出请求早就到了，必须认领一次 systray.Quit()；
// 之后再调用 Quit 都不能重复认领。
func TestReadyClaimsPendingQuitOnce(t *testing.T) {
	s := &state{}
	s.requestQuit()

	if !s.markReady() {
		t.Fatal("就绪后应认领挂起的退出请求")
	}
	if !s.quitSentToSystray() {
		t.Error("应标记为已调用 systray.Quit()")
	}
	if s.requestQuit() {
		t.Error("systray.Quit() 只能调用一次")
	}
	if s.markReady() {
		t.Error("markReady 也不应重复认领")
	}
}

// 就绪之后的退出请求应立即认领。
func TestQuitAfterReadyClaimsImmediately(t *testing.T) {
	s := &state{}
	if s.markReady() {
		t.Fatal("没有退出请求时 markReady 不应认领")
	}
	if !s.requestQuit() {
		t.Fatal("就绪后 requestQuit 应返回 true")
	}
	if s.requestQuit() {
		t.Error("第二次 requestQuit 不应重复认领")
	}
}

func TestClaimRunOnlyOnce(t *testing.T) {
	s := &state{}
	if !s.claimRun() {
		t.Fatal("第一次 claimRun 应为 true")
	}
	if s.claimRun() {
		t.Error("第二次 claimRun 应为 false")
	}
}

// Set* 允许从任意 goroutine 并发调用，状态读写不能有数据竞争（配合 -race）。
func TestStateSettersAreConcurrencySafe(t *testing.T) {
	s := &state{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				s.setStatus("同步中")
				s.setEnabled(j%2 == 0)
				s.setTooltip("share-clip")
				s.toggle()
				_ = s.snapshot()
			}
		}(i)
	}
	wg.Wait()
}

// Quit 在 Run 之前调用必须是安全空操作：既不能 panic，也不能真的去调
// systray.Quit()（那在 Register 之前会执行 nil 的退出回调而 panic）。
func TestQuitBeforeRunIsSafeNoop(t *testing.T) {
	Quit()
	Quit()

	if !st.quitRequested() {
		t.Error("Quit 之后应记录退出请求，Run 才能尽快返回")
	}
	if st.quitSentToSystray() {
		t.Error("Run 之前不应调用 systray.Quit()")
	}
}

func TestExitGateRunsCallbackOnce(t *testing.T) {
	var calls atomic.Int32
	gate := newExitGate(func() { calls.Add(1) })

	gate.fire()
	gate.fire()
	gate.waitFired()

	if got := calls.Load(); got != 1 {
		t.Errorf("回调执行了 %d 次, want 1", got)
	}
}

// 没有 fire 过时 wait 必须立即返回，否则事件循环异常结束会把进程挂死。
func TestExitGateWaitWithoutFireReturns(t *testing.T) {
	gate := newExitGate(func() { t.Error("回调不应被调用") })

	done := make(chan struct{})
	go func() {
		gate.wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("wait 在没有 fire 时阻塞了")
	}
}

// waitFired 必须等到回调真正结束，保证 Run 返回前清理已完成。
func TestExitGateWaitFiredWaitsForCallback(t *testing.T) {
	release := make(chan struct{})
	gate := newExitGate(func() { <-release })

	gate.fire()

	returned := make(chan struct{})
	go func() {
		gate.waitFired()
		close(returned)
	}()

	select {
	case <-returned:
		t.Fatal("回调尚未结束时 waitFired 就返回了")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("回调结束后 waitFired 未返回")
	}
}

// 回调为 nil 时也要能正常走完 fire/waitFired。
func TestExitGateNilCallback(t *testing.T) {
	gate := newExitGate(nil)
	gate.fire()
	gate.waitFired()
}
