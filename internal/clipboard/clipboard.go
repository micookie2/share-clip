// Package clipboard provides a small cross-platform clipboard abstraction
// used by the share-clip client.
//
// A Backend can snapshot the current clipboard content (text or PNG image),
// replace the clipboard content, and wait for the next clipboard event. The
// Watcher builds on top of a Backend: it detects local changes and reports
// them through a callback while automatically suppressing the echo caused by
// applying content that arrived over the network (see Watcher.ApplyRemote).
//
// Both platforms follow the same read policy: usable plain text wins over an
// image. Office suites (Excel, LibreOffice, WPS) put a rendered picture of the
// copied cells next to the real text, so an image-first policy made a copied
// text paste as a picture on the other machine; while text wins the image
// target is not even fetched, so the owner's delayed rendering is not forced.
// text/html, when offered, rides along as a richer rendition of the text. An
// image is only read — non-PNG encodings converted to PNG — when there is no
// usable text at all (screenshots, image editors).
//
// Two platforms are supported:
//   - Windows: Win32 clipboard APIs via the stdlib syscall package. The OS
//     exposes a clipboard sequence number, so every genuine copy action —
//     even re-copying identical content — can be detected and re-shared.
//   - Linux (X11): delegates reading/writing to xclip. Copies are detected
//     event-driven through XFixes selection-owner notifications (falling back
//     to polling when the extension is unavailable), and the formats the
//     owner advertises (xclip TARGETS) drive the snapshot.
//   - Linux (Wayland): delegates to wl-clipboard (wl-copy/wl-paste). Offered
//     MIME types are enumerated with wl-paste --list-types when the installed
//     version supports it (≥ 2.0), otherwise the snapshot falls back to a
//     text-then-PNG guess. Wayland has no compositor-wide copy event channel
//     (GNOME ships none; wlroots/KDE only via data-control protocols), so
//     monitoring stays poll based.
//
// File copies are detected (file:// URI lists) and ignored: file transfer is
// out of scope.
package clipboard

import (
	"crypto/sha256"
	"strings"
	"sync"
	"time"
)

// Kind describes what kind of content a clipboard holds.
type Kind uint8

const (
	KindEmpty Kind = iota
	KindText
	KindImage
)

func (k Kind) String() string {
	switch k {
	case KindText:
		return "text"
	case KindImage:
		return "image"
	default:
		return "empty"
	}
}

// Content is a snapshot of the clipboard. Exactly one of Text or PNG is
// meaningful, depending on Kind. For rich text (KindText with a non-empty
// HTML), Text carries the plain-text rendition and HTML the text/html
// rendition; a plain-text copy has an empty HTML.
type Content struct {
	Kind Kind
	Text string // UTF-8 text, only when Kind == KindText
	// HTML is the rich-text rendition, only when Kind == KindText (may be
	// empty). When the platform provides a full HTML document (the CF_HTML
	// StartHTML..EndHTML range, as Windows owners like Excel do), it is stored
	// whole so formatting context such as <style> and <meta> is preserved; a
	// bare fragment from an owner or peer that only provides one is kept as
	// that fragment.
	HTML string
	PNG  []byte // PNG encoded bytes, only when Kind == KindImage
}

// Empty reports whether the content carries nothing to share.
func (c Content) Empty() bool { return c.Kind == KindEmpty }

// Digest returns a content-addressed digest used for de-duplication and echo
// suppression.
func (c Content) Digest() [32]byte {
	h := sha256.New()
	switch c.Kind {
	case KindText:
		h.Write([]byte{0})
		h.Write([]byte(c.Text))
		if c.HTML != "" {
			h.Write([]byte{1})
			h.Write([]byte(c.HTML))
		}
	case KindImage:
		h.Write([]byte{1})
		h.Write(c.PNG)
	default:
		h.Write([]byte{2})
	}
	var d [32]byte
	copy(d[:], h.Sum(nil))
	return d
}

// Backend is the platform specific clipboard access.
type Backend interface {
	// Snapshot reads the current clipboard content. It returns KindEmpty when
	// the clipboard holds nothing shareable (no text and no image).
	Snapshot() (Content, error)
	// Write replaces the clipboard with the given content. Empty content is
	// ignored.
	Write(c Content) error
	// WaitForEvent blocks until a clipboard event may have occurred or stop is
	// closed. It reports whether the wakeup corresponds to a genuine clipboard
	// action (true), meaning even re-copying identical content should be
	// re-shared, or to a plain time poll (false), where only actual content
	// changes count. Implementations must return promptly when stop closes.
	WaitForEvent(stop <-chan struct{}) bool
}

