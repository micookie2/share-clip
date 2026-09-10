// Package clientui 实现桌面模式客户端的本地界面：一个只监听回环地址的小型
// HTTP 服务，外加用系统默认浏览器打开的静态页面。
//
// 为什么是网页而不是原生窗口：客户端要同时支持 Windows 与 Linux，原生窗口
// 意味着每个平台一套 GUI 依赖（以及交叉编译时的一堆系统库），而这里只需要
// 标准库——页面本身与 server 的管理页同一套视觉语言，改起来也只动 HTML。
//
// 界面只做三件事：显示连接状态、改配置、看日志。所有数据都来自 Controller。
package clientui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/micookie2/share-clip/assets"
	"github.com/micookie2/share-clip/internal/appconfig"
	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/clientsvc"
	"github.com/micookie2/share-clip/internal/logx"
)

//go:embed webui
var webuiFS embed.FS

// appTag 出现在 /api/health 里，用于「双击第二次时把已有界面调出来」的探测：
// 只有带这个标记的服务才被认定为自己的另一个实例。
const appTag = "share-clip-client"

// requestHeader 是写操作要求带上的自定义头。浏览器的跨站表单、<img>、<script>
// 都不可能带上它，而带自定义头的跨站 fetch 会先发 CORS 预检——我们不响应预检，
// 请求就被浏览器拦下了。这挡的是「恶意网页改我配置/关我客户端」。
const (
	requestHeader = "X-Requested-With"
	requestValue  = "share-clip"
)

// Controller 是界面用到的服务能力，由 clientsvc.Service 实现；抽成接口是为了
// 让界面层的测试不必真的去连 server。
type Controller interface {
	Status() clientsvc.Status
	Config() appconfig.Config
	Info() buildinfo.Info
	Hostname() string
	SaveConfig(appconfig.Config) error
	Reconnect()
	SetPaused(bool)
	ClearLogs()
	Logs(after uint64, limit int) ([]clientsvc.LogEntry, uint64)
	Subscribe() (<-chan clientsvc.Event, func())
}

// Options 是界面服务的可选配置。
type Options struct {
	LogFile     string // 日志文件路径，页脚展示用
	TrayEnabled bool   // 托盘是否可用，页脚展示用
	Quit        func() // 点击「退出客户端」时调用
}

// Server 是本地界面服务。
type Server struct {
	ctl  Controller
	opts Options

	mu   sync.Mutex
	ln   net.Listener
	http *http.Server
	url  string
}

// New 创建界面服务（尚未开始监听）。
func New(ctl Controller, opts Options) *Server {
	return &Server{ctl: ctl, opts: opts}
}

// Listen 在 preferred（如 127.0.0.1:9210）上监听并开始服务。端口被其它程序
// 占用时自动退到随机端口，保证「双击就能用」。返回可以直接打开的 URL。
//
// 只接受回环地址：界面能改配置、断开同步、退出客户端，暴露到局域网没有意义
// （写请求本来也只认回环 Host），不如在启动时就说清楚。
func (s *Server) Listen(preferred string) (string, error) {
	host, _, splitErr := net.SplitHostPort(preferred)
	if splitErr != nil || !isLoopbackHost(host) {
		return "", fmt.Errorf("本地界面只允许监听回环地址（如 127.0.0.1:9210），收到 %q", preferred)
	}

	ln, err := net.Listen("tcp", preferred)
	if err != nil {
		// 记住原因：退到随机端口后要在这里解释一句，否则用户不知道地址变了。
		firstErr := err
		ln, err = net.Listen("tcp", net.JoinHostPort(host, "0"))
		if err != nil {
			return "", fmt.Errorf("本地界面无法监听 %s: %w", preferred, firstErr)
		}
		logx.Printf("[client] 端口 %s 已被占用，本地界面改用随机端口", preferred)
	}

	s.mu.Lock()
	s.ln = ln
	s.url = "http://" + ln.Addr().String()
	s.http = &http.Server{
		Handler:           s.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	srv := s.http
	url := s.url
	s.mu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logx.Printf("[client] 本地界面服务退出: %v", err)
		}
	}()
	return url, nil
}

// URL 返回当前界面地址（未监听时为空）。
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.url
}

// Close 停止界面服务。
func (s *Server) Close() error {
	s.mu.Lock()
	srv := s.http
	s.http = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// Probe 检查 addr 上是否已经跑着一个 share-clip 客户端界面。用于双击第二次时
// 不重复起进程，而是直接把已有界面打开。
func Probe(addr string) bool {
	client := &http.Client{Timeout: 800 * time.Millisecond}
	resp, err := client.Get("http://" + addr + "/api/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		App string `json:"app"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return false
	}
	return body.App == appTag
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleIndex)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/logs", s.handleLogs)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("POST /api/config", s.handleSaveConfig)
	mux.HandleFunc("POST /api/reconnect", s.handleReconnect)
	mux.HandleFunc("POST /api/pause", s.handlePause)
	mux.HandleFunc("POST /api/logs/clear", s.handleClearLogs)
	mux.HandleFunc("POST /api/quit", s.handleQuit)
	mux.HandleFunc("GET /favicon.svg", assetHandler(assets.IconSVG, "image/svg+xml"))
	mux.HandleFunc("GET /favicon.ico", assetHandler(assets.IconICO, "image/x-icon"))
	mux.HandleFunc("GET /icon-192.png", assetHandler(assets.Icon192, "image/png"))
	mux.HandleFunc("GET /apple-touch-icon.png", assetHandler(assets.AppleTouchIcon, "image/png"))
	return s.guard(mux)
}

