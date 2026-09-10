package clientsvc

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micookie2/share-clip/internal/agent"
	"github.com/micookie2/share-clip/internal/appconfig"
	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/logx"
)

// fakeRun 顶替 agent.Run：记录每次被拉起的配置，按需上报「已连接」，然后一直
// 阻塞到上下文被取消——与真实 agent 在连接期间的阻塞行为一致。
type fakeRun struct {
	mu      sync.Mutex
	started []agent.Config
	err     error // 非 nil：模拟剪贴板后端不可用这类本地错误
}

func (f *fakeRun) run(ctx context.Context, cfg agent.Config) error {
	f.mu.Lock()
	f.started = append(f.started, cfg)
	err := f.err
	f.mu.Unlock()

	if err != nil {
		return err
	}
	if cfg.OnConnected != nil {
		cfg.OnConnected(cfg.ServerAddr)
	}
	<-ctx.Done()
	return nil
}

func (f *fakeRun) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.started)
}

func (f *fakeRun) last() agent.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.started) == 0 {
		return agent.Config{}
	}
	return f.started[len(f.started)-1]
}

func useFakeRun(t *testing.T, f *fakeRun) {
	t.Helper()
	old := runAgent
	runAgent = f.run
	t.Cleanup(func() { runAgent = old })
}

// isolateConfigHome 让 SaveConfig 写到临时目录，避免污染真实用户配置。
func isolateConfigHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("AppData", dir)
	t.Setenv("HOME", dir)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func TestStartConnectsAndReportsStatus(t *testing.T) {
	f := &fakeRun{}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{Version: "test"})
	s.Start()
	defer s.Stop()

	waitFor(t, "agent 被拉起", func() bool { return f.count() >= 1 })
	if got := f.last().ServerAddr; got != "10.0.0.1:9000" {
		t.Errorf("agent 拿到 server = %q", got)
	}
	waitFor(t, "状态变为已连接", func() bool { return s.Status().Phase == PhaseConnected })

	st := s.Status()
	if st.Server != "10.0.0.1:9000" || !st.Running || st.Paused {
		t.Errorf("状态快照不符合预期: %+v", st)
	}
	if st.Since.IsZero() {
		t.Error("已连接状态应记录起始时刻")
	}
}

func TestSaveConfigRestartsAgentWithNewServer(t *testing.T) {
	f := &fakeRun{}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{})
	s.Start()
	defer s.Stop()

	waitFor(t, "首个连接建立", func() bool { return f.count() >= 1 })

	if err := s.SaveConfig(appconfig.Config{Server: "ws://10.0.0.2:9100/ws", Name: "desk-2"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	waitFor(t, "用新地址重连", func() bool {
		return f.count() >= 2 && f.last().ServerAddr == "10.0.0.2:9100"
	})
	if got := f.last().Name; got != "desk-2" {
		t.Errorf("显示名未生效: %q", got)
	}
	if got := s.Config().Server; got != "10.0.0.2:9100" {
		t.Errorf("配置未规范化: %q", got)
	}

	// 配置必须落到磁盘，重启客户端后才能记住。
	saved, err := appconfig.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if saved.Server != "10.0.0.2:9100" || saved.Name != "desk-2" {
		t.Errorf("落盘的配置 = %+v", saved)
	}
}

func TestSaveConfigRejectsInvalidAddress(t *testing.T) {
	f := &fakeRun{}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{})
	s.Start()
	defer s.Stop()
	waitFor(t, "首个连接建立", func() bool { return f.count() >= 1 })

	if err := s.SaveConfig(appconfig.Config{Server: "   "}); err == nil {
		t.Fatal("空地址应被拒绝")
	}
	if got := s.Config().Server; got != "10.0.0.1:9000" {
		t.Errorf("非法配置不应生效，当前 = %q", got)
	}
	if f.count() != 1 {
		t.Errorf("非法配置不应触发重连，agent 被拉起 %d 次", f.count())
	}
}

func TestPauseStopsAndResumeReconnects(t *testing.T) {
	f := &fakeRun{}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{})
	s.Start()
	defer s.Stop()
	waitFor(t, "首个连接建立", func() bool { return f.count() >= 1 })

	s.SetPaused(true)
	waitFor(t, "状态变为已暂停", func() bool {
		st := s.Status()
		return st.Paused && st.Phase == PhaseIdle
	})
	time.Sleep(80 * time.Millisecond)
	if got := f.count(); got != 1 {
		t.Errorf("暂停后不应重连，agent 被拉起 %d 次", got)
	}

	s.SetPaused(false)
	waitFor(t, "恢复后重新连接", func() bool { return f.count() >= 2 })
}

func TestUnconfiguredServerStaysIdle(t *testing.T) {
	f := &fakeRun{}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{}, buildinfo.Info{})
	s.Start()
	defer s.Stop()

	time.Sleep(80 * time.Millisecond)
	if got := f.count(); got != 0 {
		t.Errorf("没有 server 地址时不该连接，agent 被拉起 %d 次", got)
	}
	st := s.Status()
	if st.Phase != PhaseIdle || !strings.Contains(st.Detail, "server") {
		t.Errorf("状态 = %+v，应提示尚未配置 server", st)
	}
}

