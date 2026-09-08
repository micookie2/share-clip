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
	"log"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/micookie2/share-clip/internal/clipboard"
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
	readTimeout  = 90 * time.Second // no message from the server within this window => dead
	writeTimeout = 15 * time.Second
	pingEvery    = 20 * time.Second
)

// Agent watches the clipboard and keeps a WebSocket connection to the server.
type Agent struct {
	cfg     Config
	watcher *clipboard.Watcher
	out     chan *protocol.Msg // outbound clip messages
	dropped atomic.Int64
}

// Run blocks until ctx is cancelled. Clipboard events observed while the
// server is unreachable are dropped (the user simply copies again once the
// connection is back).
func Run(ctx context.Context, cfg Config) error {
	backend, err := clipboard.NewBackend(cfg.PollIntervalMs)
	if err != nil {
		return err
	}
	a := &Agent{cfg: cfg, out: make(chan *protocol.Msg, 16)}
	a.watcher = clipboard.NewWatcher(backend, a.onLocalChange,
		clipboard.WithOnError(func(err error) {
			if !cfg.Quiet {
				log.Printf("[client] 读取剪贴板出错: %v", err)
			}
		}))
	a.watcher.Start()
	defer a.watcher.Stop()

	backoff := time.Second
	for {
		err := a.connect(ctx)
		if ctx.Err() != nil {
			return nil // shutting down
		}
		if !cfg.Quiet {
			if err != nil {
				log.Printf("[client] 连接断开: %v，%.0fs 后重试…", err, backoff.Seconds())
			} else {
				log.Printf("[client] 连接断开，%.0fs 后重试…", backoff.Seconds())
			}
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
	if !a.cfg.Quiet {
		log.Printf("[client] 正在连接 %s …", url)
	}
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

func (a *Agent) readLoop(ctx context.Context, conn *websocket.Conn, errCh chan<- error) {
	for {
		rc, cancel := context.WithTimeout(ctx, readTimeout)
		typ, data, err := conn.Read(rc)
		cancel()
		if err != nil {
			select {
			case errCh <- err:
			default:
			}
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		m, err := protocol.Parse(data, a.maxPayload())
		if err != nil {
			if !a.cfg.Quiet {
				log.Printf("[client] 收到损坏的帧: %v", err)
			}
			continue
		}
		a.handleIncoming(m)
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
			if err := a.writeMsg(ctx, conn, &protocol.Msg{Kind: protocol.KindPing}); err != nil {
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

func (a *Agent) writeMsg(ctx context.Context, conn *websocket.Conn, m *protocol.Msg) error {
	frame, err := m.Frame(a.maxPayload())
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(wctx, websocket.MessageBinary, frame)
}

func (a *Agent) maxPayload() int {
	if a.cfg.MaxPayload <= 0 {
		return protocol.DefaultMaxPayload
	}
	return a.cfg.MaxPayload
}

// onLocalChange runs on the watcher goroutine for every genuine local copy.
func (a *Agent) onLocalChange(c clipboard.Content) {
	var m *protocol.Msg
	switch c.Kind {
	case clipboard.KindText:
		m = protocol.NewClip(protocol.MIMEText, a.cfg.ClientID, a.cfg.Name, []byte(c.Text))
	case clipboard.KindImage:
		m = protocol.NewClip(protocol.MIMEImage, a.cfg.ClientID, a.cfg.Name, c.PNG)
	default:
		return
	}
	select {
	case a.out <- m:
	default:
		n := a.dropped.Add(1)
		if !a.cfg.Quiet && (n <= 3 || n%20 == 0) {
			log.Printf("[client] 剪贴板内容未发送（未连接或发送队列已满，已丢弃 %d 条）", n)
		}
	}
}

// handleIncoming processes one server message. Incoming content is written to
// the local clipboard through the watcher, which suppresses the echo.
func (a *Agent) handleIncoming(m *protocol.Msg) {
	switch m.Kind {
	case protocol.KindWelcome:
		if !a.cfg.Quiet {
			log.Printf("[client] 已连接 server（当前在线 %d 台）", m.Count)
		}
	case protocol.KindJoined:
		if !a.cfg.Quiet {
			log.Printf("[client] %s 上线", m.ClientName)
		}
	case protocol.KindLeft:
		if !a.cfg.Quiet {
			log.Printf("[client] %s 下线", m.ClientName)
		}
	case protocol.KindClip:
		content, ok := contentFromMessage(m)
		if !ok {
			return
		}
		if err := a.watcher.ApplyRemote(content); err != nil {
			if !a.cfg.Quiet {
				log.Printf("[client] 写入剪贴板失败: %v", err)
			}
			return
		}
		if !a.cfg.Quiet {
			from := m.ClientName
			if m.ClientID == protocol.OriginWeb {
				from = "Web 推送"
			}
			switch content.Kind {
			case clipboard.KindText:
				log.Printf("[client] 收到 %s 的文本（%d 字符）：%s",
					from, len(content.Text), preview(content.Text, 60))
			case clipboard.KindImage:
				log.Printf("[client] 收到 %s 的图片（%d B）", from, len(content.PNG))
			}
		}
	case protocol.KindPing:
		// keepalive only
	}
}

func contentFromMessage(m *protocol.Msg) (clipboard.Content, bool) {
	switch m.MIME {
	case protocol.MIMEText:
		if len(m.Payload) == 0 || clipboard.IsFileArtifact(string(m.Payload)) {
			return clipboard.Content{}, false
		}
		return clipboard.Content{Kind: clipboard.KindText, Text: string(m.Payload)}, true
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

func preview(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
