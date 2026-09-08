package server

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/micookie2/share-clip/internal/protocol"
)

// hub tracks the connected WebSocket clients.
type hub struct {
	mu      sync.Mutex
	clients map[*client]struct{}
}

func newHub() *hub {
	return &hub{clients: make(map[*client]struct{})}
}

func (h *hub) size() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

// clientInfo describes one connected node for status reporting.
type clientInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// list returns the registered sessions (unordered).
func (h *hub) list() []clientInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]clientInfo, 0, len(h.clients))
	for c := range h.clients {
		out = append(out, clientInfo{ID: c.id, Name: c.name})
	}
	return out
}

// add registers a session, kicking any older session that uses the same id
// (e.g. a stale connection from the same client process).
func (h *hub) add(c *client) {
	h.mu.Lock()
	var victims []*client
	for ex := range h.clients {
		if ex.id == c.id && ex != c {
			delete(h.clients, ex)
			victims = append(victims, ex)
		}
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	for _, v := range victims {
		v.kick("replaced by a new connection with the same id")
	}
}

// remove deregisters a session. It reports whether the session was actually
// present (so the caller knows whether to announce its departure).
func (h *hub) remove(c *client) bool {
	h.mu.Lock()
	_, ok := h.clients[c]
	if ok {
		delete(h.clients, c)
	}
	h.mu.Unlock()
	return ok
}

// broadcastAll sends a frame to every connected client. Clients that cannot
// keep up are disconnected.
func (h *hub) broadcastAll(frame []byte) int {
	h.mu.Lock()
	var slow []*client
	n := 0
	for c := range h.clients {
		if c.enqueue(frame) {
			n++
		} else {
			slow = append(slow, c)
		}
	}
	h.mu.Unlock()
	for _, c := range slow {
		c.srv.logf("client %s (%s) too slow, disconnecting", c.name, c.id)
		c.kick("send queue overflow")
	}
	return n
}

// broadcastOthers sends a frame to every connected client except except.
// Clients that cannot keep up are disconnected.
func (h *hub) broadcastOthers(frame []byte, except *client) int {
	h.mu.Lock()
	var slow []*client
	n := 0
	for c := range h.clients {
		if c == except {
			continue
		}
		if c.enqueue(frame) {
			n++
		} else {
			slow = append(slow, c)
		}
	}
	h.mu.Unlock()
	for _, c := range slow {
		c.srv.logf("client %s (%s) too slow, disconnecting", c.name, c.id)
		c.kick("send queue overflow")
	}
	return n
}

// kick disconnects every session (used at server shutdown).
func (h *hub) closeAll(reason string) {
	h.mu.Lock()
	all := make([]*client, 0, len(h.clients))
	for c := range h.clients {
		all = append(all, c)
	}
	h.clients = make(map[*client]struct{})
	h.mu.Unlock()
	for _, c := range all {
		c.kick(reason)
	}
}

// client is one WebSocket session. All backend access happens in the session
// goroutine; enqueue feeds its write loop.
type client struct {
	hub  *hub
	srv  *Server
	conn *websocket.Conn
	send chan []byte // buffered outbound frames

	id         string
	name       string
	registered bool

	ctx    context.Context
	cancel context.CancelFunc
}

func newClient(h *hub, s *Server, conn *websocket.Conn) *client {
	ctx, cancel := context.WithCancel(context.Background())
	return &client{
		hub:    h,
		srv:    s,
		conn:   conn,
		send:   make(chan []byte, 16),
		ctx:    ctx,
		cancel: cancel,
	}
}

// enqueue queues a frame for delivery, returning false when the client is too
// slow to keep up (the caller then drops it).
func (c *client) enqueue(frame []byte) bool {
	select {
	case c.send <- frame:
		return true
	default:
		return false
	}
}

// writeLoop drains the outbound queue and periodically sends keepalive pings.
// Any failure cancels the session so the read loop tears it down.
func (c *client) writeLoop() {
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-ping.C:
			if err := c.writeRaw(protocol.Msg{Kind: protocol.KindPing}.MustFrame(c.srv.maxPayload())); err != nil {
				c.srv.logf("client %s: ping write failed: %v", c.name, err)
				c.kick("write failure")
				return
			}
		case frame := <-c.send:
			if err := c.writeRaw(frame); err != nil {
				c.srv.logf("client %s: write failed: %v", c.name, err)
				c.kick("write failure")
				return
			}
		}
	}
}

func (c *client) writeFrame(m protocol.Msg) error {
	frame, err := m.Frame(c.srv.maxPayload())
	if err != nil {
		return err
	}
	return c.writeRaw(frame)
}

func (c *client) writeRaw(frame []byte) error {
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Second)
	defer cancel()
	return c.conn.Write(ctx, websocket.MessageBinary, frame)
}

// kick forcibly closes the connection and stops the session.
func (c *client) kick(reason string) {
	c.cancel()
	_ = c.conn.Close(websocket.StatusPolicyViolation, reason)
}
