package clientui

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micookie2/share-clip/internal/appconfig"
	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/clientsvc"
)

// fakeController 顶替 clientsvc.Service：界面层只跟这个接口打交道，因此不必
// 真的起 agent、连 server 就能把 HTTP 行为测全。
type fakeController struct {
	mu         sync.Mutex
	cfg        appconfig.Config
	status     clientsvc.Status
	logs       []clientsvc.LogEntry
	saveErr    error
	savedCfg   appconfig.Config
	reconnects int
	pauses     []bool
	clears     int
	eventCh    chan clientsvc.Event
}

func newFakeController() *fakeController {
	return &fakeController{
		cfg:     appconfig.Default(),
		status:  clientsvc.Status{Phase: clientsvc.PhaseIdle, Detail: "尚未配置 server 地址"},
		eventCh: make(chan clientsvc.Event, 8),
	}
}

func (f *fakeController) Status() clientsvc.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeController) Config() appconfig.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakeController) Info() buildinfo.Info {
	return buildinfo.Info{Version: "9.9.9", Commit: "abc1234", BuildTime: "2026-01-01T00:00:00Z", Platform: "linux/amd64"}
}

func (f *fakeController) Hostname() string { return "test-host" }

func (f *fakeController) SaveConfig(cfg appconfig.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	if strings.TrimSpace(cfg.Server) == "" {
		return errors.New("尚未配置 server 地址")
	}
	f.savedCfg = cfg
	f.cfg = cfg
	f.status = clientsvc.Status{Phase: clientsvc.PhaseConnecting, Server: cfg.Server}
	return nil
}

func (f *fakeController) Reconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reconnects++
}

func (f *fakeController) SetPaused(paused bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pauses = append(f.pauses, paused)
	f.status.Paused = paused
}

func (f *fakeController) ClearLogs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears++
	f.logs = nil
}

func (f *fakeController) Logs(after uint64, limit int) ([]clientsvc.LogEntry, uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []clientsvc.LogEntry
	var latest uint64
	for _, e := range f.logs {
		if e.Seq > latest {
			latest = e.Seq
		}
		if e.Seq > after {
			out = append(out, e)
		}
	}
	return out, latest
}

func (f *fakeController) Subscribe() (<-chan clientsvc.Event, func()) {
	ch := make(chan clientsvc.Event, 8)
	go func() {
		for ev := range f.eventCh {
			select {
			case ch <- ev:
			default:
			}
		}
		close(ch)
	}()
	return ch, func() {}
}

func (f *fakeController) sendStatus(st clientsvc.Status) {
	f.mu.Lock()
	f.status = st
	f.mu.Unlock()
	f.eventCh <- clientsvc.Event{Kind: "status", Status: &st}
}

// newTestServer 起一个只服务界面路由的测试服务器（不占固定端口）。
func newTestServer(t *testing.T, ctl *fakeController, quit func()) *httptest.Server {
	t.Helper()
	s := New(ctl, Options{LogFile: "/tmp/client.log", TrayEnabled: true, Quit: quit})
	srv := httptest.NewServer(s.routes())
	t.Cleanup(srv.Close)
	return srv
}

func doJSON(t *testing.T, method, url string, body string, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &parsed)
	}
	return resp, parsed
}

