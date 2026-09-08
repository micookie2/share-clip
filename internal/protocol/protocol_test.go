package protocol

import (
	"bytes"
	"errors"
	"testing"
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
	m := &Msg{Kind: KindPing, ClientID: "c"}
	frame, err := m.Frame(0)
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	got, err := Parse(frame, 0)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Kind != KindPing || len(got.Payload) != 0 {
		t.Fatalf("got %+v", got)
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
