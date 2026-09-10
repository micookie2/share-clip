// Package protocol defines the wire format used between the share-clip
// clients and the server.
//
// Every WebSocket message is one protocol frame with the layout:
//
//	[4 bytes]  big-endian uint32: length of the JSON header
//	[N bytes]  JSON header (see Msg; Payload/Payload2 are excluded)
//	[4 bytes]  big-endian uint32: length of the binary payload
//	[M bytes]  raw payload bytes
//	[4 bytes]  big-endian uint32: length of the secondary payload (only
//	           present when the header carries a non-empty mime2)
//	[M2 bytes] raw secondary payload bytes (only when mime2 is present)
//
// The payload carries the actual clipboard content: UTF-8 text for
// text/plain messages, PNG encoded bytes for image/png messages, or HTML for
// text/html messages. A text/html message also carries a secondary
// text/plain payload so the receiving clipboard can offer both a plain-text
// and a rich-text rendition of the same copy. Control messages (hello,
// welcome, ...) simply have a zero length payload.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Message kinds.
const (
	KindHello   = "hello"   // client -> server, on connect
	KindWelcome = "welcome" // server -> client, right after hello
	KindClip    = "clip"    // client -> server and server -> clients
	KindJoined  = "joined"  // server -> clients, presence notification
	KindLeft    = "left"    // server -> clients, presence notification
)

// Origin identifiers used by the server when it originates a message itself.
const (
	OriginWeb  = "web"
	OriginName = "web"
)

// MIME types used for clipboard content.
const (
	MIMEText  = "text/plain"
	MIMEImage = "image/png"
	MIMEHTML  = "text/html"
)

// Limits applied to frames.
const (
	MaxHeaderSize     = 64 << 10 // 64 KiB is far more than enough for a header
	DefaultMaxPayload = 32 << 20 // 32 MiB, configurable per process
	// FrameOverhead is the largest possible non-payload part of a frame
	// (8 length bytes + header). WebSocket read limits must allow at least
	// MaxPayload + FrameOverhead per message.
	FrameOverhead = 8 + MaxHeaderSize
)

var (
	// ErrHeaderTooLarge is returned when a header exceeds MaxHeaderSize.
	ErrHeaderTooLarge = fmt.Errorf("protocol: header exceeds %d bytes", MaxHeaderSize)
	// ErrPayloadTooLarge is returned when a payload exceeds the configured limit.
	ErrPayloadTooLarge = errors.New("protocol: payload exceeds configured limit")
	// ErrTruncated is returned when the frame data ends before the declared lengths.
	ErrTruncated = errors.New("protocol: truncated frame")
)

// Msg is a protocol message. Payload is transported as raw bytes after the
// JSON header and is never serialized into the header itself. MIME2/Payload2
// carry the optional secondary rendition of a rich-text clip (text/plain for
// a text/html message); they are transported after the primary payload and
// are only present when MIME2 is non-empty.
type Msg struct {
	Kind       string `json:"kind"`
	MIME       string `json:"mime,omitempty"`       // text/plain | image/png | text/html
	MIME2      string `json:"mime2,omitempty"`      // optional secondary format, e.g. text/plain
	ClientID   string `json:"clientId,omitempty"`   // originator identity
	ClientName string `json:"clientName,omitempty"` // originator display name
	Count      int    `json:"count,omitempty"`      // used by welcome
	Size       int    `json:"size,omitempty"`       // primary payload length, informational
	Payload    []byte `json:"-"`
	Payload2   []byte `json:"-"`
}

// Frame returns the wire bytes for a message. Payloads larger than maxPayload
// are rejected.
func (m *Msg) Frame(maxPayload int) ([]byte, error) {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPayload
	}
	if len(m.Payload) > maxPayload || len(m.Payload2) > maxPayload {
		return nil, ErrPayloadTooLarge
	}
	header, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("protocol: marshal header: %w", err)
	}
	if len(header) > MaxHeaderSize {
		return nil, ErrHeaderTooLarge
	}
	hasSecond := m.MIME2 != ""
	n := 8 + len(header) + len(m.Payload)
	if hasSecond {
		n += 4 + len(m.Payload2)
	}
	out := make([]byte, n)
	binary.BigEndian.PutUint32(out[0:4], uint32(len(header)))
	copy(out[4:], header)
	off := 4 + len(header)
	binary.BigEndian.PutUint32(out[off:off+4], uint32(len(m.Payload)))
	off += 4
	copy(out[off:], m.Payload)
	off += len(m.Payload)
	if hasSecond {
		binary.BigEndian.PutUint32(out[off:off+4], uint32(len(m.Payload2)))
		off += 4
		copy(out[off:], m.Payload2)
	}
	return out, nil
}

// MustFrame is Frame for messages that are known to be valid.
func (m Msg) MustFrame(maxPayload int) []byte {
	frame, err := m.Frame(maxPayload)
	if err != nil {
		panic(fmt.Sprintf("protocol: MustFrame: %v", err))
	}
	return frame
}