func TestHealthIdentifiesApp(t *testing.T) {
	ctl := newFakeController()
	srv := newTestServer(t, ctl, nil)

	resp, body := doJSON(t, http.MethodGet, srv.URL+"/api/health", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	if body["app"] != appTag {
		t.Errorf("app = %v, want %v", body["app"], appTag)
	}
	if _, ok := body["pid"]; !ok {
		t.Error("健康检查应带上 pid")
	}
}

func TestStateCarriesConfigStatusAndBuildInfo(t *testing.T) {
	ctl := newFakeController()
	ctl.cfg = appconfig.Config{Server: "10.0.0.1:9000", Name: "desk", PollMs: 500, MaxPayload: 1 << 20}
	ctl.status = clientsvc.Status{Phase: clientsvc.PhaseConnected, Server: "10.0.0.1:9000", Running: true}
	srv := newTestServer(t, ctl, nil)

	resp, body := doJSON(t, http.MethodGet, srv.URL+"/api/state", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	st := body["status"].(map[string]any)
	if st["phase"] != string(clientsvc.PhaseConnected) {
		t.Errorf("status.phase = %v", st["phase"])
	}
	cfg := body["config"].(map[string]any)
	if cfg["server"] != "10.0.0.1:9000" {
		t.Errorf("config.server = %v", cfg["server"])
	}
	if body["hostname"] != "test-host" {
		t.Errorf("hostname = %v", body["hostname"])
	}
	info := body["info"].(map[string]any)
	if info["version"] != "9.9.9" {
		t.Errorf("info.version = %v", info["version"])
	}
	if body["trayEnabled"] != true {
		t.Error("trayEnabled 应为 true")
	}
	// 有 server 时顺带给出服务器管理页地址，界面才能放链接。
	if body["serverWebURL"] != "http://10.0.0.1:9000/" {
		t.Errorf("serverWebURL = %v", body["serverWebURL"])
	}
}

func TestSaveConfigAcceptsAndReportsError(t *testing.T) {
	ctl := newFakeController()
	srv := newTestServer(t, ctl, nil)

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/config",
		`{"server":"10.0.0.2:9100","name":"lap","pollMs":700,"maxPayload":2048}`,
		map[string]string{requestHeader: requestValue})
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("保存失败: %d %v", resp.StatusCode, body)
	}
	if ctl.savedCfg.Server != "10.0.0.2:9100" || ctl.savedCfg.Name != "lap" {
		t.Errorf("保存的配置 = %+v", ctl.savedCfg)
	}
	if body["state"] == nil {
		t.Error("保存成功后应回一份最新状态，界面无需再拉一次")
	}

	ctl.saveErr = errors.New("设置已生效，但保存配置文件失败（重启后可能丢失）：磁盘只读")
	resp, body = doJSON(t, http.MethodPost, srv.URL+"/api/config", `{"server":"10.0.0.3:9000"}`,
		map[string]string{requestHeader: requestValue})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("失败时状态码 = %d, want 400", resp.StatusCode)
	}
	if body["saved"] != true {
		t.Error("「已生效但未落盘」应通过 saved=true 告诉界面这是警告而不是彻底失败")
	}
	if !strings.Contains(body["error"].(string), "磁盘只读") {
		t.Errorf("错误信息应原样带回: %v", body["error"])
	}
}

func TestWriteEndpointsRequireCustomHeader(t *testing.T) {
	ctl := newFakeController()
	srv := newTestServer(t, ctl, nil)

	for _, path := range []string{"/api/config", "/api/pause", "/api/reconnect", "/api/logs/clear", "/api/quit"} {
		resp, _ := doJSON(t, http.MethodPost, srv.URL+path, `{}`, nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s 无自定义头时状态码 = %d, want 403", path, resp.StatusCode)
		}
	}
	if ctl.reconnects != 0 || ctl.clears != 0 || len(ctl.pauses) != 0 {
		t.Error("被拦下的请求不应产生任何副作用")
	}
}

func TestNonLoopbackHostIsRejected(t *testing.T) {
	ctl := newFakeController()
	srv := newTestServer(t, ctl, nil)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/state", nil)
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("非回环 Host 状态码 = %d, want 403", resp.StatusCode)
	}
}

func TestPauseAndReconnectAndClearLogs(t *testing.T) {
	ctl := newFakeController()
	srv := newTestServer(t, ctl, nil)
	h := map[string]string{requestHeader: requestValue}

	doJSON(t, http.MethodPost, srv.URL+"/api/pause", `{"paused":true}`, h)
	doJSON(t, http.MethodPost, srv.URL+"/api/pause", `{"paused":false}`, h)
	if len(ctl.pauses) != 2 || !ctl.pauses[0] || ctl.pauses[1] {
		t.Errorf("暂停调用序列 = %v", ctl.pauses)
	}

	doJSON(t, http.MethodPost, srv.URL+"/api/reconnect", `{}`, h)
	if ctl.reconnects != 1 {
		t.Errorf("reconnects = %d", ctl.reconnects)
	}

	doJSON(t, http.MethodPost, srv.URL+"/api/logs/clear", `{}`, h)
	if ctl.clears != 1 {
		t.Errorf("clears = %d", ctl.clears)
	}
}

func TestQuitInvokesCallback(t *testing.T) {
	ctl := newFakeController()
	quit := make(chan struct{})
	srv := newTestServer(t, ctl, func() { close(quit) })

	resp, body := doJSON(t, http.MethodPost, srv.URL+"/api/quit", `{}`,
		map[string]string{requestHeader: requestValue})
	if resp.StatusCode != http.StatusOK || body["ok"] != true {
		t.Fatalf("退出请求失败: %d %v", resp.StatusCode, body)
	}
	select {
	case <-quit:
	case <-time.After(3 * time.Second):
		t.Fatal("退出回调未被调用")
	}
}

