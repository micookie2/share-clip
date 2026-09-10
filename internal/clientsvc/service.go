// Package clientsvc 是桌面模式客户端的「大脑」：它持有用户配置，按配置拉起
// agent（剪贴板监听 + WebSocket 长连接），在配置变化时重启它，并把当前状态与
// 日志暴露给本地网页界面和系统托盘。
//
// 之所以单独成包：cmd/shareclip-client 只负责选择运行模式（控制台/桌面）并
// 把托盘、界面、agent 串起来；真正的生命周期与状态机放在这里，才能被单元测试
// 直接驱动，而不必真的开一个图形会话。
package clientsvc

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/micookie2/share-clip/internal/agent"
	"github.com/micookie2/share-clip/internal/appconfig"
	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/logx"
)

// Phase 描述客户端当前处于哪个阶段，界面与托盘据此显示状态。
type Phase string

const (
	PhaseIdle       Phase = "idle"       // 未配置 server 地址，或同步已暂停
	PhaseConnecting Phase = "connecting" // 正在连接
	PhaseConnected  Phase = "connected"  // 已连上 server
	PhaseRetrying   Phase = "retrying"   // 连不上，等待重试
	PhaseError      Phase = "error"      // 本地环境问题（如剪贴板不可用）
)

// Status 是给界面与托盘看的一份状态快照。
type Status struct {
	Phase     Phase     `json:"phase"`
	Detail    string    `json:"detail"`              // 一句话说明，可直接展示
	Server    string    `json:"server"`              // 当前使用的 server 地址
	Name      string    `json:"name"`                // 本机显示名
	Paused    bool      `json:"paused"`              // 是否被用户暂停
	Running   bool      `json:"running"`             // agent 循环是否在跑
	Since     time.Time `json:"since,omitempty"`     // 进入当前阶段的时刻
	RetryInMs int       `json:"retryInMs,omitempty"` // 距下次重连的毫秒数
}

// LogEntry 是日志视图里的一行。
type LogEntry struct {
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Event 是推送给界面的实时事件：一条日志，或一次状态变化。
type Event struct {
	Kind   string    `json:"kind"` // "log" | "status"
	Log    *LogEntry `json:"log,omitempty"`
	Status *Status   `json:"status,omitempty"`
}

// runAgent 是「跑一个 agent 直到被取消」的接缝：生产环境就是 agent.Run，
// 测试里替换成假实现，就能在没有任何图形会话、没有 server 的情况下验证
// 生命周期（启动、配置变更重启、暂停）。
var runAgent = agent.Run

// Service 拥有配置、agent 循环与日志环。
type Service struct {
	info     buildinfo.Info
	hub      *eventHub
	clientID string
	hostname string

	mu     sync.Mutex
	cfg    appconfig.Config
	paused bool
	gen    uint64 // 每次配置变化自增，用于判断在跑的 agent 是否已过期

	loopCtx  context.Context
	cancel   context.CancelFunc
	loopDone chan struct{}
	pokeCh   chan struct{} // 唤醒 agent 循环：配置变了 / 要立刻重连

	stopOnce sync.Once
}

// New 用给定配置创建一个服务（不启动任何循环，需再调 Start）。
func New(cfg appconfig.Config, info buildinfo.Info) *Service {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	cfg = cfg.WithDefaults()
	s := &Service{
		info:     info,
		hub:      newEventHub(2000),
		clientID: fmt.Sprintf("%s-%d-%s", host, os.Getpid(), randHex(4)),
		hostname: host,
		cfg:      cfg,
		pokeCh:   make(chan struct{}, 1),
	}
	s.hub.setStatus(s.buildStatus(PhaseIdle, "", 0))
	return s
}

// Info 返回构建信息（界面页脚展示用）。
func (s *Service) Info() buildinfo.Info { return s.info }

// Config 返回当前配置的快照。
func (s *Service) Config() appconfig.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// Status 返回当前状态快照。
func (s *Service) Status() Status { return s.hub.status() }

// Paused 报告同步是否被用户暂停。
func (s *Service) Paused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// Hostname 返回本机主机名（界面上作为显示名的默认值）。
func (s *Service) Hostname() string { return s.hostname }

// DisplayName 返回当前生效的显示名。
func (s *Service) DisplayName() string {
	cfg := s.Config()
	if cfg.Name != "" {
		return cfg.Name
	}
	return s.hostname
}

// Start 启动 agent 循环。可重复调用；已经在跑时什么都不做。
func (s *Service) Start() {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.loopCtx, s.cancel = ctx, cancel
	s.loopDone = make(chan struct{})
	done := s.loopDone
	s.mu.Unlock()

	go s.run(ctx, done)
}

// Stop 停止 agent 循环并等待它退出。可重复调用。
func (s *Service) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	done := s.loopDone
	s.cancel, s.loopCtx = nil, nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
	s.hub.setStatus(s.buildStatus(PhaseIdle, "已停止", 0))
	logx.Printf("[client] 同步已停止")
}