// guard 做两件本地服务的常规防护：
//
//  1. Host 必须是回环地址——挡 DNS rebinding：外部域名解析到 127.0.0.1 时，
//     浏览器带的是那个域名的 Host，这里直接拒绝。
//  2. 写操作必须带自定义请求头——挡跨站请求伪造（见 requestHeader 注释）。
//
// 界面只在本机使用，读写都不涉及密钥，这两条足够，不必再引入 token。
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(requestHeader) != requestValue {
				http.Error(w, "missing "+requestHeader+" header", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// isLoopbackHost 判断请求的 Host 是否指向本机回环地址。
func isLoopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := webuiFS.ReadFile("webui/index.html")
	if err != nil {
		http.Error(w, "页面缺失: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"app":     appTag,
		"version": s.ctl.Info().Version,
		"pid":     os.Getpid(),
	})
}

// statePayload 是页面初始化与轮询时拿到的全部内容。
type statePayload struct {
	Status       clientsvc.Status `json:"status"`
	Config       appconfig.Config `json:"config"`
	Info         buildinfo.Info   `json:"info"`
	Hostname     string           `json:"hostname"`
	LogFile      string           `json:"logFile,omitempty"`
	ConfigPath   string           `json:"configPath,omitempty"`
	TrayEnabled  bool             `json:"trayEnabled"`
	ServerWebURL string           `json:"serverWebURL,omitempty"`
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.state())
}

func (s *Server) state() statePayload {
	cfg := s.ctl.Config()
	sp := statePayload{
		Status:      s.ctl.Status(),
		Config:      cfg,
		Info:        s.ctl.Info(),
		Hostname:    s.ctl.Hostname(),
		LogFile:     s.opts.LogFile,
		ConfigPath:  configPath(),
		TrayEnabled: s.opts.TrayEnabled,
	}
	if cfg.Server != "" {
		sp.ServerWebURL = "http://" + cfg.Server + "/"
	}
	return sp
}

// handleLogs 返回历史日志：after=0 表示最近一批（界面首次加载），否则增量。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	after := parseUint(r.URL.Query().Get("after"))
	limit := int(parseUint(r.URL.Query().Get("limit")))
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	entries, latest := s.ctl.Logs(after, limit)
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries, "latest": latest})
}

// handleEvents 用 SSE 把日志与状态变化实时推给页面。
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	events, unsubscribe := s.ctl.Subscribe()
	defer unsubscribe()

	// 先给页面一份完整状态，避免它还要单独拉一次才知道自己连的是哪台 server。
	if err := writeSSE(w, "status", s.state()); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := s.writeEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent 把一个服务事件翻译成 SSE 帧。状态事件附带完整快照，页面直接覆盖
// 渲染即可，不必再去理解增量。
func (s *Server) writeEvent(w io.Writer, ev clientsvc.Event) error {
	switch ev.Kind {
	case "status":
		return writeSSE(w, "status", s.state())
	case "cleared":
		return writeSSE(w, "cleared", map[string]any{})
	case "log":
		if ev.Log == nil {
			return nil
		}
		return writeSSE(w, "log", ev.Log)
	}
	return nil
}

func (s *Server) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Server     string `json:"server"`
		Key        string `json:"key"`
		Name       string `json:"name"`
		PollMs     int    `json:"pollMs"`
		MaxPayload int    `json:"maxPayload"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	cfg := appconfig.Config{
		Server:     body.Server,
		Key:        body.Key,
		Name:       body.Name,
		PollMs:     body.PollMs,
		MaxPayload: body.MaxPayload,
	}
	if err := s.ctl.SaveConfig(cfg); err != nil {
		// 校验失败与「已生效但没存下来」是两回事，用 saved 字段让页面区分提示。
		saved := strings.Contains(err.Error(), "设置已生效")
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(),
			"saved": saved,
			"state": s.state(),
		})
		return
	}
	logx.Printf("[client] 已应用界面上的设置")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.state()})
}

func (s *Server) handleReconnect(w http.ResponseWriter, r *http.Request) {
	s.ctl.Reconnect()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.state()})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paused bool `json:"paused"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		return
	}
	s.ctl.SetPaused(body.Paused)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": s.state()})
}

func (s *Server) handleClearLogs(w http.ResponseWriter, r *http.Request) {
	s.ctl.ClearLogs()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleQuit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	if s.opts.Quit != nil {
		logx.Printf("[client] 收到界面上的退出请求")
		// 先把响应发出去再退出，否则浏览器只会看到一个断开的连接。
		go func() {
			time.Sleep(150 * time.Millisecond)
			s.opts.Quit()
		}()
	}
}

func assetHandler(data []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(data)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// decodeJSON 读取请求体；出错时已写好 400 响应。
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误: " + err.Error()})
		return err
	}
	return nil
}

func writeSSE(w io.Writer, event string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	return nil
}

// configPath 返回配置文件路径，页面页脚展示出来，方便用户备份/排查。
func configPath() string {
	p, err := appconfig.Path()
	if err != nil {
		return ""
	}
	return p
}

func parseUint(s string) uint64 {
	n, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
