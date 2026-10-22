package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/micookie2/share-clip/internal/clipboard"
	"github.com/micookie2/share-clip/internal/protocol"
)

func TestContentFromMessageHTML(t *testing.T) {
	m := protocol.NewClipHTML("h", "n", []byte("<b>hi</b>"), []byte("hi"))
	c, ok := contentFromMessage(m)
	if !ok {
		t.Fatal("contentFromMessage rejected a rich clip")
	}
	if c.Kind != clipboard.KindText || c.Text != "hi" || c.HTML != "<b>hi</b>" {
		t.Fatalf("got %+v", c)
	}
}

func TestContentFromMessagePlainAndEmptyHTML(t *testing.T) {
	if c, ok := contentFromMessage(protocol.NewClip(protocol.MIMEText, "h", "n", []byte("plain"))); !ok || c.HTML != "" || c.Text != "plain" {
		t.Fatalf("plain text mismatch: %+v ok=%v", c, ok)
	}
	if _, ok := contentFromMessage(&protocol.Msg{Kind: protocol.KindClip, MIME: protocol.MIMEHTML}); ok {
		t.Fatal("empty HTML payload must be rejected")
	}
	if _, ok := contentFromMessage(protocol.NewClip(protocol.MIMEText, "h", "n", []byte("file:///etc/hosts"))); ok {
		t.Fatal("file artifact must be rejected")
	}
}

func TestIsOwnEcho(t *testing.T) {
	rich := clipboard.Content{Kind: clipboard.KindText, Text: "hi", HTML: "<b>hi</b>"}
	a := &Agent{}
	if a.isOwnEcho(rich) {
		t.Error("empty history must not report an echo")
	}
	a.rememberSent(rich)

	if !a.isOwnEcho(rich) {
		t.Error("identical rich copy not detected as echo")
	}
	if !a.isOwnEcho(clipboard.Content{Kind: clipboard.KindText, Text: "hi"}) {
		t.Error("rich copy downgraded to plain text not detected as echo")
	}
	if a.isOwnEcho(clipboard.Content{Kind: clipboard.KindText, Text: "other"}) {
		t.Error("unrelated text treated as echo")
	}
	if a.isOwnEcho(clipboard.Content{Kind: clipboard.KindText, Text: "hi", HTML: "<i>hi</i>"}) {
		t.Error("different rich content treated as echo")
	}

	// Sends older than the window are forgotten.
	b := &Agent{}
	b.rememberSent(rich)
	b.sentMu.Lock()
	b.sentLog[0].at = time.Now().Add(-time.Minute)
	b.sentMu.Unlock()
	if b.isOwnEcho(rich) {
		t.Error("expired send treated as echo")
	}
}

func TestIsOwnEchoImageReencoded(t *testing.T) {
	img := testPNGPicture()
	orig := encodeTestPNG(t, img, png.BestSpeed)
	// Same picture, different bytes: this is what a Windows CF_DIB round trip
	// (or a web push of a re-encoded copy) looks like on the wire.
	reencoded := encodeTestPNG(t, img, png.BestCompression)
	if bytes.Equal(orig, reencoded) {
		t.Fatal("test setup: both encodings produced identical bytes")
	}

	a := &Agent{}
	a.rememberSent(clipboard.Content{Kind: clipboard.KindImage, PNG: orig})

	if !a.isOwnEcho(clipboard.Content{Kind: clipboard.KindImage, PNG: orig}) {
		t.Error("identical image bytes not detected as echo")
	}
	if !a.isOwnEcho(clipboard.Content{Kind: clipboard.KindImage, PNG: reencoded}) {
		t.Error("re-encoded copy of the same picture not detected as echo")
	}

	other := testPNGPicture()
	other.SetNRGBA(2, 3, color.NRGBA{R: 7, G: 8, B: 9, A: 255})
	if a.isOwnEcho(clipboard.Content{Kind: clipboard.KindImage, PNG: encodeTestPNG(t, other, png.BestSpeed)}) {
		t.Error("a different picture was treated as an echo")
	}
	// Text history never makes an image look like an echo.
	b := &Agent{}
	b.rememberSent(clipboard.Content{Kind: clipboard.KindText, Text: "hello"})
	if b.isOwnEcho(clipboard.Content{Kind: clipboard.KindImage, PNG: orig}) {
		t.Error("image treated as echo of a text send")
	}
}

func TestIsOwnEchoImageExpires(t *testing.T) {
	img := testPNGPicture()
	orig := encodeTestPNG(t, img, png.BestSpeed)
	reencoded := encodeTestPNG(t, img, png.BestCompression)
	a := &Agent{}
	a.rememberSent(clipboard.Content{Kind: clipboard.KindImage, PNG: orig})
	a.sentMu.Lock()
	a.sentLog[0].at = time.Now().Add(-time.Minute)
	a.sentMu.Unlock()
	if a.isOwnEcho(clipboard.Content{Kind: clipboard.KindImage, PNG: reencoded}) {
		t.Error("picture send older than the echo window treated as echo")
	}
}

func TestIsOwnEchoUndecodableImage(t *testing.T) {
	// A send that cannot be fingerprinted still matches its own exact bytes,
	// and never matches unrelated bytes.
	a := &Agent{}
	a.rememberSent(clipboard.Content{Kind: clipboard.KindImage, PNG: []byte("not a png")})
	if !a.isOwnEcho(clipboard.Content{Kind: clipboard.KindImage, PNG: []byte("not a png")}) {
		t.Error("exact undecodable image bytes not detected as echo")
	}
	if a.isOwnEcho(clipboard.Content{Kind: clipboard.KindImage, PNG: []byte("different garbage")}) {
		t.Error("unrelated undecodable image treated as echo")
	}
}

