// Package agent implements the share-clip client: it watches the local
// clipboard, forwards every genuine copy event to the server over WebSocket,
// applies broadcast content to the local clipboard (without echoing it back)
// and reconnects automatically when the server is unreachable.
package agent

import (
	"bytes"
	"context"
	"fmt"
	"image/png"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/micookie2/share-clip/internal/clipboard"
	"github.com/micookie2/share-clip/internal/logx"
	"github.com/micookie2/share-clip/internal/protocol"
)

// Config configures a client agent.
type Config struct {
	ServerAddr     string // host:port of the share-clip server
	ClientID       string // unique identity for this client process
	Name           string // display name (defaults to the host name)
	PollIntervalMs int    // clipboard poll interval used by Linux
	MaxPayload     int    // maximum clipboard payload accepted
	Quiet          bool
}

const (
	writeTimeout       = 15 * time.Second // per-frame write budget
	pingEvery          = 20 * time.Second // WebSocket control-ping cadence
	pongWait           = 10 * time.Second // one missed pong => connection is dead
	stableConnDuration = 30 * time.Second // reset reconnect backoff after a healthy session
)

// Agent watches the clipboard and keeps a WebSocket connection to the server.
type Agent struct {
	cfg      Config
	watcher  *clipboard.Watcher
	out      chan *protocol.Msg // outbound clip messages
	incoming chan *protocol.Msg // inbound clip messages, applied off the read path
	dropped  atomic.Int64

	// sentLog remembers what this client recently put on the wire so an echo
	// of its own copy (which the server normally never relays back) cannot
	// overwrite the local clipboard. See isOwnEcho.
	sentMu  sync.Mutex
	sentLog []sentClip
}

// sentClip is one entry of the recent-send ring used for echo protection.
type sentClip struct {
	digest [32]byte
	plain  string
	rich   bool
	at     time.Time
}

// echoWindow bounds how long after sending a copy an incoming clip is still
// treated as a possible echo of it; echoLogMax bounds the ring size.
const (
	echoWindow = 20 * time.Second
	echoLogMax = 16
)

// logf writes one log record unless the agent runs quiet. Every record goes
// through logx, so a clipboard payload can never split it over several lines.
func (a *Agent) logf(format string, args ...any) {
	if a.cfg.Quiet {
		return
	}
	logx.Printf(format, args...)
}

