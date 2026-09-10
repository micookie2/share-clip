package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/micookie2/share-clip/internal/buildinfo"
	"github.com/micookie2/share-clip/internal/protocol"
	"github.com/micookie2/share-clip/internal/server"
)

func startServer(t *testing.T) *server.Server {
	t.Helper()
	s, err := server.New(server.Config{
		Addr:         ":0",
		DBPath:       filepath.Join(t.TempDir(), "history.db"),
		HistoryLimit: 500,
		MaxPayload:   32 << 20,
		Quiet:        true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func wsURL(base string) string { return "ws" + base[len("http"):] + "/ws" }

func dialClient(t *testing.T, base, id, name string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL(base), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// Mirror the agent: raise the 32 KiB library default read limit.
	conn.SetReadLimit(int64(protocol.DefaultMaxPayload) + protocol.FrameOverhead)
	t.Cleanup(func() { conn.CloseNow() })

	send(t, conn, &protocol.Msg{Kind: protocol.KindHello, ClientID: id, ClientName: name})
	// Wait for the welcome message.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m := readMsg(t, conn, 2*time.Second)
		if m != nil && m.Kind == protocol.KindWelcome {
			return conn
		}
	}
	t.Fatal("no welcome received")
	return nil
}

func send(t *testing.T, conn *websocket.Conn, m *protocol.Msg) {
	t.Helper()
	frame, err := m.Frame(protocol.DefaultMaxPayload)
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageBinary, frame); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

// readMsg reads until the next clip/control message or the timeout. Returns
// nil when no message arrived in time. Presence notices are skipped. (Server
// keepalive now runs on WebSocket control pings, which never surface here.)
func readMsg(t *testing.T, conn *websocket.Conn, timeout time.Duration) *protocol.Msg {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		_, data, err := conn.Read(ctx)
		cancel()
		if err != nil {
			return nil
		}
		m, perr := protocol.Parse(data, protocol.DefaultMaxPayload)
		if perr != nil {
			continue
		}
		switch m.Kind {
		case protocol.KindJoined, protocol.KindLeft:
			continue
		}
		return m
	}
	return nil
}

func TestBroadcastClipToOthers(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	c2 := dialClient(t, ts.URL, "id-b", "host-b")

	send(t, c1, protocol.NewClip(protocol.MIMEText, "id-a", "host-a", []byte("hello from a")))

	got := readMsg(t, c2, 5*time.Second)
	if got == nil || got.Kind != protocol.KindClip {
		t.Fatalf("c2 did not receive clip, got %+v", got)
	}
	if string(got.Payload) != "hello from a" || got.ClientName != "host-a" || got.MIME != protocol.MIMEText {
		t.Fatalf("c2 clip mismatch: %+v", got)
	}

	// The sender must not receive its own broadcast.
	if m := readMsg(t, c1, 400*time.Millisecond); m != nil {
		t.Fatalf("c1 received its own clip: %+v", m)
	}
}

func TestBroadcastImageToOthers(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	c2 := dialClient(t, ts.URL, "id-b", "host-b")

	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 1, 2, 3}
	send(t, c1, protocol.NewClip(protocol.MIMEImage, "id-a", "host-a", png))

	got := readMsg(t, c2, 5*time.Second)
	if got == nil || got.MIME != protocol.MIMEImage || !bytes.Equal(got.Payload, png) {
		t.Fatalf("c2 image clip mismatch: %+v", got)
	}
}

// TestLargeClipBeyondLibraryReadLimit guards against regressions of the
// WebSocket library's default 32 KiB per-message read limit: screenshots are
// routinely bigger than that.
func TestLargeClipBeyondLibraryReadLimit(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	c2 := dialClient(t, ts.URL, "id-b", "host-b")

	big := bytes.Repeat([]byte("0123456789abcdef"), 20_000) // 320 KB
	send(t, c1, protocol.NewClip(protocol.MIMEText, "id-a", "host-a", big))

	got := readMsg(t, c2, 5*time.Second)
	if got == nil || got.Kind != protocol.KindClip || !bytes.Equal(got.Payload, big) {
		t.Fatalf("large clip not delivered: %+v", got)
	}

	// Both connections must still be usable afterwards (no close frame).
	send(t, c2, protocol.NewClip(protocol.MIMEText, "id-b", "host-b", []byte("still alive")))
	if m := readMsg(t, c1, 5*time.Second); m == nil || string(m.Payload) != "still alive" {
		t.Fatalf("connection broken after large clip: %+v", m)
	}
}

