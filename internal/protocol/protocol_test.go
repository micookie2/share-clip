package protocol

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/micookie2/share-clip/internal/logx"
)

func TestFrameRoundTripText(t *testing.T) {
	m := &Msg{
		Kind:       KindClip,
		MIME:       MIMEText,
		ClientID:   "host-1",
		ClientName: "alice",
		Payload:    []byte("你好, clipboard!\nsecond line"),
	}
	frame, err := m.Frame(0)
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	got, err := Parse(frame, 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != m.Kind || got.MIME != m.MIME || got.ClientID != m.ClientID ||
		got.ClientName != m.ClientName || !bytes.Equal(got.Payload, m.Payload) || got.Size != len(m.Payload) {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, m)
	}
}

func TestFrameRoundTripBinary(t *testing.T) {
	payload := []byte{0x89, 'P', 'N', 'G', 0x00, 0x01, 0xff, 0xfe}
	m := NewClip(MIMEImage, "x", "bob", payload)
	frame, err := m.Frame(0)
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	got, err := Parse(frame, 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Fatalf("payload mismatch")
	}
}

func TestControlFrameNoPayload(t *testing.T) {
	m := &Msg{Kind: KindJoined, ClientID: "c"}
	frame, err := m.Frame(0)
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	got, err := Parse(frame, 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != KindJoined || len(got.Payload) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestFrameRoundTripHTMLWithPlainText(t *testing.T) {
	m := NewClipHTML("host-1", "alice", []byte("<b>hello</b>"), []byte("hello"))
	frame, err := m.Frame(0)
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	got, err := Parse(frame, 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != KindClip || got.MIME != MIMEHTML || got.MIME2 != MIMEText {
		t.Fatalf("mimes mismatch: %+v", got)
	}
	if !bytes.Equal(got.Payload, []byte("<b>hello</b>")) || !bytes.Equal(got.Payload2, []byte("hello")) {
		t.Fatalf("payloads mismatch: %q %q", got.Payload, got.Payload2)
	}
	if got.Size != len(m.Payload) {
		t.Fatalf("Size = %d, want %d", got.Size, len(m.Payload))
	}
	// A single-payload frame must remain byte-identical to the old format.
	plain := NewClip(MIMEText, "h", "n", []byte("hi"))
	plainFrame, _ := plain.Frame(0)
	plainParsed, err := Parse(plainFrame, 0)
	if err != nil {
		t.Fatalf("Parse plain: %v", err)
	}
	if plainParsed.MIME2 != "" || !bytes.Equal(plainParsed.Payload, []byte("hi")) {
		t.Fatalf("plain frame regressed: %+v", plainParsed)
	}
}

func TestHTMLSummary(t *testing.T) {
	m := NewClipHTML("host-1", "alice", []byte("<b>hello</b>"), []byte("hello"))
	got := m.Summary()
	if !strings.Contains(got, "text/html+text/plain") || !strings.Contains(got, "hello") {
		t.Fatalf("rich summary = %q", got)
	}
}

func TestOversizedPayloadRejected(t *testing.T) {
	m := &Msg{Kind: KindClip, MIME: MIMEText, Payload: bytes.Repeat([]byte{'a'}, 4096)}
	frame, err := m.Frame(4095)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("Frame: want ErrPayloadTooLarge, got %v", err)
	}
	if frame != nil {
		t.Fatalf("expected nil frame")
	}
	// Writing succeeds under a larger cap, but parsing under a small cap fails.
	frame, err = m.Frame(0)
	if err != nil {
		t.Fatalf("Frame under big cap: %v", err)
	}
	if _, err := Parse(frame, 1024); !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("Parse: want ErrPayloadTooLarge, got %v", err)
	}
}

func TestTruncatedFrame(t *testing.T) {
	m := &Msg{Kind: KindClip, MIME: MIMEText, Payload: []byte("hello world")}
	frame, err := m.Frame(0)
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	for _, cut := range []int{0, 3, 7, len(frame) - 1, len(frame) - 5} {
		if _, err := Parse(frame[:cut], 0); !errors.Is(err, ErrTruncated) {
			t.Fatalf("Parse cut=%d: want ErrTruncated, got %v", cut, err)
		}
	}
}

func TestEmptyInput(t *testing.T) {
	if _, err := Parse(nil, 0); !errors.Is(err, ErrTruncated) {
		t.Fatalf("want ErrTruncated, got %v", err)
	}
}

func TestGarbageHeader(t *testing.T) {
	frame := make([]byte, 8+16)
	// header length says 16 but content is not JSON
	if _, err := Parse(frame, 0); err == nil {
		t.Fatalf("expected parse error")
	}
}

func TestSummary(t *testing.T) {
	cases := []struct {
		name string
		msg  *Msg
		want string
	}{
		{"hello", &Msg{Kind: KindHello, ClientID: "host-1", ClientName: "alice"},
			"hello alice (host-1)"},
		{"welcome", &Msg{Kind: KindWelcome, Count: 2}, "welcome count=2"},
		{"text clip", NewClip(MIMEText, "host-1", "alice", []byte("hello")),
			"clip text/plain 5 B from alice (host-1): hello"},
		{"image clip", NewClip(MIMEImage, "host-2", "bob", []byte{1, 2, 3}),
			"clip image/png 3 B from bob (host-2)"},
		{"web clip", NewClip(MIMEText, OriginWeb, OriginName, []byte("pushed")),
			"clip text/plain 6 B from web: pushed"},
		{"joined", &Msg{Kind: KindJoined, ClientID: "host-1", ClientName: "alice"},
			"joined alice (host-1)"},
		{"left id only", &Msg{Kind: KindLeft, ClientID: "host-1"}, "left host-1"},
		{"unknown", &Msg{Kind: "surprise"}, "unknown kind=surprise"},
	}
	for _, c := range cases {
		if got := c.msg.Summary(); got != c.want {
			t.Errorf("%s: Summary() = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestSummaryStaysOneLineOnceLogged checks the contract the loggers rely on:
// Summary may embed raw clipboard bytes, but logx.SingleLine — the single
// place every record passes through — collapses them to one line.
func TestSummaryStaysOneLineOnceLogged(t *testing.T) {
	m := NewClip(MIMEText, "host-1", "alice", []byte("first\nsecond\r\nthird"))
	got := logx.SingleLine(m.Summary())
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("logged summary contains a line break: %q", got)
	}
	if !strings.Contains(got, `\n`) || !strings.Contains(got, `\r`) {
		t.Fatalf("newlines were dropped rather than escaped: %q", got)
	}
}

func TestSummaryTruncatesLongText(t *testing.T) {
	got := NewClip(MIMEText, "h", "n", []byte(strings.Repeat("好", 200))).Summary()
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("long text not truncated: %q", got)
	}
	// "clip text/plain 600 B from n (h): " prefix plus 60 runes and the ellipsis.
	if n := len([]rune(got)); n > 200 {
		t.Fatalf("preview not bounded: %d runes", n)
	}
}
