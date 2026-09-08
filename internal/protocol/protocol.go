// Package protocol defines the wire format used between the share-clip
// clients and the server.
//
// Every WebSocket message is one protocol frame with the layout:
//
//	[4 bytes]  big-endian uint32: length of the JSON header
//	[N bytes]  JSON header (see Msg; Payload is excluded)
//	[4 bytes]  big-endian uint32: length of the binary payload
//	[M bytes]  raw payload bytes
//
// The payload carries the actual clipboard content: UTF-8 text for
// text/plain messages or PNG encoded bytes for image/png messages. Control
// messages (hello, welcome, ...) simply have a zero length payload.
package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
)

// Message kinds.
const (
	KindHello   = "hello"   // client -> server, on connect
	KindWelcome = "welcome" // server -> client, right after hello
	KindClip    = "clip"    // client -> server and server -> clients
	KindJoined  = "joined"  // server -> clients, presence notification
	KindLeft    = "left"    // server -> clients, presence notification
	KindPing    = "ping"    // keepalive, both directions
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
// JSON header and is never serialized into the header itself.
type Msg struct {
	Kind       string `json:"kind"`
	MIME       string `json:"mime,omitempty"`       // text/plain | image/png
	ClientID   string `json:"clientId,omitempty"`   // originator identity
	ClientName string `json:"clientName,omitempty"` // originator display name
	Count      int    `json:"count,omitempty"`      // used by welcome
	Size       int    `json:"size,omitempty"`       // payload length, informational
	Payload    []byte `json:"-"`
}

// Frame returns the wire bytes for a message. Payloads larger than maxPayload
// are rejected.
func (m *Msg) Frame(maxPayload int) ([]byte, error) {
	if maxPayload <= 0 {
		maxPayload = DefaultMaxPayload
	}
	if len(m.Payload) > maxPayload {
		return nil, ErrPayloadTooLarge
	}
	header, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("protocol: marshal header: %w", err)
	}
	if len(header) > MaxHeaderSize {
		return nil, ErrHeaderTooLarge
	}
	out := make([]byte, 8+len(header)+len(m.Payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(len(header)))
	copy(out[4:], header)
	binary.BigEndian.PutUint32(out[4+len(header):8+len(header)], uint32(len(m.Payload)))
	copy(out[8+len(header):], m.Payload)
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
	pLen := int(binary.BigEndian.Uint32(body[hLen : hLen+4]))
	if pLen > maxPayload {
		return nil, ErrPayloadTooLarge
	}
	if len(body) < hLen+4+pLen {
		return nil, ErrTruncated
	}
	if pLen > 0 {
		m.Payload = body[hLen+4 : hLen+4+pLen]
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