// NewBackend is implemented per platform (see clipboard_windows.go,
// clipboard_linux.go and clipboard_unsupported.go); pollIntervalMs is used by
// platforms that must poll (Linux) and ignored on Windows.
//func NewBackend(pollIntervalMs int) (Backend, error)

// warnf reports non-fatal clipboard degradation — for example a rich-text
// write that had to fall back to a lower-fidelity path. It defaults to a
// no-op so library use stays silent, and is replaced by SetWarnf.
var warnf = func(string, ...any) {}

// SetWarnf installs a callback for non-fatal clipboard warnings.
func SetWarnf(f func(string, ...any)) {
	if f != nil {
		warnf = f
	}
}

// WarnKnownClipboardAgents reports, through the warn callback, guest-side
// clipboard agents (e.g. spice-vdagent in a SPICE cloud desktop) that take
// ownership of the local clipboard and only support plain text. They make a
// locally copied rich text lose its formatting on the local machine while the
// peers still receive the rich rendition. Call it after SetWarnf; platforms
// without such known agents do nothing.
func WarnKnownClipboardAgents() {
	warnKnownClipboardAgents()
}

// defaultDowngradeWindow is how long after sharing a rich-text copy a
// plain-text-only rendition of the same text is treated as a lossy downgrade
// and ignored. It covers Excel finishing its multi-format write and a
// competing clipboard writer settling, while still allowing a deliberate later
// plain copy to be shared.
const defaultDowngradeWindow = 2 * time.Second

// Watcher monitors one Backend from a single goroutine and reports local
// clipboard changes through OnChange. All backend access is serialized by the
// watcher, so ApplyRemote may be called from other goroutines (e.g. the
// network reader) without racing the monitoring loop.
type Watcher struct {
	backend  Backend
	onChange func(Content)
	onError  func(error)

	mu         sync.Mutex
	lastDigest [32]byte
	suppressed *[32]byte // digest we just wrote ourselves via ApplyRemote

	// Downgrade guard: the last content accepted for sharing and when it was
	// accepted. A later snapshot with the same plain text but without the HTML
	// is a lossy downgrade of it and is ignored for a short window.
	lastContent      Content
	lastContentAt    time.Time
	downgradeWindow  time.Duration
	downgradeIgnored bool

	stop chan struct{}
	done chan struct{}
}

// WatcherOption tunes a Watcher.
type WatcherOption func(*Watcher)

// WithOnError reports backend errors encountered while monitoring.
func WithOnError(f func(error)) WatcherOption {
	return func(w *Watcher) {
		if f != nil {
			w.onError = f
		}
	}
}

