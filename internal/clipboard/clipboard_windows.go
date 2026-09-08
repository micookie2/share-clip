//go:build windows

package clipboard

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/micookie2/share-clip/internal/dib"
)

// Win32 clipboard format identifiers (winuser.h).
const (
	cfText        = 1  // CF_TEXT
	cfBitmap      = 2  // CF_BITMAP
	cfDIB         = 8  // CF_DIB
	cfUnicodeText = 13 // CF_UNICODETEXT
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procOpenClipboard         = user32.NewProc("OpenClipboard")
	procCloseClipboard        = user32.NewProc("CloseClipboard")
	procEmptyClipboard        = user32.NewProc("EmptyClipboard")
	procGetClipboardData      = user32.NewProc("GetClipboardData")
	procSetClipboardData      = user32.NewProc("SetClipboardData")
	procEnumClipboardFormats  = user32.NewProc("EnumClipboardFormats")
	procGetClipboardSeqNumber = user32.NewProc("GetClipboardSequenceNumber")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalSize   = kernel32.NewProc("GlobalSize")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
)

// winBackend talks to the Win32 clipboard API.
type winBackend struct {
	lastSeq uint32
}

// NewBackend constructs the Windows clipboard backend.
func NewBackend(pollIntervalMs int) (Backend, error) {
	return &winBackend{}, nil
}

// Snapshot reads the current clipboard. Image content is preferred over text;
// if the bitmap cannot be decoded but text is present, the text is returned.
func (b *winBackend) Snapshot() (Content, error) {
	var rawDIB []byte
	var rawText []byte
	var hasText bool

	err := withClipboard(func() error {
		seen := map[uint32]bool{}
		format := uint32(0)
		for {
			r, _, _ := procEnumClipboardFormats.Call(uintptr(format))
			if r == 0 {
				break
			}
			seen[uint32(r)] = true
			format = uint32(r)
		}
		if seen[cfDIB] {
			data, err := readGlobal(cfDIB)
			if err != nil {
				return err
			}
			rawDIB = data
		}
		if seen[cfUnicodeText] {
			data, err := readGlobal(cfUnicodeText)
			if err != nil {
				return err
			}
			rawText = data
			hasText = len(data) > 0
		}
		return nil
	})
	if err != nil {
		return Content{}, err
	}

	if len(rawDIB) > 0 {
		img, derr := dib.Decode(rawDIB)
		if derr == nil {
			var buf bytes.Buffer
			if err := png.Encode(&buf, img); err != nil {
				return Content{}, fmt.Errorf("clipboard: encode png: %w", err)
			}
			return Content{Kind: KindImage, PNG: buf.Bytes()}, nil
		}
		if !hasText {
			return Content{}, fmt.Errorf("clipboard: decode dib: %v", derr)
		}
		// Fall through to text below.
	}

	if hasText {
		text := utf16BytesToString(rawText)
		if text == "" || IsFileArtifact(text) {
			return Content{}, nil // nothing shareable
		}
		return Content{Kind: KindText, Text: text}, nil
	}
	return Content{}, nil
}

// Write puts text or image content on the clipboard.
func (b *winBackend) Write(c Content) error {
	switch c.Kind {
	case KindEmpty:
		return nil
	case KindText:
		return b.writeText(c.Text)
	case KindImage:
		return b.writeImage(c.PNG)
	default:
		return errors.New("clipboard: unknown content kind")
	}
}

func (b *winBackend) writeText(text string) error {
	u16 := utf16.Encode([]rune(text))
	u16 = append(u16, 0) // NUL terminator expected by CF_UNICODETEXT
	raw := uint16Bytes(u16)
	return withClipboard(func() error {
		if err := emptyClipboard(); err != nil {
			return err
		}
		h, err := allocAndCopy(raw)
		if err != nil {
			return err
		}
		r, _, e1 := procSetClipboardData.Call(cfUnicodeText, h)
		if r == 0 {
			procGlobalFree.Call(h)
			return fmt.Errorf("clipboard: SetClipboardData: %v", e1)
		}
		return nil
	})
}

