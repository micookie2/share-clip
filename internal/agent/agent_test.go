package agent

import (
	"errors"
	"testing"
	"time"

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
