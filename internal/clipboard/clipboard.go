// Package clipboard provides a small cross-platform clipboard abstraction
// used by the share-clip client.
//
// A Backend can snapshot the current clipboard content (text or PNG image),
// replace the clipboard content, and wait for the next clipboard event. The
// Watcher builds on top of a Backend: it detects local changes and reports
// them through a callback while automatically suppressing the echo caused by
// applying content that arrived over the network (see Watcher.ApplyRemote).
//
// Two platforms are supported:
//   - Windows: Win32 clipboard APIs via the stdlib syscall package. The OS
//     exposes a clipboard sequence number, so every genuine copy action —
//     even re-copying identical content — can be detected and re-shared.
//   - Linux: delegates to xclip (X11) or wl-clipboard (Wayland), which must
//     be installed. Those tools only expose content, not copy events, so the
//     backend polls and identical re-copies cannot be told apart from "no
//     change" (they are naturally de-duplicated). File copies are detected
//     (file:// URI lists) and ignored: file transfer is out of scope.
package clipboard

import (
	"crypto/sha256"
	"strings"
	"sync"
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
// meaningful, depending on Kind.
type Content struct {
	Kind Kind
	Text string // UTF-8 text, only when Kind == KindText
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
		backend:  b,
		onChange: onChange,
		onError:  func(error) {},
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
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
// echo suppression: the next observation of that digest is recognized as our
// own write and is not re-shared.
func (w *Watcher) ApplyRemote(c Content) error {
	if c.Empty() {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	d := c.Digest()
	w.suppressed = &d
	if err := w.backend.Write(c); err != nil {
		// The write failed, so the clipboard is unchanged; disarm so a later
		// genuine local copy with the same digest is not swallowed.
		w.suppressed = nil
		return err
	}
	return nil
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
			w.mu.Unlock()
			return
		}
		// Our own write was superseded before it could be observed; a stale
		// suppression must not swallow a future genuine copy with that digest.
		w.suppressed = nil
	}

	if d == w.lastDigest {
		w.mu.Unlock()
		if genuine && !c.Empty() {
			// A real copy action with identical content (Windows can detect
			// these): re-share it.
			cc := c
			cb := w.onChange
			cb(cc)
		}
		return
	}
	w.lastDigest = d
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