func (b *winBackend) writeImage(pngBytes []byte) error {
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return fmt.Errorf("clipboard: decode png: %w", err)
	}
	dibBytes, err := dib.Encode(img)
	if err != nil {
		return fmt.Errorf("clipboard: encode dib: %w", err)
	}
	return withClipboard(func() error {
		if err := emptyClipboard(); err != nil {
			return err
		}
		h, err := allocAndCopy(dibBytes)
		if err != nil {
			return err
		}
		r, _, e1 := procSetClipboardData.Call(cfDIB, h)
		if r == 0 {
			procGlobalFree.Call(h)
			return fmt.Errorf("clipboard: SetClipboardData(CF_DIB): %v", e1)
		}
		return nil
	})
}

// WaitForEvent blocks until the clipboard sequence number changes (every real
// copy action bumps it, including re-copies of identical content).
func (b *winBackend) WaitForEvent(stop <-chan struct{}) bool {
	t := time.NewTicker(150 * time.Millisecond)
	defer t.Stop()
	for {
		seq := clipboardSequenceNumber()
		if seq != b.lastSeq {
			b.lastSeq = seq
			return true
		}
		select {
		case <-stop:
			return false
		case <-t.C:
		}
	}
}

// --- Win32 plumbing ---------------------------------------------------------

func withClipboard(fn func() error) error {
	// OpenClipboard fails while another process holds the clipboard open;
	// retry briefly before giving up.
	var lastErr error
	for i := 0; i < 20; i++ {
		r, _, _ := procOpenClipboard.Call(0) // NULL hwnd
		if r != 0 {
			err := fn()
			procCloseClipboard.Call()
			return err
		}
		lastErr = errors.New("clipboard: OpenClipboard failed (held by another process)")
		time.Sleep(25 * time.Millisecond)
	}
	return lastErr
}

func emptyClipboard() error {
	r, _, e1 := procEmptyClipboard.Call()
	if r == 0 {
		return fmt.Errorf("clipboard: EmptyClipboard: %v", e1)
	}
	return nil
}

// readGlobal copies the clipboard object's raw bytes while the clipboard is
// open. The system owns the memory; we only copy.
func readGlobal(format uint32) ([]byte, error) {
	h, _, e1 := procGetClipboardData.Call(uintptr(format))
	if h == 0 {
		return nil, fmt.Errorf("clipboard: GetClipboardData(%d): %v", format, e1)
	}
	p, _, e2 := procGlobalLock.Call(h)
	if p == 0 {
		return nil, fmt.Errorf("clipboard: GlobalLock: %v", e2)
	}
	defer procGlobalUnlock.Call(h)

	n, _, _ := procGlobalSize.Call(h)
	if n == 0 {
		return nil, errors.New("clipboard: GlobalSize returned 0")
	}
	src := unsafe.Slice((*byte)(unsafe.Pointer(p)), int(n))
	out := make([]byte, int(n))
	copy(out, src)
	return out, nil
}

// allocAndCopy allocates a moveable global memory block and fills it.
func allocAndCopy(data []byte) (uintptr, error) {
	const gmemMoveable = 0x0002
	if len(data) == 0 {
		return 0, errors.New("clipboard: empty allocation")
	}
	h, _, e1 := procGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if h == 0 {
		return 0, fmt.Errorf("clipboard: GlobalAlloc: %v", e1)
	}
	p, _, e2 := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return 0, fmt.Errorf("clipboard: GlobalLock: %v", e2)
	}
	dst := unsafe.Slice((*byte)(unsafe.Pointer(p)), len(data))
	copy(dst, data)
	procGlobalUnlock.Call(h)
	return h, nil
}

func clipboardSequenceNumber() uint32 {
	r, _, _ := procGetClipboardSeqNumber.Call()
	return uint32(r)
}

func utf16BytesToString(raw []byte) string {
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-1]
	}
	n := len(raw) / 2
	u16 := make([]uint16, n)
	for i := 0; i < n; i++ {
		u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	// Trim at the first NUL terminator, if any.
	for i, u := range u16 {
		if u == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}

func uint16Bytes(u16 []uint16) []byte {
	raw := make([]byte, len(u16)*2)
	for i, u := range u16 {
		raw[2*i] = byte(u)
		raw[2*i+1] = byte(u >> 8)
	}
	return raw
}