// TestDuplicateClipIgnored covers the duplicate filter: a clip identical to
// the most recent history entry is ignored — not stored, not broadcast —
// while an older value stores again once something else interrupts the
// streak. NOTE: a timed-out websocket Read closes the conn in this library,
// so the negative (no-broadcast) check must be the last read on c2.
func TestDuplicateClipIgnored(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	c2 := dialClient(t, ts.URL, "id-b", "host-b")

	clip := func(text string) {
		t.Helper()
		send(t, c1, protocol.NewClip(protocol.MIMEText, "id-a", "host-a", []byte(text)))
	}

	clip("first")
	if m := readMsg(t, c2, 5*time.Second); m == nil || string(m.Payload) != "first" {
		t.Fatalf("c2 missed initial clip: %+v", m)
	}
	clip("second")
	if m := readMsg(t, c2, 5*time.Second); m == nil || string(m.Payload) != "second" {
		t.Fatalf("c2 missed second clip: %+v", m)
	}

	// Exact repeat of the latest entry: nothing must be broadcast.
	clip("second")
	if m := readMsg(t, c2, 400*time.Millisecond); m != nil {
		t.Fatalf("duplicate clip was broadcast: %+v", m)
	} // c2's conn is now closed by the timed-out read; use a fresh receiver.

	// Streak broken by the (ignored) repeat attempt? No — the latest stored
	// entry is still "second", so repeating "first" (an older value) stores.
	c3 := dialClient(t, ts.URL, "id-c", "host-c")
	clip("first")
	if m := readMsg(t, c3, 5*time.Second); m == nil || string(m.Payload) != "first" {
		t.Fatalf("c3 missed resumed clip: %+v", m)
	}

	hist, err := http.Get(ts.URL + "/api/history?limit=10&offset=0")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer hist.Body.Close()
	var h struct {
		Items []struct {
			Preview string `json:"preview"`
		} `json:"items"`
	}
	if err := json.NewDecoder(hist.Body).Decode(&h); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	// first, second, first — the ignored repeat left no row.
	if len(h.Items) != 3 {
		t.Fatalf("history = %+v, want 3 entries", h.Items)
	}
}

// TestReencodedImageDuplicateNotRebroadcast covers the image duplicate filter
// end to end: an image whose *bytes differ* but which decodes to the same
// picture as the latest history entry (what a receiver echoes after a Windows
// CF_DIB round-trip re-encodes it) must be ignored — not stored, not
// broadcast — so clients never receive the same picture twice.
func TestReencodedImageDuplicateNotRebroadcast(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	c2 := dialClient(t, ts.URL, "id-b", "host-b")

	orig := testPNG(t, 0)
	reencoded := canonicalPNG(t, orig)
	if bytes.Equal(orig, reencoded) {
		t.Fatal("test PNGs must differ byte-wise to model a re-encoding")
	}

	send(t, c1, protocol.NewClip(protocol.MIMEImage, "id-a", "host-a", orig))
	if m := readMsg(t, c2, 5*time.Second); m == nil || m.Kind != protocol.KindClip ||
		!bytes.Equal(m.Payload, orig) {
		t.Fatalf("c2 missed original image clip: %+v", m)
	}

	// Re-encoding of the same picture: nothing must be broadcast.
	send(t, c1, protocol.NewClip(protocol.MIMEImage, "id-a", "host-a", reencoded))
	if m := readMsg(t, c2, 400*time.Millisecond); m != nil {
		t.Fatalf("re-encoded duplicate image was broadcast: %+v", m)
	}

	// The ignored echo left no history row.
	hist, err := http.Get(ts.URL + "/api/history?limit=10&offset=0")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer hist.Body.Close()
	var h struct {
		Items []struct {
			Kind string `json:"kind"`
		} `json:"items"`
	}
	if err := json.NewDecoder(hist.Body).Decode(&h); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(h.Items) != 1 || h.Items[0].Kind != "image" {
		t.Fatalf("history = %+v, want a single image entry", h.Items)
	}
}