// Parse parses one frame produced by Msg.Frame.
func Parse(data []byte, maxPayload int) (*Msg, error) {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPayload
	}
	if len(data) < 8 {
		return nil, ErrTruncated
	}
	hLen := int(binary.BigEndian.Uint32(data[0:4]))
	if hLen > MaxHeaderSize {
		return nil, ErrHeaderTooLarge
	}
	body := data[4:]
	if len(body) < hLen+4 {
		return nil, ErrTruncated
	}
	m := &Msg{}
	if err := json.Unmarshal(body[:hLen], m); err != nil {
		return nil, fmt.Errorf("protocol: parse header: %w", err)
	}
	off := hLen
	pLen := int(binary.BigEndian.Uint32(body[off : off+4]))
	off += 4
	if pLen > maxPayload {
		return nil, ErrPayloadTooLarge
	}
	if len(body) < off+pLen {
		return nil, ErrTruncated
	}
	if pLen > 0 {
		m.Payload = body[off : off+pLen]
	}
	off += pLen
	if m.MIME2 != "" {
		if len(body) < off+4 {
			return nil, ErrTruncated
		}
		p2Len := int(binary.BigEndian.Uint32(body[off : off+4]))
		off += 4
		if p2Len > maxPayload {
			return nil, ErrPayloadTooLarge
		}
		if len(body) < off+p2Len {
			return nil, ErrTruncated
		}
		if p2Len > 0 {
			m.Payload2 = body[off : off+p2Len]
		}
		off += p2Len
	}
	m.Size = pLen
	return m, nil
}

// NewClip builds a clip message carrying clipboard content.
func NewClip(mime, clientID, clientName string, payload []byte) *Msg {
	return &Msg{
		Kind:       KindClip,
		MIME:       mime,
		ClientID:   clientID,
		ClientName: clientName,
		Payload:    payload,
	}
}

// NewClipHTML builds a rich-text clip message carrying both the HTML
// rendition (primary) and the plain-text rendition (secondary).
func NewClipHTML(clientID, clientName string, html, plain []byte) *Msg {
	return &Msg{
		Kind:       KindClip,
		MIME:       MIMEHTML,
		MIME2:      MIMEText,
		ClientID:   clientID,
		ClientName: clientName,
		Payload:    html,
		Payload2:   plain,
	}
}

// previewRunes bounds the clipboard text embedded in Summary so that one log
// line stays short even for a large copy.
const previewRunes = 60

// Summary returns a one-line description of the message for logging, for
// example:
//
//	hello alice (host-1)
//	clip text/plain 12 B from alice: hello world
//	welcome count=2
//
// Text payloads are truncated to a short preview, but the preview may still
// contain raw control bytes from the clipboard: callers log the result through
// logx, which is what actually keeps one record on one line.
func (m *Msg) Summary() string {
	switch m.Kind {
	case KindHello:
		return fmt.Sprintf("hello %s", m.origin())
	case KindWelcome:
		return fmt.Sprintf("welcome count=%d", m.Count)
	case KindClip:
		mime := m.MIME
		if m.MIME2 != "" {
			mime += "+" + m.MIME2
		}
		s := fmt.Sprintf("clip %s %d B from %s", mime, len(m.Payload)+len(m.Payload2), m.origin())
		if p := m.plainText(); len(p) > 0 {
			if t := textPreview(p); t != "" {
				s += ": " + t
			}
		}
		return s
	case KindJoined:
		return fmt.Sprintf("joined %s", m.origin())
	case KindLeft:
		return fmt.Sprintf("left %s", m.origin())
	default:
		return fmt.Sprintf("unknown kind=%s", m.Kind)
	}
}

// origin renders the sender as "name (id)", falling back to whichever half is
// present so presence and clip records stay readable. Server-originated
// messages carry the same value in both fields, so they collapse to one name.
func (m *Msg) origin() string {
	if m.ClientID == OriginWeb {
		return OriginName
	}
	switch {
	case m.ClientName != "" && m.ClientID != "":
		return m.ClientName + " (" + m.ClientID + ")"
	case m.ClientName != "":
		return m.ClientName
	case m.ClientID != "":
		return m.ClientID
	default:
		return "unknown"
	}
}

// plainText returns the plain-text rendition of a clip for preview purposes:
// the primary payload for text/plain clips, or the secondary payload of a
// rich text/html clip. It returns nil for images and other kinds.
func (m *Msg) plainText() []byte {
	if m.MIME == MIMEText {
		return m.Payload
	}
	if m.MIME2 == MIMEText {
		return m.Payload2
	}
	return nil
}

// PlainText reports the plain-text rendition of a clip, used by the server to
// derive a preview for a rich-text entry.
func (m *Msg) PlainText() []byte {
	return m.plainText()
}

// textPreview returns the leading runes of a text payload with invalid UTF-8
// replaced, so a malformed copy cannot break the log line.
func textPreview(payload []byte) string {
	s := strings.ToValidUTF8(string(payload), "")
	r := []rune(s)
	if len(r) > previewRunes {
		return string(r[:previewRunes]) + "…"
	}
	return s
}
