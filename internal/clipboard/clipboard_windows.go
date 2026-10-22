//go:build windows

package clipboard

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"runtime"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/micookie2/share-clip/internal/dib"
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
	procRegisterClipboardFmt  = user32.NewProc("RegisterClipboardFormatW")

	procGlobalAlloc  = kernel32.NewProc("GlobalAlloc")
	procGlobalLock   = kernel32.NewProc("GlobalLock")
	procGlobalUnlock = kernel32.NewProc("GlobalUnlock")
	procGlobalSize   = kernel32.NewProc("GlobalSize")
	procGlobalFree   = kernel32.NewProc("GlobalFree")
)

// cfHTML is the registered "HTML Format" clipboard format carrying rich text.
// It is resolved once at package init; 0 means the format could not be
// registered and rich text is simply not read or written.
var cfHTML = registerClipboardFormat("HTML Format")

func registerClipboardFormat(name string) uint32 {
	u16 := utf16.Encode([]rune(name))
	if len(u16) == 0 {
		return 0
	}
	u16 = append(u16, 0) // RegisterClipboardFormatW expects a NUL-terminated string
	r, _, _ := procRegisterClipboardFmt.Call(uintptr(unsafe.Pointer(&u16[0])))
	return uint32(r)
}

// winBackend talks to the Win32 clipboard API.
type winBackend struct {
	lastSeq uint32
}

// NewBackend constructs the Windows clipboard backend.
func NewBackend(pollIntervalMs int) (Backend, error) {
	return &winBackend{}, nil
}

// Snapshot reads the current clipboard with a text-first policy: usable plain
// text (CF_UNICODETEXT) is what the user copied, even when the app also offers
// an image rendition of it (Excel cell/range copies offer text, HTML and a
// picture). The image format is only read when there is no usable text, so a
// text copy is shared as text and Excel is not asked to render its delayed
// picture. When CF_HTML is offered alongside text, the rich-text rendition is
// captured too. A single unreadable format does not abort the snapshot; the
// remaining formats are still tried.
//
// Only raw bytes are read while the clipboard is open; decoding/PNG-encoding a
// bitmap happens afterwards in windowsSnapshotContent, so a large image copy
// does not keep the clipboard locked (and block other applications) across the
// expensive part.
func (b *winBackend) Snapshot() (Content, error) {
	var textContent Content
	var rawDIB []byte
	err := withClipboard(func() error {
		src := clipboardSource{
			seen:       make(map[uint32]bool),
			htmlFormat: cfHTML,
			read:       readGlobal,
		}
		format := uint32(0)
		for {
			r, _, _ := procEnumClipboardFormats.Call(uintptr(format))
			if r == 0 {
				break
			}
			next := uint32(r)
			if !src.seen[next] {
				src.order = append(src.order, next)
			}
			src.seen[next] = true
			format = next
		}
		// Reading formats requires the clipboard to stay open; the DIB is only
		// copied here and decoded after withClipboard has closed the clipboard.
		var serr error
		textContent, rawDIB, serr = snapshotFromFormats(src)
		return serr
	})
	if err != nil {
		return Content{}, err
	}
	return windowsSnapshotContent(textContent, rawDIB)
}

// Write puts text or image content on the clipboard.
func (b *winBackend) Write(c Content) error {
	switch c.Kind {
	case KindEmpty:
		return nil
	case KindText:
		return b.writeText(c.Text, c.HTML)
	case KindImage:
		return b.writeImage(c.PNG)
	default:
		return errors.New("clipboard: unknown content kind")
	}
}

func (b *winBackend) writeText(text, html string) error {
	u16 := utf16.Encode([]rune(text))
	u16 = append(u16, 0) // NUL terminator expected by CF_UNICODETEXT
	textRaw := uint16Bytes(u16)
	hasHTML := html != "" && cfHTML != 0

	return withClipboard(func() error {
		// Allocate before emptying the clipboard so a failed allocation cannot
		// leave the clipboard emptied. Allocation does not require the
		// clipboard to be open.
		hText, err := allocAndCopy(textRaw)
		if err != nil {
			return err
		}
		var hHTML uintptr
		if hasHTML {
			hHTML, err = allocAndCopy(htmlToCFHTML(html))
			if err != nil {
				procGlobalFree.Call(hText)
				return err
			}
		}

		if err := emptyClipboard(); err != nil {
			procGlobalFree.Call(hText)
			if hasHTML {
				procGlobalFree.Call(hHTML)
			}
			return err
		}
		r, _, e1 := procSetClipboardData.Call(cfUnicodeText, hText)
		if r == 0 {
			procGlobalFree.Call(hText)
			if hasHTML {
				procGlobalFree.Call(hHTML)
			}
			return fmt.Errorf("clipboard: SetClipboardData(CF_UNICODETEXT): %v", e1)
		}
		if hasHTML {
			r, _, e1 := procSetClipboardData.Call(uintptr(cfHTML), hHTML)
			if r == 0 {
				procGlobalFree.Call(hHTML)
				return fmt.Errorf("clipboard: SetClipboardData(CF_HTML): %v", e1)
			}
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
	// Win32 clipboard ownership is bound to the OS thread that called
	// OpenClipboard: only that thread's CloseClipboard releases it. A Go
	// goroutine may migrate between OS threads between (or even inside) the
	// OpenClipboard..CloseClipboard calls; if that happens CloseClipboard runs
	// on a different thread and the clipboard stays open forever — blocking
	// every other application's clipboard access until this process exits.
	// Pinning the goroutine to its thread for the whole open scope guarantees
	// OpenClipboard and CloseClipboard run on the same thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

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
