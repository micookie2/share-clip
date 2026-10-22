package clipboard

// Tests for the platform-neutral sequence watcher in clipboard_seq.go. The
// sequence number is an injected atomic counter and the clock is the real one
// with shortened poll/quiet windows, so no Win32 syscall and no long sleep is
// involved.

import (
	"sync/atomic"
	"testing"
	"time"
)

// newTestSeqWatcher creates a watcher over an atomic sequence counter with a
// short poll/quiet window so the tests run in milliseconds.
func newTestSeqWatcher(initial uint32) (*seqWatcher, *atomic.Uint32) {
	var seq atomic.Uint32
	seq.Store(initial)
	w := newSeqWatcher(func() uint32 { return seq.Load() }, realSeqClock)
	w.poll = 2 * time.Millisecond
	w.quiet = 30 * time.Millisecond
	return w, &seq
}

// awaitEvent waits for the next wait() result.
func awaitEvent(t *testing.T, events <-chan bool, timeout time.Duration) bool {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for WaitForEvent")
		return false
	}
}

// TestSeqWatcherBurstCoalescesToOneEvent models one Excel copy writing its
// formats in quick succession (possibly also bumping the number several times):
// the watcher must report a single event, not one per bump.
func TestSeqWatcherBurstCoalescesToOneEvent(t *testing.T) {
	w, seq := newTestSeqWatcher(10)
	stop := make(chan struct{})
	defer close(stop)
	events := make(chan bool, 4)

	go func() { events <- w.wait(stop) }()
	for _, v := range []uint32{11, 12, 13} { // tight burst, well inside the quiet window
		seq.Store(v)
	}

	if ev := awaitEvent(t, events, 2*time.Second); !ev {
		t.Fatal("burst must produce a genuine event")
	}
	if got := seq.Load(); got != 13 {
		t.Fatalf("sequence changed during test: %d", got)
	}

	// The same burst must not produce a second event: tracked is now stable at
	// the last value and nothing changed.
	go func() { events <- w.wait(stop) }()
	select {
	case ev := <-events:
		t.Fatalf("burst produced a second event (ev=%v)", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSeqWatcherRebaseAbsorbsSelfInducedBump covers the delayed-rendering case:
// while the watcher is settling a bump caused by our own GetClipboardData, the
// end-of-Snapshot rebase moves the tracked number to the current one, so the
// change is absorbed instead of reported as a new copy.
func TestSeqWatcherRebaseAbsorbsSelfInducedBump(t *testing.T) {
	w, seq := newTestSeqWatcher(100)
	stop := make(chan struct{})
	defer close(stop)
	events := make(chan bool, 4)

	go func() { events <- w.wait(stop) }()

	seq.Store(101)                   // forced delayed rendering bumped the number
	time.Sleep(5 * time.Millisecond) // let wait() notice and start settling
	w.rebase()                       // Snapshot finished: absorb our own bump

	select {
	case ev := <-events:
		t.Fatalf("self-induced bump reported as a copy (ev=%v)", ev)
	case <-time.After(200 * time.Millisecond):
	}

	// A real copy after the rebase is still reported exactly once.
	seq.Store(102)
	if ev := awaitEvent(t, events, 2*time.Second); !ev {
		t.Fatal("genuine change after rebase was not reported")
	}
}

// TestSeqWatcherChangeAfterQuietIsOneEvent covers the normal case: one bump,
// stable, then exactly one event.
func TestSeqWatcherChangeAfterQuietIsOneEvent(t *testing.T) {
	w, seq := newTestSeqWatcher(1)
	stop := make(chan struct{})
	defer close(stop)
	events := make(chan bool, 4)

	go func() { events <- w.wait(stop) }()
	seq.Store(2)

	if ev := awaitEvent(t, events, 2*time.Second); !ev {
		t.Fatal("change must be reported as a genuine event")
	}

	// Nothing changed since: no further event.
	go func() { events <- w.wait(stop) }()
	select {
	case ev := <-events:
		t.Fatalf("spurious event without a change (ev=%v)", ev)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSeqWatcherInitAtCurrentSeqNoEvent verifies the constructor seeds the
// tracked number: monitoring a pre-existing clipboard produces no startup event.
func TestSeqWatcherInitAtCurrentSeqNoEvent(t *testing.T) {
	w, seq := newTestSeqWatcher(42)
	stop := make(chan struct{})
	events := make(chan bool, 1)

	go func() { events <- w.wait(stop) }()
	select {
	case ev := <-events:
		t.Fatalf("pre-existing clipboard produced a startup event (ev=%v)", ev)
	case <-time.After(100 * time.Millisecond):
	}
	close(stop)
	if ev := awaitEvent(t, events, 2*time.Second); ev {
		t.Fatal("stop must return false, not an event")
	}
	if got := seq.Load(); got != 42 {
		t.Fatalf("sequence changed during test: %d", got)
	}
}

// TestSeqWatcherStopIsPrompt checks a blocked wait returns quickly once stop
// closes.
func TestSeqWatcherStopIsPrompt(t *testing.T) {
	w, _ := newTestSeqWatcher(7)
	stop := make(chan struct{})
	events := make(chan bool, 1)

	go func() { events <- w.wait(stop) }()
	time.Sleep(10 * time.Millisecond)
	closedAt := time.Now()
	close(stop)

	select {
	case ev := <-events:
		if ev {
			t.Fatal("stop must return false")
		}
		if d := time.Since(closedAt); d > 500*time.Millisecond {
			t.Fatalf("stop took %v, want prompt return", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not return after stop closed")
	}
}

// TestSeqWatcherRebaseWithoutChangeNoEvent verifies a rebase that does not move
// the number is a no-op and does not fabricate an event.
func TestSeqWatcherRebaseWithoutChangeNoEvent(t *testing.T) {
	w, _ := newTestSeqWatcher(5)
	stop := make(chan struct{})
	defer close(stop)
	events := make(chan bool, 1)

	go func() { events <- w.wait(stop) }()
	w.rebase()
	select {
	case ev := <-events:
		t.Fatalf("rebase without a change produced an event (ev=%v)", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

// TestSeqWatcherSettleQuietWindowDeterministic drives settle() with an injected
// virtual clock: once the sequence number has been stable for the quiet window
// the change is committed as exactly one event.
func TestSeqWatcherSettleQuietWindowDeterministic(t *testing.T) {
	now := time.Unix(0, 0)
	var seq atomic.Uint32
	seq.Store(1) // tracked value when monitoring started
	clock := seqClock{
		now:   func() time.Time { return now },
		sleep: func(d time.Duration) { now = now.Add(d) },
	}
	w := newSeqWatcher(func() uint32 { return seq.Load() }, clock)
	w.poll = 10 * time.Millisecond
	w.quiet = 30 * time.Millisecond
	seq.Store(2) // user copy observed; watcher settling on it

	event, ok := w.settle(2, 1, make(chan struct{}))
	if !ok || !event {
		t.Fatalf("settle = (event=%v, ok=%v), want one event", event, ok)
	}
	w.mu.Lock()
	tracked := w.seq
	w.mu.Unlock()
	if tracked != 2 {
		t.Fatalf("tracked sequence = %d, want 2", tracked)
	}
}

// TestSeqWatcherSettleAbsorbsRebaseDeterministic drives settle() with an
// injected virtual clock and a rebase that fires mid-settle (as a concurrent
// Snapshot would): the observed change must be absorbed, not reported.
func TestSeqWatcherSettleAbsorbsRebaseDeterministic(t *testing.T) {
	now := time.Unix(0, 0)
	var seq atomic.Uint32
	seq.Store(1) // tracked value when monitoring started
	var w *seqWatcher
	sleeps := 0
	clock := seqClock{
		now: func() time.Time { return now },
		sleep: func(d time.Duration) {
			now = now.Add(d)
			sleeps++
			if sleeps == 1 { // our own snapshot read bumped the number
				seq.Store(3)
				w.rebase()
			}
		},
	}
	w = newSeqWatcher(func() uint32 { return seq.Load() }, clock)
	w.poll = 10 * time.Millisecond
	w.quiet = 30 * time.Millisecond
	seq.Store(2) // user copy observed; watcher settling on it

	event, ok := w.settle(2, 1, make(chan struct{}))
	if !ok || event {
		t.Fatalf("settle = (event=%v, ok=%v), want absorbed change", event, ok)
	}
	w.mu.Lock()
	tracked := w.seq
	w.mu.Unlock()
	if tracked != 3 {
		t.Fatalf("tracked sequence = %d, want the rebased 3", tracked)
	}
}