// SetPaused 暂停/恢复同步：暂停会断开与 server 的连接，恢复则立即重连。
// 暂停期间本机复制的内容不会补发（与断线期间的行为一致）。
func (s *Service) SetPaused(paused bool) {
	s.mu.Lock()
	changed := s.paused != paused
	s.paused = paused
	s.mu.Unlock()
	if !changed {
		return
	}
	if paused {
		logx.Printf("[client] 已暂停同步（不再收发剪贴板内容）")
		// 立刻更新对外状态：界面的按钮与托盘的勾选状态都从状态快照读，
		// 不能等到 agent 循环转一圈才生效，否则点了没反应。
		s.hub.setStatus(s.buildStatus(PhaseIdle, "已暂停同步，本机复制不会发送", 0))
	} else {
		logx.Printf("[client] 已恢复同步")
		s.hub.setStatus(s.buildStatus(s.connectingPhase(), s.connectingDetail(), 0))
	}
	s.poke()
}

// connectingPhase/connectingDetail 描述「马上要开始连接」这一瞬间的状态。
func (s *Service) connectingPhase() Phase {
	if s.Config().Server == "" {
		return PhaseIdle
	}
	return PhaseConnecting
}

func (s *Service) connectingDetail() string {
	if addr := s.Config().Server; addr != "" {
		return "正在连接 " + addr + " …"
	}
	return "尚未配置 server 地址，请在界面里填写"
}

// SaveConfig 校验、应用并持久化新配置，然后立刻用新配置重连。
//
// 即使写入配置文件失败，内存中的设置仍会生效——用户的意图是「改成这个地址」，
// 而不是「写文件」；此时返回的 error 只用于提示保存失败，界面会把它当成警告。
func (s *Service) SaveConfig(cfg appconfig.Config) error {
	if err := appconfig.Validate(cfg); err != nil {
		return err
	}
	normalized, err := cfg.Normalized()
	if err != nil {
		return err
	}

	s.mu.Lock()
	old := s.cfg
	s.cfg = normalized
	s.gen++
	s.mu.Unlock()

	logx.Printf("[client] 配置已更新：server=%s，显示名=%s", normalized.Server, s.DisplayName())
	if normalized.Server != old.Server {
		logx.Printf("[client] server 地址由 %s 变更为 %s，正在重连", old.Server, normalized.Server)
	}
	if !s.Paused() {
		// 与 SetPaused 同理：立刻把「正在连接」摆出来，界面点了保存就有反馈。
		s.hub.setStatus(s.buildStatus(s.connectingPhase(), s.connectingDetail(), 0))
	}
	s.poke()

	if err := normalized.Save(); err != nil {
		return fmt.Errorf("设置已生效，但保存配置文件失败（重启后可能丢失）：%w", err)
	}
	return nil
}

// Reconnect 丢弃当前连接，立刻用现有配置重新连接。
func (s *Service) Reconnect() {
	logx.Printf("[client] 手动重连")
	s.poke()
}

// Logs 返回序号大于 after 的日志（最多 limit 条）。返回值第二个是当前最新序号，
// 界面用它继续增量拉取。
func (s *Service) Logs(after uint64, limit int) ([]LogEntry, uint64) {
	return s.hub.logsAfter(after, limit)
}

// Subscribe 订阅实时事件流（日志 + 状态变化）。注销函数可重复调用。
func (s *Service) Subscribe() (<-chan Event, func()) { return s.hub.subscribe() }

// ClearLogs 清空日志环形缓冲，并通知界面清空日志视图。日志文件不受影响。
func (s *Service) ClearLogs() { s.hub.clearLogs() }

// poke 叫醒 agent 循环；缓冲区为 1，重复请求会自然合并。
func (s *Service) poke() {
	select {
	case s.pokeCh <- struct{}{}:
	default:
	}
}