// testPNG renders a small paletted picture; different shift values produce
// different pixel patterns.
func testPNG(t *testing.T, shift int) []byte {
	t.Helper()
	pal := color.Palette{
		color.RGBA{R: 255, A: 255},
		color.RGBA{G: 255, A: 255},
		color.RGBA{B: 255, A: 255},
		color.RGBA{R: 255, G: 255, A: 255},
	}
	pm := image.NewPaletted(image.Rect(0, 0, 4, 3), pal)
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			pm.SetColorIndex(x, y, uint8((x+y+shift)%4))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, pm); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// canonicalPNG re-encodes a PNG as an opaque RGBA image, modelling how the
// same picture comes back from a Windows CF_DIB round-trip.
func canonicalPNG(t *testing.T, data []byte) []byte {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	rgba := image.NewRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			rgba.Set(x, y, img.At(x, y))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		t.Fatalf("encode canonical: %v", err)
	}
	return buf.Bytes()
}

func TestHistoryWebAndPush(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	c2 := dialClient(t, ts.URL, "id-b", "host-b")

	text := "pushed via web later"
	send(t, c1, protocol.NewClip(protocol.MIMEText, "id-a", "host-a", []byte(text)))
	if m := readMsg(t, c2, 5*time.Second); m == nil || string(m.Payload) != text {
		t.Fatalf("c2 did not receive initial clip")
	}

	// Web UI serves the page.
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(page, []byte("share-clip")) {
		t.Fatalf("web page broken: %d", resp.StatusCode)
	}

	// History API lists the entry.
	resp, err = http.Get(ts.URL + "/api/history?limit=10&offset=0")
	if err != nil {
		t.Fatalf("GET history: %v", err)
	}
	defer resp.Body.Close()
	var hist struct {
		Items []struct {
			ID         int64  `json:"id"`
			Kind       string `json:"kind"`
			SourceName string `json:"sourceName"`
			Preview    string `json:"preview"`
		} `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&hist); err != nil {
		t.Fatalf("decode history: %v", err)
	}
	if len(hist.Items) != 1 || hist.Items[0].Kind != "text" ||
		hist.Items[0].SourceName != "host-a" || hist.Items[0].Preview != text {
		t.Fatalf("history mismatch: %+v", hist.Items)
	}
	id := hist.Items[0].ID

	// Content endpoint returns the raw text.
	resp, err = http.Get(fmt.Sprintf("%s/api/items/%d/content", ts.URL, id))
	if err != nil {
		t.Fatalf("GET content: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != text {
		t.Fatalf("content mismatch: %q", body)
	}

	// Push from the web reaches both online clients.
	resp, err = http.Post(fmt.Sprintf("%s/api/items/%d/push", ts.URL, id),
		"application/json", nil)
	if err != nil {
		t.Fatalf("POST push: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("push status %d", resp.StatusCode)
	}

	for _, conn := range []*websocket.Conn{c1, c2} {
		got := readMsg(t, conn, 5*time.Second)
		if got == nil || got.Kind != protocol.KindClip || got.ClientID != protocol.OriginWeb {
			t.Fatalf("push not received: %+v", got)
		}
		if string(got.Payload) != text {
			t.Fatalf("push payload mismatch")
		}
	}
}

// TestHeartbeatPing covers the WebSocket control-frame heartbeat: while a
// registered session idles, its pings must be answered with pongs and the
// session must still deliver clips afterwards. Keepalive frames never appear
// as protocol messages, so this exercises the raw Conn.
func TestHeartbeatPing(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	c1 := dialClient(t, ts.URL, "id-a", "host-a")
	c2 := dialClient(t, ts.URL, "id-b", "host-b")

	// Ping needs a concurrent reader on c2 to observe the pong; that reader
	// also drains broadcasts and reports the next clip.
	clipCh := make(chan *protocol.Msg, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, data, err := c2.Read(ctx)
			cancel()
			if err != nil {
				return
			}
			if m, perr := protocol.Parse(data, protocol.DefaultMaxPayload); perr == nil && m.Kind == protocol.KindClip {
				select {
				case clipCh <- m:
				default:
				}
			}
		}
	}()
	defer func() {
		c2.CloseNow()
		<-readDone
	}()

	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := c2.Ping(ctx)
		cancel()
		if err != nil {
			t.Fatalf("ping %d: %v", i+1, err)
		}
		time.Sleep(300 * time.Millisecond)
	}

	// The session must still be fully usable after the heartbeat round trips.
	send(t, c1, protocol.NewClip(protocol.MIMEText, "id-a", "host-a", []byte("after heartbeat")))
	select {
	case m := <-clipCh:
		if string(m.Payload) != "after heartbeat" {
			t.Fatalf("clip after heartbeat mismatch: %q", m.Payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("clip did not arrive after heartbeats; session dropped?")
	}
}

func TestHistoryClearAnd404(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	resp, err := http.Post(ts.URL+"/api/history/clear", "application/json", nil)
	if err != nil {
		t.Fatalf("POST clear: %v", err)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/api/items/42/content")
	if err != nil {
		t.Fatalf("GET missing content: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// The web UI advertises these icon URLs; they must stay reachable and keep
// their content type even though they are served outside the embedded webui.
func TestIconAssets(t *testing.T) {
	s := startServer(t)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	defer s.Close()

	cases := []struct {
		path, ctype, marker string
	}{
		{"/favicon.svg", "image/svg+xml", "<svg"},
		{"/icon.svg", "image/svg+xml", "<svg"},
		{"/favicon.ico", "image/x-icon", "\x00\x00\x01\x00"},
		{"/apple-touch-icon.png", "image/png", "\x89PNG"},
		{"/icon-192.png", "image/png", "\x89PNG"},
		{"/icon-512.png", "image/png", "\x89PNG"},
	}
	for _, c := range cases {
		resp, err := http.Get(ts.URL + c.path)
		if err != nil {
			t.Fatalf("GET %s: %v", c.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", c.path, resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Type"); got != c.ctype {
			t.Fatalf("GET %s: content type %q, want %q", c.path, got, c.ctype)
		}
		if !bytes.HasPrefix(body, []byte(c.marker)) {
			t.Fatalf("GET %s: body does not start with %q", c.path, c.marker)
		}
	}
}

// The web UI reads the stamped build metadata from /api/version and shows it
// in the header and the footer; a plain `go test` build knows nothing about
// ldflags, so only the shape is asserted here.
func TestVersionEndpoint(t *testing.T) {
	s := startServer(t)
	defer s.Close()
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/version")
	if err != nil {
		t.Fatalf("GET /api/version: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Version   string `json:"version"`
		Commit    string `json:"commit"`
		BuildTime string `json:"buildTime"`
		GoVersion string `json:"goVersion"`
		Platform  string `json:"platform"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := buildinfo.Get()
	if body.Version != want.Version || body.Commit != want.Commit || body.BuildTime != want.BuildTime {
		t.Errorf("/api/version = %+v, want the build's own metadata %+v", body, want)
	}
	if body.GoVersion == "" || !strings.Contains(body.Platform, "/") {
		t.Errorf("/api/version lacks runtime info: %+v", body)
	}

	page, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	html, _ := io.ReadAll(page.Body)
	page.Body.Close()
	for _, marker := range []string{`id="buildInfo"`, `id="verTag"`, `"/api/version"`} {
		if !strings.Contains(string(html), marker) {
			t.Errorf("index.html does not reference %s", marker)
		}
	}
}
