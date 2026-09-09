package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/micookie2/share-clip/internal/buildinfo"
)

// broker fans out server-side events (node presence changes, new history
// entries, history cleared) to web UI listeners attached through the
// /api/events Server-Sent Events endpoint.
type broker struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newBroker() *broker {
	return &broker{subs: make(map[chan []byte]struct{})}
}

func (b *broker) subscribe() chan []byte {
	ch := make(chan []byte, 32)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch
}

func (b *broker) unsubscribe(ch chan []byte) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// publish sends one SSE event to every listener. Listeners that cannot keep
// up simply miss the event; the web UI additionally reconciles periodically,
// so a dropped notification is not fatal.
func (b *broker) publish(event string, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	frame := []byte("event: " + event + "\ndata: " + string(payload) + "\n\n")
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- frame:
		default:
		}
	}
}

// statusData is the payload of "status" events and of GET /api/status.
type statusData struct {
	Count   int          `json:"count"`
	Clients []clientInfo `json:"clients"`
}

func (s *Server) currentStatus() statusData {
	infos := s.hub.list()
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].Name != infos[j].Name {
			return infos[i].Name < infos[j].Name
		}
		return infos[i].ID < infos[j].ID
	})
	return statusData{Count: len(infos), Clients: infos}
}

// publishStatus announces the current set of online nodes to the web UI.
func (s *Server) publishStatus() {
	s.events.publish("status", s.currentStatus())
}

// handleStatus returns a one-shot snapshot of the connected nodes.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentStatus())
}

// handleVersion reports the build metadata stamped into this binary at link
// time; the web UI shows it in the header and footer. It never changes while
// the process runs, so it is safe to cache.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, buildinfo.Get())
}

// handleEvents streams server events to the web UI via Server-Sent Events.
// The browser's EventSource reconnects automatically; every fresh attach
// starts with a status snapshot so the UI re-converges without a manual
// refresh.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	ch := s.events.subscribe()
	defer s.events.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	if init, err := json.Marshal(s.currentStatus()); err == nil {
		_, _ = w.Write([]byte("event: status\ndata: " + string(init) + "\n\n"))
		flusher.Flush()
	}

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-ch:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