// run 是 agent 循环：反复地「按当前配置起一个 agent，直到被要求重启或本地环境
// 出错」。agent.Run 自己负责断线重连，所以它返回 nil 只可能是被我们取消。
func (s *Service) run(ctx context.Context, done chan struct{}) {
	defer close(done)

	backoff := time.Second
	for {
		cfg, paused, gen := s.snapshot()
		if paused {
			s.hub.setStatus(s.buildStatus(PhaseIdle, "已暂停同步，本机复制不会发送", 0))
			if !s.waitPoke(ctx, time.Hour) {
				return
			}
			continue
		}
		if cfg.Server == "" {
			s.hub.setStatus(s.buildStatus(PhaseIdle, "尚未配置 server 地址，请在界面里填写", 0))
			if !s.waitPoke(ctx, time.Hour) {
				return
			}
			continue
		}

		s.hub.setStatus(s.buildStatus(PhaseConnecting, "正在连接 "+cfg.Server+" …", 0))
		runCtx, cancelRun := context.WithCancel(ctx)
		watchDone := make(chan struct{})
		go func() {
			defer close(watchDone)
			select {
			case <-runCtx.Done():
			case <-s.pokeCh:
				cancelRun() // 配置变了 / 要求重连：结束本次连接
			}
		}()

		err := runAgent(runCtx, s.agentConfig(cfg))
		cancelRun()
		<-watchDone

		if ctx.Err() != nil {
			return
		}
		// 配置在本次连接期间变过：立刻按新配置重来，不用退避。
		if s.generation() != gen {
			continue
		}
		if err == nil {
			// 没被取消却正常返回：agent 只会在上下文结束时这样返回，保险起见
			// 短暂等待后重来，避免万一变成忙循环。
			if !s.waitPoke(ctx, time.Second) {
				return
			}
			continue
		}

		// 本地环境问题（典型：没有图形会话、缺少 xclip/wl-clipboard）。这跟
		// 网络断线不同，agent 内部不会自愈，所以要在这里退避重试，让用户装好
		// 依赖后不必重启客户端。
		s.hub.setStatus(s.buildStatus(PhaseError, err.Error(), backoff))
		logx.Printf("[client] 无法开始同步: %v（%.0fs 后重试）", err, backoff.Seconds())
		if !s.waitPoke(ctx, backoff) {
			return
		}
		s.hub.setStatus(s.buildStatus(PhaseError, err.Error(), 0))
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// waitPoke 最多等待 d，或被 poke 提前唤醒；ctx 结束时返回 false。
func (s *Service) waitPoke(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-s.pokeCh:
		return true
	case <-t.C:
		return true
	}
}

func (s *Service) snapshot() (appconfig.Config, bool, uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.paused, s.gen
}

func (s *Service) generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gen
}

// agentConfig 把服务配置翻译成 agent 配置，并挂上状态回调。
func (s *Service) agentConfig(cfg appconfig.Config) agent.Config {
	return agent.Config{
		ServerAddr:     cfg.Server,
		Key:            cfg.Key,
		ClientID:       s.clientID,
		Name:           s.DisplayName(),
		PollIntervalMs: cfg.PollMs,
		MaxPayload:     cfg.MaxPayload,
		OnConnected: func(addr string) {
			s.hub.setStatus(s.buildStatus(PhaseConnected, "已连接 "+addr, 0))
		},
		OnDisconnected: func(err error, retryIn time.Duration) {
			detail := "连接已断开"
			if err != nil {
				detail = err.Error()
			}
			s.hub.setStatus(s.buildStatus(PhaseRetrying, detail, retryIn))
		},
	}
}

// buildStatus 组装状态快照；Since 由 hub 负责在阶段变化时刷新。
func (s *Service) buildStatus(phase Phase, detail string, retryIn time.Duration) Status {
	cfg := s.Config()
	return Status{
		Phase:     phase,
		Detail:    detail,
		Server:    cfg.Server,
		Name:      s.DisplayName(),
		Paused:    s.Paused(),
		Running:   s.running(),
		RetryInMs: int(retryIn / time.Millisecond),
	}
}

func (s *Service) running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancel != nil
}

// randHex 生成 n 字节的随机十六进制串，用于把同一台机器上的多个客户端实例
// 区分开。
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
