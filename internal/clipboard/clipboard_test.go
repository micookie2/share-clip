package clipboard

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeBackend models an event-driven clipboard like Windows: every Set is a
// genuine copy event, so identical re-copies are reported too.
type fakeBackend struct {
	mu       sync.Mutex
	content  Content
	event    bool
	failRead bool
	failWrit bool
}

func (f *fakeBackend) set(c Content) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.content = c
	f.event = true
}

func (f *fakeBackend) setFailWrite(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failWrit = v
}

func (f *fakeBackend) Snapshot() (Content, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRead {
		return Content{}, errors.New("read boom")
	}
	return f.content, nil
}

func (f *fakeBackend) Write(c Content) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWrit {
		return errors.New("write boom")
	}
	f.content = c
	f.event = true
	return nil
}

func (f *fakeBackend) WaitForEvent(stop <-chan struct{}) bool {
	t := time.NewTicker(time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return false
		case <-t.C:
			f.mu.Lock()
			ev := f.event
			f.event = false
			f.mu.Unlock()
			if ev {
				return true
			}
		}
	}
}

// pollBackend models a polled clipboard like Linux: WaitForEvent just sleeps,
// so identical content is naturally de-duplicated.
type pollBackend struct {
	fakeBackend
	interval time.Duration
}

func (p *pollBackend) WaitForEvent(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return false
	case <-time.After(p.interval):
		return false
	}
}

// quietSet changes content without raising a genuine event (as when the
// clipboard content is replaced between two polls).
func (p *pollBackend) quietSet(c Content) {
	p.fakeBackend.mu.Lock()
	p.fakeBackend.content = c
	p.fakeBackend.mu.Unlock()
}

// recorder is a thread-safe list of reported content, used by the tests since
// onChange fires from the watcher goroutine.
type recorder struct {
	mu    sync.Mutex
	items []Content
}

func (r *recorder) add(c Content) {
	r.mu.Lock()
	r.items = append(r.items, c)
	r.mu.Unlock()
}

func (r *recorder) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.items)
}

func (r *recorder) item(i int) Content {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.items[i]
}

func TestWatcherReportsLocalChanges(t *testing.T) {
	b := &fakeBackend{}
	rec := &recorder{}
	w := NewWatcher(b, rec.add)
	w.Start()
	defer w.Stop()

	time.Sleep(20 * time.Millisecond) // let it prime the empty state

	b.set(Content{Kind: KindText, Text: "hello"})
	waitFor(t, 2*time.Second, func() bool { return rec.len() >= 1 })
	if rec.len() == 0 || rec.item(0).Text != "hello" {
		t.Fatalf("got %d items, want hello first", rec.len())
	}

	// Same content copied again is a genuine event on Windows: re-share it.
	b.set(Content{Kind: KindText, Text: "hello"})
	waitFor(t, 2*time.Second, func() bool { return rec.len() >= 2 })
	if rec.len() < 2 || rec.item(1).Text != "hello" {
		t.Fatalf("identical repeat not re-shared: %+v", rec.items)
	}
}

func TestWatcherNoEmitWithoutEvent(t *testing.T) {
	b := &fakeBackend{}
	rec := &recorder{}
	w := NewWatcher(b, rec.add)
	w.Start()
	defer w.Stop()
	time.Sleep(30 * time.Millisecond)
	if rec.len() != 0 {
		t.Fatalf("spurious emissions: %d", rec.len())
	}
}

