package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/micookie2/share-clip/assets"
	"github.com/micookie2/share-clip/internal/logx"
	"github.com/micookie2/share-clip/internal/protocol"
	"github.com/micookie2/share-clip/internal/store"
)

//go:embed webui
var webuiFS embed.FS

// Config configures the share-clip server.
type Config struct {
	Addr         string // listen address, e.g. ":9000"
	DBPath       string // SQLite database file, e.g. "shareclip.db"
	HistoryLimit int    // max history entries kept (pruned automatically)
	MaxPayload   int    // max accepted clipboard payload in bytes
	Quiet        bool   // reduce logging
}

// Defaults used when Config fields are zero.
const (
	DefaultAddr       = ":9000"
	DefaultDBPath     = "shareclip.db"
	DefaultHistory    = 500
	DefaultMaxPayload = 32 << 20
	readDeadline      = 90 * time.Second
	helloDeadline     = 30 * time.Second
)

// Server is the share-clip server: WebSocket endpoint for clients, SQLite
// history and a web UI.
type Server struct {
	cfg    Config
	store  *store.Store
	hub    *hub
	events *broker
	logf   func(format string, args ...any)
}

// New opens the database and prepares the server.
func New(cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = DefaultAddr
	}
	if cfg.DBPath == "" {
		cfg.DBPath = DefaultDBPath
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = DefaultHistory
	}
	if cfg.MaxPayload <= 0 {
		cfg.MaxPayload = DefaultMaxPayload
	}
	st, err := store.Open(cfg.DBPath, cfg.HistoryLimit)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:    cfg,
		store:  st,
		hub:    newHub(),
		events: newBroker(),
		logf:   func(string, ...any) {},
	}
	if !cfg.Quiet {
		s.logf = func(format string, args ...any) {
			logx.Printf("[server] "+format, args...)
		}
	}
	return s, nil
}

// Close releases resources and disconnects all clients.
func (s *Server) Close() {
	s.hub.closeAll("server shutting down")
	_ = s.store.Close()
}

func (s *Server) maxPayload() int { return s.cfg.MaxPayload }

// Handler returns the HTTP handler serving the web UI, REST API and the
// WebSocket endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("GET /api/items/{id}/content", s.handleContent)
	mux.HandleFunc("POST /api/items/{id}/push", s.handlePush)
	mux.HandleFunc("POST /api/history/clear", s.handleClear)
	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	// Brand icon: served from the embedded assets package so the favicon and
	// touch icons work even when only the server binary is deployed.
	mux.HandleFunc("GET /favicon.svg", serveAsset(assets.IconSVG, "image/svg+xml"))
	mux.HandleFunc("GET /icon.svg", serveAsset(assets.IconSVG, "image/svg+xml"))
	mux.HandleFunc("GET /favicon.ico", serveAsset(assets.IconICO, "image/x-icon"))
	mux.HandleFunc("GET /apple-touch-icon.png", serveAsset(assets.AppleTouchIcon, "image/png"))
	mux.HandleFunc("GET /icon-192.png", serveAsset(assets.Icon192, "image/png"))
	mux.HandleFunc("GET /icon-512.png", serveAsset(assets.Icon512, "image/png"))

	sub, err := fs.Sub(webuiFS, "webui")
	if err != nil {
		panic(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

// serveAsset returns a handler for a static embedded asset. The icons are
// immutable per build, so they get a long client-side cache.
func serveAsset(data []byte, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(data)
	}
}

// ListenAndServe serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		host := s.cfg.Addr
		if strings.HasPrefix(host, ":") {
			host = "localhost" + host
		}
		s.logf("listening on http://%s (clients connect to ws://%s/ws)", host, host)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case <-ctx.Done():
		s.Close()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		s.Close()
		return err
	}
}

// --- WebSocket session ------------------------------------------------------

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	// The library default read limit is 32 KiB; raise it so large clipboard
	// payloads (up to -max-payload) fit in a single WebSocket message.
	conn.SetReadLimit(int64(s.maxPayload()) + protocol.FrameOverhead)
	c := newClient(s.hub, s, conn)
	s.serveClient(c)
}

func (s *Server) serveClient(c *client) {
	defer func() {
		c.cancel()
		_ = c.conn.Close(websocket.StatusNormalClosure, "bye")
		if c.registered && c.hub.remove(c) {
			s.logf("client %s (%s) left (%d online)", c.name, c.id, c.hub.size())
			c.hub.broadcastOthers(protocol.Msg{
				Kind: protocol.KindLeft, ClientID: c.id, ClientName: c.name,
			}.MustFrame(s.maxPayload()), c)
			s.publishStatus()
		}
	}()
	go c.writeLoop()

	helloAt := time.Now()
	for {
		if !c.registered && time.Since(helloAt) > helloDeadline {
			c.kick("no hello received")
			return
		}
		readCtx, cancel := context.WithTimeout(c.ctx, readDeadline)
		typ, data, err := c.conn.Read(readCtx)
		cancel()
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		m, err := protocol.Parse(data, s.maxPayload())
		if err != nil {
			s.logf("client %s: bad frame: %v", c.id, err)
			c.kick("protocol violation")
			return
		}
		if !s.dispatch(c, m) {
			return
		}
	}
}