// Run blocks until ctx is cancelled. Clipboard events observed while the
// server is unreachable are dropped (the user simply copies again once the
// connection is back).
func Run(ctx context.Context, cfg Config) error {
	backend, err := clipboard.NewBackend(cfg.PollIntervalMs)
	if err != nil {
		return err
	}
	a := &Agent{
		cfg:      cfg,
		out:      make(chan *protocol.Msg, 16),
		incoming: make(chan *protocol.Msg, 16),
	}
	// Surface clipboard degradations (e.g. a rich write falling back to plain
	// text) in the client log instead of losing formatting silently.
	clipboard.SetWarnf(a.logf)
	a.watcher = clipboard.NewWatcher(backend, a.onLocalChange,
		clipboard.WithOnError(func(err error) {
			a.logf("[client] 读取剪贴板出错: %v", err)
		}))
	a.watcher.Start()
	defer a.watcher.Stop()

	// applyLoop applies received content off the network read path, so a slow
	// clipboard write can never stall the WebSocket reader and delay heartbeat
	// pongs (the usual cause of "heartbeat: failed to wait for pong" drops).
	go a.applyLoop(ctx)

	backoff := time.Second
	for {
		start := time.Now()
		err := a.connect(ctx)
		stable := time.Since(start) >= stableConnDuration
		if ctx.Err() != nil {
			return nil // shutting down
		}
		if err != nil {
			a.logf("[client] 连接断开: %v，%.0fs 后重试…", err, backoff.Seconds())
		} else {
			a.logf("[client] 连接断开，%.0fs 后重试…", backoff.Seconds())
		}
		// A session that stayed healthy for a while is not flapping: reset the
		// backoff so the next reconnect is prompt instead of waiting the full
		// accumulated delay.
		if stable {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

// connect runs one session until it dies; it returns nil (retry) unless ctx
// is cancelled.
func (a *Agent) connect(ctx context.Context) error {
	url := "ws://" + a.cfg.ServerAddr + "/ws"
	a.logf("[client] 正在连接 %s …", url)
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", url, err)
	}
	// The library default read limit is 32 KiB; raise it so large clipboard
	// payloads (up to -max-payload) fit in a single WebSocket message.
	conn.SetReadLimit(int64(a.maxPayload()) + protocol.FrameOverhead)
	if err := a.writeMsg(ctx, conn, &protocol.Msg{
		Kind:       protocol.KindHello,
		ClientID:   a.cfg.ClientID,
		ClientName: a.cfg.Name,
	}); err != nil {
		conn.CloseNow()
		return fmt.Errorf("hello: %w", err)
	}

	sessCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 2)
	go a.readLoop(sessCtx, conn, errCh)
	go a.writeLoop(sessCtx, conn, errCh)

	err = <-errCh
	cancel()
	_ = conn.Close(websocket.StatusGoingAway, "bye")
	a.drainOut()
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// readLoop consumes data messages until the session ends. Liveness is not
// checked here: keepalive uses WebSocket control pings (see heartbeat), which
// the library answers and never surfaces as data, and a perfectly idle
// clipboard session legitimately receives no data frames for a long time.
// Received clips are handed to applyLoop instead of being applied inline, so
// clipboard I/O can never block this reader and delay a heartbeat pong.
func (a *Agent) readLoop(ctx context.Context, conn *websocket.Conn, errCh chan<- error) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		if typ != websocket.MessageBinary {
			a.logf("[recv] 忽略非二进制帧 (type=%d)", typ)
			continue
		}
		m, err := protocol.Parse(data, a.maxPayload())
		if err != nil {
			a.logf("[recv] 损坏的帧: %v", err)
			continue
		}
		a.logf("[recv] %s", m.Summary())
		select {
		case a.incoming <- m:
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) writeLoop(ctx context.Context, conn *websocket.Conn, errCh chan<- error) {
	ping := time.NewTicker(pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if err := a.heartbeat(ctx, conn); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		case m := <-a.out:
			if err := a.writeMsg(ctx, conn, m); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}
}

// heartbeat sends a WebSocket ping (RFC 6455) and waits for the peer's pong.
// The library answers pings we receive and matches the pong of our own ping
// while readLoop reads, so a pong missing within pongWait means the server is
// gone or the path is broken in one direction.
func (a *Agent) heartbeat(ctx context.Context, conn *websocket.Conn) error {
	pctx, cancel := context.WithTimeout(ctx, pongWait)
	defer cancel()
	if err := conn.Ping(pctx); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (a *Agent) writeMsg(ctx context.Context, conn *websocket.Conn, m *protocol.Msg) error {
	frame, err := m.Frame(a.maxPayload())
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageBinary, frame); err != nil {
		return err
	}
	a.logf("[send] %s", m.Summary())
	return nil
}

func (a *Agent) maxPayload() int {
	if a.cfg.MaxPayload <= 0 {
		return protocol.DefaultMaxPayload
	}
	return a.cfg.MaxPayload
}

// onLocalChange runs on the watcher goroutine for every genuine local copy.
func (a *Agent) onLocalChange(c clipboard.Content) {
	a.rememberSent(c)
	var m *protocol.Msg
	switch c.Kind {
	case clipboard.KindText:
		if c.HTML != "" {
			m = protocol.NewClipHTML(a.cfg.ClientID, a.cfg.Name, []byte(c.HTML), []byte(c.Text))
		} else {
			m = protocol.NewClip(protocol.MIMEText, a.cfg.ClientID, a.cfg.Name, []byte(c.Text))
		}
	case clipboard.KindImage:
		m = protocol.NewClip(protocol.MIMEImage, a.cfg.ClientID, a.cfg.Name, c.PNG)
	default:
		return
	}
	select {
	case a.out <- m:
	default:
		n := a.dropped.Add(1)
		if n <= 3 || n%20 == 0 {
			a.logf("[client] 剪贴板内容未发送（未连接或发送队列已满，已丢弃 %d 条）", n)
		}
	}
}

// applyLoop writes received clipboard content to the local clipboard on its
// own goroutine. Keeping this off the network read loop means a slow clipboard
// write (xclip/wl-clipboard can block on selection contention) can never stop
// the reader from processing heartbeat pongs.
func (a *Agent) applyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case m := <-a.incoming:
			a.handleIncoming(m)
		}
	}
}