// NewWatcher creates a watcher. onChange is invoked (from the watcher
// goroutine) with each locally observed clipboard event. Content that arrived
// via ApplyRemote is never reported back.
func NewWatcher(b Backend, onChange func(Content), opts ...WatcherOption) *Watcher {
	w := &Watcher{
		backend:         b,
		onChange:        onChange,
		onError:         func(error) {},
		downgradeWindow: defaultDowngradeWindow,
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
	for _, o := range opts {
		o(w)
	}
	return w
}

// Start launches the watcher loop in the background. The current clipboard
// content is primed without being reported, so a freshly started client does
// not spam the network with whatever is already on the clipboard.
func (w *Watcher) Start() {
	go func() {
		defer close(w.done)

		// Prime with the current state.
		if c, err := w.backend.Snapshot(); err == nil {
			w.mu.Lock()
			w.lastDigest = c.Digest()
			w.observe(c)
			w.mu.Unlock()
		}

		for {
			select {
			case <-w.stop:
				return
			default:
			}
			genuine := w.backend.WaitForEvent(w.stop)
			select {
			case <-w.stop:
				return
			default:
			}
			w.poll(genuine)
		}
	}()
}

// Stop terminates the watcher loop and waits for it to exit.
func (w *Watcher) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

// ApplyRemote writes network-received content into the clipboard while arming
// echo suppression: the next observation of that content is recognized as our
// own write and is not re-shared.
//
// The suppression key is the content the *platform* presents after the write,
// not necessarily the bytes that were handed over. Writing a PNG to the
// Windows clipboard stores it as a CF_DIB bitmap, and reading it back yields a
// re-encoded PNG whose bytes usually differ from the original (same picture,
// different encoding). Suppressing only the raw bytes would then miss the
// echo: the received image would be re-shared once per machine and broadcast
// back to everyone as a "duplicate receive". Snapshotting right after the
// write captures the canonical form the watcher will actually observe, so the
// echo is suppressed reliably. Platforms whose clipboard preserves bytes
// exactly (Linux xclip/wl-clipboard, Windows text) simply observe the same
// bytes and nothing changes.
func (w *Watcher) ApplyRemote(c Content) error {
	if c.Empty() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.backend.Write(c); err != nil {
		// The write failed, so the clipboard is unchanged; do not arm any
		// suppression that could swallow a later genuine local copy.
		return err
	}
	d := c.Digest()
	if rb, err := w.backend.Snapshot(); err == nil && !rb.Empty() {
		d = rb.Digest()
	}
	w.suppressed = &d
	return nil
}

// isLossyDowngrade reports whether c is the plain-text version of the rich
// text we accepted most recently: same KindText text, the previous content had
// HTML and c does not. The caller must hold w.mu.
func (w *Watcher) isLossyDowngrade(c Content) bool {
	if c.Kind != KindText || c.HTML != "" {
		return false
	}
	prev := w.lastContent
	if prev.Kind != KindText || prev.HTML == "" || prev.Text != c.Text {
		return false
	}
	return time.Since(w.lastContentAt) <= w.downgradeWindow
}

// observe records c as the last accepted clipboard content for the downgrade
// guard. The caller must hold w.mu.
func (w *Watcher) observe(c Content) {
	w.lastContent = c
	w.lastContentAt = time.Now()
}

func (w *Watcher) poll(genuine bool) {
	w.mu.Lock()

	c, err := w.backend.Snapshot()
	if err != nil {
		// Keep the previous digest so a transient failure does not look like
		// a content change.
		cb := w.onError
		w.mu.Unlock()
		cb(err)
		return
	}
	d := c.Digest()

	if w.suppressed != nil {
		if d == *w.suppressed {
			w.suppressed = nil
			w.lastDigest = d
			w.observe(c)
			w.downgradeIgnored = false
			w.mu.Unlock()
			return
		}
		// Our own write was superseded before it could be observed; a stale
		// suppression must not swallow a future genuine copy with that digest.
		w.suppressed = nil
	}

	// Downgrade guard: an application, a clipboard manager or a competing
	// share-clip instance can rewrite a just-shared rich-text copy as plain
	// text (or Excel's multi-format write is caught mid-flight). Sharing that
	// plain version would overwrite the rich clipboard on every peer, so it is
	// ignored — and not made the new lastDigest, so repeated degraded snapshots
	// stay ignored — until the content genuinely changes.
	if w.isLossyDowngrade(c) {
		warn := !w.downgradeIgnored
		w.downgradeIgnored = true
		w.mu.Unlock()
		if warn {
			warnf("clipboard: 本机剪贴板被其它程序改写为纯文本（常见于云桌面/虚拟机剪贴板代理，" +
				"如 spice-vdagent/VMware Tools/VirtualBox 增强功能，它们只支持纯文本），" +
				"已忽略以免覆盖对端已有的富文本；本机粘贴可能因此丢格式")
		}
		return
	}
	w.downgradeIgnored = false

	if d == w.lastDigest {
		if genuine && !c.Empty() {
			// A real copy action with identical content (Windows can detect
			// these): re-share it. Refreshing the baseline also restarts the
			// downgrade window for this rich rendition.
			w.observe(c)
			cc := c
			cb := w.onChange
			w.mu.Unlock()
			cb(cc)
			return
		}
		w.mu.Unlock()
		return
	}
	w.lastDigest = d
	w.observe(c)
	if c.Empty() {
		w.mu.Unlock()
		return // nothing to share
	}

	cc := c
	cb := w.onChange
	w.mu.Unlock()
	cb(cc)
}

// IsFileArtifact reports whether text most plausibly came from copying file
// objects (file manager copy), in which case it must not be treated as plain
// text to share — file transfer is explicitly out of scope. Such text looks
// like one or more file:// URIs.
func IsFileArtifact(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	sawAny := false
	for _, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sawAny = true
		if !strings.HasPrefix(line, "file://") {
			return false
		}
	}
	return sawAny
}