func TestWatcherRemoteApplyNotReported(t *testing.T) {
	b := &fakeBackend{}
	rec := &recorder{}
	w := NewWatcher(b, rec.add)
	w.Start()
	defer w.Stop()

	time.Sleep(20 * time.Millisecond)

	remote := Content{Kind: KindText, Text: "from network"}
	if err := w.ApplyRemote(remote); err != nil {
		t.Fatalf("ApplyRemote: %v", err)
	}

	deadline := time.Now().Add(250 * time.Millisecond)
	for time.Now().Before(deadline) {
		if rec.len() != 0 {
			t.Fatalf("remote content reported as local change: %+v", rec.items)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A genuine local re-copy of the same content afterwards must be shared.
	b.set(Content{Kind: KindText, Text: "from network"})
	waitFor(t, 2*time.Second, func() bool { return rec.len() == 1 })
}

func TestWatcherUserChangeBetweenApplyAndPoll(t *testing.T) {
	b := &fakeBackend{}
	rec := &recorder{}
	w := NewWatcher(b, rec.add)
	w.Start()
	defer w.Stop()

	time.Sleep(20 * time.Millisecond)
	if err := w.ApplyRemote(Content{Kind: KindText, Text: "remote"}); err != nil {
		t.Fatalf("ApplyRemote: %v", err)
	}
	// User copies something else before the watcher observes the remote write.
	b.set(Content{Kind: KindText, Text: "user"})
	waitFor(t, 2*time.Second, func() bool { return rec.len() == 1 })
	if rec.len() != 1 || rec.item(0).Text != "user" {
		t.Fatalf("got %+v", rec.items)
	}
}

func TestWatcherPolledLinuxSemantics(t *testing.T) {
	b := &pollBackend{interval: 2 * time.Millisecond}
	rec := &recorder{}
	w := NewWatcher(b, rec.add)
	w.Start()
	defer w.Stop()

	time.Sleep(10 * time.Millisecond) // prime

	// Content replaced quietly: picked up by a poll.
	b.quietSet(Content{Kind: KindText, Text: "pasted externally"})
	waitFor(t, 2*time.Second, func() bool { return rec.len() == 1 })
	if rec.item(0).Text != "pasted externally" {
		t.Fatalf("got %+v", rec.items)
	}

	// Identical content cannot be distinguished from "no change" -> no emit.
	time.Sleep(100 * time.Millisecond)
	if rec.len() != 1 {
		t.Fatalf("identical content re-shared on polled platform")
	}

	// Remote apply under poll semantics: not re-shared.
	remote := Content{Kind: KindText, Text: "from network"}
	if err := w.ApplyRemote(remote); err != nil {
		t.Fatalf("ApplyRemote: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if rec.len() != 1 {
		t.Fatalf("remote content re-shared: %+v", rec.items)
	}
}

func TestWatcherWriteErrorDisarmsSuppression(t *testing.T) {
	b := &fakeBackend{}
	rec := &recorder{}
	w := NewWatcher(b, rec.add)
	w.Start()
	defer w.Stop()

	time.Sleep(20 * time.Millisecond)
	b.setFailWrite(true)
	if err := w.ApplyRemote(Content{Kind: KindText, Text: "nope"}); err == nil {
		t.Fatalf("expected write error")
	}
	b.setFailWrite(false)

	// User now copies the same text locally: it must be reported (the failed
	// remote write must not have armed suppression).
	b.set(Content{Kind: KindText, Text: "nope"})
	waitFor(t, 2*time.Second, func() bool { return rec.len() == 1 })
	if rec.len() != 1 || rec.item(0).Text != "nope" {
		t.Fatalf("got %+v", rec.items)
	}
}

func TestWatcherStops(t *testing.T) {
	b := &fakeBackend{}
	w := NewWatcher(b, func(Content) {})
	w.Start()
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return")
	}
}

func TestIsFileArtifact(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"", false},
		{"hello world", false},
		{"file:///home/user/a.txt", true},
		{"file:///home/user/a.txt\nfile:///home/user/b.txt", true},
		{"\nfile:///home/user/a.txt\r\n", true},
		{"file:///a\nplain text", false},
		{"https://example.com/x", false},
		{"  file:///x  ", true},
	}
	for _, c := range cases {
		if got := IsFileArtifact(c.text); got != c.want {
			t.Errorf("IsFileArtifact(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %v", d)
	}
}