func TestLocalBackendErrorIsReportedAndRetried(t *testing.T) {
	f := &fakeRun{err: errors.New("clipboard: Wayland session needs wl-clipboard")}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{})
	s.Start()
	defer s.Stop()

	waitFor(t, "报出本地错误", func() bool { return s.Status().Phase == PhaseError })
	if got := s.Status().Detail; !strings.Contains(got, "wl-clipboard") {
		t.Errorf("状态详情未带上原始错误: %q", got)
	}
	// 首次退避 1s 后会再试：本地依赖装好之后不必重启客户端。
	waitFor(t, "自动重试", func() bool { return f.count() >= 2 })
}

func TestStopIsIdempotentAndWaitsForLoop(t *testing.T) {
	f := &fakeRun{}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{})
	s.Start()
	waitFor(t, "首个连接建立", func() bool { return f.count() >= 1 })

	done := make(chan struct{})
	go func() {
		s.Stop()
		s.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 未能在超时内返回")
	}
	if s.Status().Running {
		t.Error("停止后 Running 应为 false")
	}
}

func TestLogsBackfillAndSubscribe(t *testing.T) {
	f := &fakeRun{}
	useFakeRun(t, f)
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{})

	events, unsubscribe := s.Subscribe()
	defer unsubscribe()

	// 订阅后应立刻收到一条状态事件，界面无需额外拉取就能点亮指示灯。
	select {
	case ev := <-events:
		if ev.Kind != "status" || ev.Status == nil {
			t.Fatalf("首个事件应为状态，实际 %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到初始状态事件")
	}

	logx.Printf("clientsvc-test-marker-%d", 4242)

	gotLog := false
	deadline := time.After(2 * time.Second)
	for !gotLog {
		select {
		case ev := <-events:
			if ev.Kind == "log" && ev.Log != nil && strings.Contains(ev.Log.Text, "clientsvc-test-marker-4242") {
				gotLog = true
			}
		case <-deadline:
			t.Fatal("未收到日志事件")
		}
	}

	// 环形缓冲里也能查到这条日志，且序号可用于增量拉取。
	entries, latest := s.Logs(0, 50)
	found := false
	for _, e := range entries {
		if strings.Contains(e.Text, "clientsvc-test-marker-4242") {
			found = true
		}
	}
	if !found {
		t.Error("日志环形缓冲里找不到刚打印的记录")
	}
	if latest == 0 {
		t.Error("最新序号不应为 0")
	}
	if rest, _ := s.Logs(latest, 50); len(rest) != 0 {
		t.Errorf("按最新序号增量拉取应为空，得到 %d 条", len(rest))
	}
}

func TestEventHubRingKeepsNewestEntries(t *testing.T) {
	h := newEventHub(3)
	for i := 0; i < 5; i++ {
		h.onLog(logx.Entry{At: time.Now(), Text: string(rune('a' + i))})
	}
	entries, latest := h.logsAfter(0, 100)
	if len(entries) != 3 {
		t.Fatalf("环形缓冲应保留 3 条，得到 %d 条: %+v", len(entries), entries)
	}
	if entries[0].Text != "c" || entries[2].Text != "e" {
		t.Errorf("保留了错误的记录: %+v", entries)
	}
	if latest != 5 {
		t.Errorf("最新序号 = %d, want 5", latest)
	}

	rest, _ := h.logsAfter(entries[1].Seq, 100)
	if len(rest) != 1 || rest[0].Text != "e" {
		t.Errorf("增量拉取结果 = %+v", rest)
	}
}

func TestEventHubStatusSinceTracksPhase(t *testing.T) {
	h := newEventHub(4)
	h.setStatus(Status{Phase: PhaseConnecting})
	first := h.status().Since
	if first.IsZero() {
		t.Fatal("首次状态应记录起始时刻")
	}

	time.Sleep(10 * time.Millisecond)
	h.setStatus(Status{Phase: PhaseConnecting, Detail: "还在连"})
	if !h.status().Since.Equal(first) {
		t.Error("同一阶段内的时间戳不应被刷新")
	}

	h.setStatus(Status{Phase: PhaseConnected})
	if h.status().Since.Equal(first) {
		t.Error("阶段变化后应更新时间戳")
	}
}

func TestDisplayNameFallsBackToHostname(t *testing.T) {
	isolateConfigHome(t)

	s := New(appconfig.Config{Server: "10.0.0.1:9000"}, buildinfo.Info{})
	if got := s.DisplayName(); got != s.Hostname() {
		t.Errorf("未配置显示名时应回退到主机名，得到 %q", got)
	}
	if err := s.SaveConfig(appconfig.Config{Server: "10.0.0.1:9000", Name: "laptop"}); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if got := s.DisplayName(); got != "laptop" {
		t.Errorf("DisplayName = %q", got)
	}
}