func TestLogsBackfill(t *testing.T) {
	ctl := newFakeController()
	ctl.logs = []clientsvc.LogEntry{
		{Seq: 1, Text: "first"},
		{Seq: 2, Text: "second"},
		{Seq: 3, Text: "third"},
	}
	srv := newTestServer(t, ctl, nil)

	_, body := doJSON(t, http.MethodGet, srv.URL+"/api/logs?after=0&limit=10", "", nil)
	entries := body["entries"].([]any)
	if len(entries) != 3 {
		t.Fatalf("首屏应拿到 3 条，得到 %d", len(entries))
	}
	if body["latest"].(float64) != 3 {
		t.Errorf("latest = %v", body["latest"])
	}

	_, body = doJSON(t, http.MethodGet, srv.URL+"/api/logs?after=2", "", nil)
	if got := len(body["entries"].([]any)); got != 1 {
		t.Errorf("增量拉取应只拿到 1 条，得到 %d", got)
	}
}

// 打开 /api/events 后，界面应当先收到一份完整状态，随后是实时事件。
func TestEventsStreamStartsWithStatusThenLogs(t *testing.T) {
	ctl := newFakeController()
	srv := newTestServer(t, ctl, nil)

	resp, err := http.Get(srv.URL + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}

	reader := bufio.NewReader(resp.Body)
	want := []string{"event: status", "event: log"}
	for _, wantLine := range want {
		line, err := readLineWithTimeout(t, reader, 3*time.Second)
		if err != nil {
			t.Fatalf("读取 SSE 失败: %v", err)
		}
		if line != wantLine {
			t.Fatalf("SSE 行 = %q, want %q", line, wantLine)
		}
		if _, err := readLineWithTimeout(t, reader, time.Second); err != nil { // data: 行
			t.Fatalf("缺少 data 行: %v", err)
		}
		if blank, err := readLineWithTimeout(t, reader, time.Second); err != nil || blank != "" {
			t.Fatalf("SSE 帧应以空行结束，得到 %q (err=%v)", blank, err)
		}
		// 触发一条日志事件给下一轮循环读取。
		if wantLine == "event: status" {
			ctl.eventCh <- clientsvc.Event{Kind: "log", Log: &clientsvc.LogEntry{Seq: 7, Text: "hello"}}
		}
	}
}

// readLineWithTimeout 在超时内读一行（bufio.Reader 本身没有超时）。
func readLineWithTimeout(t *testing.T, r *bufio.Reader, d time.Duration) (string, error) {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := r.ReadString('\n')
		ch <- result{strings.TrimRight(line, "\r\n"), err}
	}()
	select {
	case res := <-ch:
		return res.line, res.err
	case <-time.After(d):
		return "", errors.New("读取超时")
	}
}

func TestProbeRejectsNonShareClipResponders(t *testing.T) {
	// 别的程序占了端口：Probe 必须判定「不是自己人」，客户端才会另选端口。
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"app":"something-else"}`))
	}))
	defer other.Close()
	if Probe(strings.TrimPrefix(other.URL, "http://")) {
		t.Error("Probe 不应把其它程序当成自己的实例")
	}
	if Probe("127.0.0.1:1") {
		t.Error("没人监听时 Probe 应为 false")
	}
}

func TestListenRejectsNonLoopbackAddress(t *testing.T) {
	ctl := newFakeController()
	for _, addr := range []string{"0.0.0.0:9210", ":9210", "192.168.1.10:9210", "example.com:9210"} {
		if _, err := New(ctl, Options{}).Listen(addr); err == nil {
			t.Errorf("Listen(%q) 应拒绝非回环地址", addr)
		}
	}
}

func TestListenFallsBackWhenPortBusy(t *testing.T) {
	ctl := newFakeController()

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("占位监听失败: %v", err)
	}
	defer busy.Close()

	// 端口被占用时不能直接失败：双击运行的客户端必须还能用。
	s := New(ctl, Options{})
	url, err := s.Listen(busy.Addr().String())
	if err != nil {
		t.Fatalf("端口被占用时应退到随机端口，得到 %v", err)
	}
	defer s.Close()
	if url == "http://"+busy.Addr().String() {
		t.Fatalf("退避后仍报告被占用的地址: %s", url)
	}
	resp, err := http.Get(url + "/api/health")
	if err != nil {
		t.Fatalf("退避后的界面不可访问: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("退避后的界面状态码 = %d", resp.StatusCode)
	}
}