// handleIncoming applies clipboard content from the server. Every frame is
// already logged by readLoop; control frames need no further action here.
// Incoming content is written to the local clipboard through the watcher,
// which suppresses the echo.
func (a *Agent) handleIncoming(m *protocol.Msg) {
	if m.Kind != protocol.KindClip {
		return
	}
	content, ok := contentFromMessage(m)
	if !ok {
		return
	}
	if a.isOwnEcho(content) {
		a.logf("[client] 忽略自身副本回传，不改写本机剪贴板: %s", m.Summary())
		return
	}
	if err := a.watcher.ApplyRemote(content); err != nil {
		a.logf("[client] 写入剪贴板失败: %v", err)
	} else {
		a.logf("[client] 已写入本机剪贴板: %s", m.Summary())
	}
}

// rememberSent records content this client just shared so a later echo of it
// can be recognized. It runs on the watcher goroutine.
func (a *Agent) rememberSent(c clipboard.Content) {
	a.sentMu.Lock()
	defer a.sentMu.Unlock()
	a.sentLog = append(a.sentLog, sentClip{
		digest: c.Digest(),
		plain:  c.Text,
		rich:   c.HTML != "",
		at:     time.Now(),
	})
	if len(a.sentLog) > echoLogMax {
		a.sentLog = a.sentLog[len(a.sentLog)-echoLogMax:]
	}
}

// isOwnEcho reports whether incoming content is this client's own recent copy
// coming back. The server normally relays a clip to every client except its
// sender, so this should not happen; when it does (for example a peer echoes
// the clip back, or downgrades a rich copy to plain text on the way), applying
// it would replace the local clipboard with a copy of what the user just
// copied — the "share-clip clobbered my clipboard" symptom. Echoes are
// therefore ignored, within echoWindow of the send.
func (a *Agent) isOwnEcho(c clipboard.Content) bool {
	now := time.Now()
	d := c.Digest()
	a.sentMu.Lock()
	defer a.sentMu.Unlock()
	for len(a.sentLog) > 0 && now.Sub(a.sentLog[0].at) > echoWindow {
		a.sentLog = a.sentLog[1:]
	}
	for _, s := range a.sentLog {
		if s.digest == d {
			return true
		}
		// A rich copy that came back as plain text only (formatting dropped
		// somewhere in the round trip) is still the same copy.
		if s.rich && c.HTML == "" && c.Text != "" && s.plain == c.Text {
			return true
		}
	}
	return false
}

func contentFromMessage(m *protocol.Msg) (clipboard.Content, bool) {
	switch m.MIME {
	case protocol.MIMEText:
		if len(m.Payload) == 0 || clipboard.IsFileArtifact(string(m.Payload)) {
			return clipboard.Content{}, false
		}
		return clipboard.Content{Kind: clipboard.KindText, Text: string(m.Payload)}, true
	case protocol.MIMEHTML:
		if len(m.Payload) == 0 {
			return clipboard.Content{}, false
		}
		return clipboard.Content{
			Kind: clipboard.KindText,
			Text: string(m.Payload2),
			HTML: string(m.Payload),
		}, true
	case protocol.MIMEImage:
		if len(m.Payload) == 0 {
			return clipboard.Content{}, false
		}
		if !validPNG(m.Payload) {
			return clipboard.Content{}, false
		}
		return clipboard.Content{Kind: clipboard.KindImage, PNG: m.Payload}, true
	}
	return clipboard.Content{}, false
}

func validPNG(data []byte) bool {
	if len(data) < 8 || data[0] != 0x89 || data[1] != 'P' || data[2] != 'N' || data[3] != 'G' {
		return false
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > 1<<26 {
		return false
	}
	return true
}

// drainOut drops queued outbound messages that belong to the dead session.
func (a *Agent) drainOut() {
	for {
		select {
		case <-a.out:
		default:
			return
		}
	}
}