// dispatch handles one message from a client. It returns false when the
// session should be closed.
func (s *Server) dispatch(c *client, m *protocol.Msg) bool {
	switch m.Kind {
	case protocol.KindHello:
		if c.registered {
			return true
		}
		c.id = m.ClientID
		if c.id == "" {
			c.id = "unknown"
		}
		c.name = m.ClientName
		if c.name == "" {
			c.name = c.id
		}
		c.registered = true
		c.hub.add(c)
		s.logf("client %s (%s) joined (%d online)", c.name, c.id, c.hub.size())
		if err := c.writeFrame(protocol.Msg{
			Kind: protocol.KindWelcome, ClientID: c.id, ClientName: c.name,
			Count: c.hub.size(),
		}); err != nil {
			c.kick("welcome write failed")
			return false
		}
		c.hub.broadcastOthers(protocol.Msg{
			Kind: protocol.KindJoined, ClientID: c.id, ClientName: c.name,
		}.MustFrame(s.maxPayload()), c)
		s.publishStatus()
		return true

	case protocol.KindClip:
		if !c.registered {
			return true
		}
		kind, ok := mimeKind(m.MIME)
		if !ok || len(m.Payload) == 0 {
			return true
		}
		// The server is authoritative about the origin. store.Add filters
		// out content identical to the most recent history entry; when it
		// reports stored=false the copy is ignored entirely (no history
		// row, no broadcast, no web event).
		m.ClientID = c.id
		m.ClientName = c.name
		entryID, stored, err := s.store.Add(kind, m.MIME, c.id, c.name, m.Payload)
		if err != nil {
			s.logf("store: %v", err)
			return true
		}
		if !stored {
			s.logf("[clip] %s repeated latest %s (%d B), ignored", c.name, kind, len(m.Payload))
			return true
		}
		s.logf("[clip] %s shared %s (%d B, history #%d)", c.name, kind, len(m.Payload), entryID)
		s.events.publish("clip", itemEntry{
			ID:         entryID,
			Kind:       kind,
			MIME:       m.MIME,
			Size:       len(m.Payload),
			SourceID:   c.id,
			SourceName: c.name,
			Time:       time.Now().UTC().Format(time.RFC3339),
			Preview:    textPreview(kind, m.Payload),
		})
		c.hub.broadcastOthers(m.MustFrame(s.maxPayload()), c)
		return true

	case protocol.KindPing:
		return true

	default:
		return true
	}
}

func mimeKind(mime string) (string, bool) {
	switch mime {
	case protocol.MIMEText:
		return store.KindText, true
	case protocol.MIMEImage:
		return store.KindImage, true
	}
	return "", false
}

// --- REST API ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// itemEntry is the JSON representation of one history item shared by the
// history API and live "clip" events, so the web UI can render both the same
// way.
type itemEntry struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	MIME       string `json:"mime"`
	Size       int    `json:"size"`
	SourceID   string `json:"sourceId"`
	SourceName string `json:"sourceName"`
	Time       string `json:"time"`
	Preview    string `json:"preview,omitempty"`
}

// textPreview mirrors the first-500-bytes preview the store returns for
// history rows; live clip events need the same truncation.
func textPreview(kind string, payload []byte) string {
	if kind != store.KindText || len(payload) == 0 {
		return ""
	}
	const maxPreview = 500
	if len(payload) > maxPreview {
		payload = payload[:maxPreview]
	}
	return strings.ToValidUTF8(string(payload), "")
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	items, err := s.store.Recent(limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]itemEntry, 0, len(items))
	for _, it := range items {
		out = append(out, itemEntry{
			ID:         it.ID,
			Kind:       it.Kind,
			MIME:       it.MIME,
			Size:       it.Size,
			SourceID:   it.SourceID,
			SourceName: it.SourceName,
			Time:       it.CreatedAt.Format(time.RFC3339),
			Preview:    it.TextPreview,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleContent(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	e, payload, err := s.store.Content(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "entry not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", e.MIME)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

// handlePush re-broadcasts a stored history entry to every online client.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	e, payload, err := s.store.Content(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "entry not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	msg := protocol.NewClip(e.MIME, protocol.OriginWeb, protocol.OriginName, payload)
	n := s.hub.broadcastAll(msg.MustFrame(s.maxPayload()))
	s.logf("[push] entry #%d (%s, %d B) pushed to %d client(s)", id, e.Kind, len(payload), n)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "clients": n})
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Clear(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.logf("[history] cleared")
	s.events.publish("cleared", map[string]any{"ok": true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