func TestIsOversizedClip(t *testing.T) {
	if isOversizedClip(nil) {
		t.Error("nil error treated as oversized clip")
	}
	if !isOversizedClip(protocol.ErrPayloadTooLarge) {
		t.Error("ErrPayloadTooLarge not recognized")
	}
	// writeMsg / Frame may wrap the sentinel; errors.Is must still see it.
	if !isOversizedClip(fmt.Errorf("frame: %w", protocol.ErrPayloadTooLarge)) {
		t.Error("wrapped ErrPayloadTooLarge not recognized")
	}
	if isOversizedClip(protocol.ErrHeaderTooLarge) {
		t.Error("ErrHeaderTooLarge treated as an oversized clip")
	}
	if isOversizedClip(errors.New("websocket closed")) {
		t.Error("unrelated connection error treated as an oversized clip")
	}
}

func TestWriteMsgPreservesPayloadTooLarge(t *testing.T) {
	a := &Agent{cfg: Config{MaxPayload: 8, Quiet: true}}
	m := protocol.NewClip(protocol.MIMEImage, "h", "n", make([]byte, 9))
	// writeMsg wraps errors from the caller's perspective; this call fails in
	// Frame before the connection is touched, so a nil conn is safe here and
	// keeps the test focused on the error type that writeLoop must classify.
	err := a.writeMsg(context.Background(), nil, m)
	if err == nil {
		t.Fatal("writeMsg accepted a payload above the limit")
	}
	if !errors.Is(err, protocol.ErrPayloadTooLarge) {
		t.Fatalf("writeMsg hid ErrPayloadTooLarge: %v", err)
	}
	if !isOversizedClip(err) {
		t.Fatal("writeLoop would treat the oversized clip as a fatal session error")
	}
}

// TestWriteLoopSkipsOversizedClip drives a real WebSocket session: an
// oversized clip must be skipped without killing the session, and the clip
// queued behind it must still reach the peer.
func TestWriteLoopSkipsOversizedClip(t *testing.T) {
	const maxPayload = 16
	received := make(chan []byte, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			typ, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if typ == websocket.MessageBinary {
				received <- append([]byte(nil), data...)
			}
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	a := &Agent{cfg: Config{MaxPayload: maxPayload, Quiet: true}, out: make(chan *protocol.Msg, 4)}
	errCh := make(chan error, 2)
	loopCtx, stopLoop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.writeLoop(loopCtx, conn, errCh)
	}()
	defer func() {
		stopLoop()
		<-done
	}()

	// 1) A clip whose frame exceeds maxPayload: writeMsg fails with
	// protocol.ErrPayloadTooLarge and must not end the session.
	a.out <- protocol.NewClip(protocol.MIMEImage, "h", "n", make([]byte, maxPayload+1))
	// 2) A normal clip queued behind it must still go out on the same session.
	want := []byte("still connected")
	a.out <- protocol.NewClip(protocol.MIMEText, "h", "n", want)

	select {
	case frame := <-received:
		m, err := protocol.Parse(frame, protocol.DefaultMaxPayload)
		if err != nil {
			t.Fatalf("peer received an unparsable frame: %v", err)
		}
		if string(m.Payload) != string(want) {
			t.Fatalf("peer received %q, want %q (the oversized clip was not skipped)", m.Payload, want)
		}
	case err := <-errCh:
		t.Fatalf("writeLoop ended the session on an oversized clip: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the clip behind the oversized one")
	}
	select {
	case err := <-errCh:
		t.Fatalf("writeLoop reported an error for the oversized clip: %v", err)
	default:
	}
}

// testPNGPicture returns a deterministic opaque picture.
func testPNGPicture() *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, 6, 6))
	for y := 0; y < m.Rect.Dy(); y++ {
		for x := 0; x < m.Rect.Dx(); x++ {
			i := y*m.Stride + x*4
			m.Pix[i+0] = uint8(x * 37)
			m.Pix[i+1] = uint8(y * 41)
			m.Pix[i+2] = uint8((x*3 + y*5) * 11)
			m.Pix[i+3] = 255
		}
	}
	return m
}

// encodeTestPNG encodes m as a PNG with the given compression level.
func encodeTestPNG(t *testing.T, m image.Image, level png.CompressionLevel) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: level}
	if err := enc.Encode(&buf, m); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func TestStatusHooks(t *testing.T) {
	var (
		addr    string
		gotErr  error
		retryIn time.Duration
	)
	a := &Agent{cfg: Config{
		ServerAddr:     "10.0.0.1:9000",
		Quiet:          true,
		OnConnected:    func(s string) { addr = s },
		OnDisconnected: func(err error, r time.Duration) { gotErr, retryIn = err, r },
	}}
	a.notifyConnected()
	if addr != "10.0.0.1:9000" {
		t.Errorf("OnConnected 收到 %q", addr)
	}
	a.notifyDisconnected(errors.New("boom"), 4*time.Second)
	if gotErr == nil || gotErr.Error() != "boom" || retryIn != 4*time.Second {
		t.Errorf("OnDisconnected 收到 err=%v retryIn=%v", gotErr, retryIn)
	}

	// 未注册回调时不能 panic，断了连接也不能把状态展示的 panic 传出去。
	(&Agent{}).notifyConnected()
	(&Agent{}).notifyDisconnected(errors.New("x"), time.Second)
	(&Agent{cfg: Config{Quiet: true, OnConnected: func(string) { panic("ui bug") }}}).notifyConnected()
	(&Agent{cfg: Config{Quiet: true, OnDisconnected: func(error, time.Duration) { panic("ui bug") }}}).
		notifyDisconnected(errors.New("x"), time.Second)
}
