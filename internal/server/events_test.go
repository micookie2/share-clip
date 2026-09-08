package server_test

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/micookie2/share-clip/internal/protocol"
)

type sseEvent struct {
	event string
	data  string
}

// openSSE attaches to /api/events and returns a helper that blocks until the
// next event arrives, plus a closer that must run before the test server is
// closed (an attached stream keeps the httptest server open otherwise).
func openSSE(t *testing.T, base string) (next func() sseEvent, closeSSE func()) {
	t.Helper()
	resp, err := http.Get(base + "/api/events")
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("events status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("events content-type %q", ct)
	}

	br := bufio.NewReader(resp.Body)
	return func() sseEvent {
		t.Helper()
		var ev sseEvent
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("SSE read: %v", err)
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if ev.event != "" {
					return ev
				}
			case strings.HasPrefix(line, ":"): // keepalive comment
			case strings.HasPrefix(line, "event: "):
				ev.event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}, func() { resp.Body.Close() }
}

type statusPayload struct {
	Count   int             `json:"count"`
	Clients []clientPayload `json:"clients"`
}

type clientPayload struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func TestStatusAPI(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	get := func() statusPayload {
		t.Helper()
		resp, err := http.Get(ts.URL + "/api/status")
		if err != nil {
			t.Fatalf("GET /api/status: %v", err)
		}
		defer resp.Body.Close()
		var st statusPayload
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return st
	}

	if st := get(); st.Count != 0 || len(st.Clients) != 0 {
		t.Fatalf("expected empty status, got %+v", st)
	}

	dialClient(t, ts.URL, "id-a", "host-a")
	dialClient(t, ts.URL, "id-b", "host-b")

	st := get()
	if st.Count != 2 {
		t.Fatalf("expected 2 nodes, got %+v", st)
	}
	// Names are sorted for a stable display.
	if st.Clients[0].Name != "host-a" || st.Clients[1].Name != "host-b" {
		t.Fatalf("unexpected clients: %+v", st.Clients)
	}
}

func TestSESEventsLifecycle(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	next, closeSSE := openSSE(t, ts.URL)
	defer closeSSE() // registered last => runs before ts.Close()

	// Fresh attach always starts with a status snapshot.
	if ev := next(); ev.event != "status" {
		t.Fatalf("first event = %q, want status", ev.event)
	} else {
		var st statusPayload
		json.Unmarshal([]byte(ev.data), &st)
		if st.Count != 0 {
			t.Fatalf("initial status count = %d, want 0", st.Count)
		}
	}

	// A node joining produces a status event.
	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	if ev := next(); ev.event != "status" {
		t.Fatalf("join event = %q, want status", ev.event)
	} else {
		var st statusPayload
		json.Unmarshal([]byte(ev.data), &st)
		if st.Count != 1 || st.Clients[0].Name != "host-a" {
			t.Fatalf("join status mismatch: %s", ev.data)
		}
	}

	// A new clipboard item produces a clip event carrying its metadata.
	send(t, c1, protocol.NewClip(protocol.MIMEText, "id-a", "host-a", []byte("live push text")))
	if ev := next(); ev.event != "clip" {
		t.Fatalf("clip event = %q, want clip", ev.event)
	} else {
		var it struct {
			ID         int64  `json:"id"`
			Kind       string `json:"kind"`
			SourceName string `json:"sourceName"`
			Size       int    `json:"size"`
			Preview    string `json:"preview"`
			Time       string `json:"time"`
		}
		if err := json.Unmarshal([]byte(ev.data), &it); err != nil {
			t.Fatalf("decode clip: %v", err)
		}
		if it.ID == 0 || it.Kind != "text" || it.SourceName != "host-a" ||
			it.Size != len("live push text") || it.Preview != "live push text" || it.Time == "" {
			t.Fatalf("clip payload mismatch: %s", ev.data)
		}
	}

	// Clearing history is announced too.
	resp, err := http.Post(ts.URL+"/api/history/clear", "application/json", nil)
	if err != nil {
		t.Fatalf("POST clear: %v", err)
	}
	resp.Body.Close()
	if ev := next(); ev.event != "cleared" {
		t.Fatalf("clear event = %q, want cleared", ev.event)
	}

	// A node leaving produces a final status event.
	c1.CloseNow()
	if ev := next(); ev.event != "status" {
		t.Fatalf("leave event = %q, want status", ev.event)
	} else {
		var st statusPayload
		json.Unmarshal([]byte(ev.data), &st)
		if st.Count != 0 {
			t.Fatalf("after-leave status count = %d, want 0", st.Count)
		}
	}
}
