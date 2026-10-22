package clipboard

// This file implements the platform-neutral half of the Win32 clipboard
// sequence-number event source used by the Windows backend. It deliberately
// carries no build tag (and no *_windows filename suffix, which Go would treat
// as an implicit GOOS constraint) so the debounce/rebase logic is unit-testable
// with an injected sequence source and clock; the real syscalls live in
// clipboard_windows.go.
//
// Why coalescing is needed: the clipboard sequence number is bumped by the
// owner for every format it writes, so one copy in Excel can raise it several
// times over a short window. Worse, reading a delayed-rendered format (our
// GetClipboardData calls force Excel to materialise its rendered picture and
// rich text) bumps it again, so without extra care share-clip observes its own
// reads as new copies and re-shares the same content repeatedly. Two rules fix
// that:
//   - debounce: a change is only reported once the sequence number has been
//     stable for a quiet window, so all writes of one copy collapse into one
//     event (and an early partial snapshot is avoided);
//   - rebase: at the end of Snapshot the tracked number is re-synced to the
//     current one, absorbing the bumps our own reads caused.

import (
	"sync"
	"time"
)

const (
	// seqPollInterval is how often the sequence number is sampled while
	// waiting for a change or for the quiet window to elapse.
	seqPollInterval = 25 * time.Millisecond
	// seqQuietWindow is how long the sequence number must stay unchanged
	// before a change is reported. Excel writes the formats of one copy over a
	// short window, so this must comfortably exceed that window while staying
	// imperceptible to the user.
	seqQuietWindow = 125 * time.Millisecond
)

// seqClock is the time source of seqWatcher. It is a pair of functions so tests
// can substitute a deterministic clock; realSeqClock uses the wall clock.
type seqClock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

var realSeqClock = seqClock{now: time.Now, sleep: time.Sleep}

// seqWatcher coalesces raw clipboard sequence-number changes into copy events.
// It is safe for concurrent use: the watcher goroutine blocks in wait while a
// concurrent ApplyRemote (another goroutine) may call rebase via Snapshot.
type seqWatcher struct {
	mu     sync.Mutex
	seq    uint32 // last tracked/committed sequence number
	getSeq func() uint32
	clock  seqClock
	poll   time.Duration
	quiet  time.Duration
}

// newSeqWatcher creates a watcher whose tracked sequence number starts at the
// clipboard's current value. Seeding matters: a pre-existing clipboard must not
// look like a fresh copy event when monitoring starts.
func newSeqWatcher(getSeq func() uint32, clock seqClock) *seqWatcher {
	if clock.now == nil {
		clock.now = time.Now
	}
	if clock.sleep == nil {
		clock.sleep = time.Sleep
	}
	return &seqWatcher{
		seq:    getSeq(),
		getSeq: getSeq,
		clock:  clock,
		poll:   seqPollInterval,
		quiet:  seqQuietWindow,
	}
}

// stopRequested reports whether stop has been closed.
func stopRequested(stop <-chan struct{}) bool {
	select {
	case <-stop:
		return true
	default:
		return false
	}
}

// wait blocks until a genuine clipboard change has settled, then returns true.
// A change is reported only after the sequence number has been stable for the
// quiet window, so a burst of writes from one copy yields exactly one event.
// Closing stop makes it return false promptly.
func (w *seqWatcher) wait(stop <-chan struct{}) bool {
	for {
		if stopRequested(stop) {
			return false
		}
		cur := w.getSeq()
		w.mu.Lock()
		tracked := w.seq
		w.mu.Unlock()
		if cur != tracked {
			event, ok := w.settle(cur, tracked, stop)
			if !ok {
				return false
			}
			if event {
				return true
			}
			// A concurrent rebase absorbed the change as self-induced; keep
			// waiting for the next one.
			continue
		}
		w.clock.sleep(w.poll)
	}
}

// settle waits until the sequence number observed for a change stops moving for
// the quiet window. baseline is the tracked value from before the change; if a
// concurrent rebase moved it while settling, the change was caused by our own
// snapshot reads and is not reported (ok=true, event=false). ok=false means
// stop was closed.
func (w *seqWatcher) settle(observed, baseline uint32, stop <-chan struct{}) (event, ok bool) {
	last := observed
	lastChange := w.clock.now()
	for {
		w.clock.sleep(w.poll)
		if stopRequested(stop) {
			return false, false
		}
		cur := w.getSeq()
		if cur != last {
			last = cur
			lastChange = w.clock.now()
			continue
		}
		if w.clock.now().Sub(lastChange) < w.quiet {
			continue
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.seq != baseline {
			// A Snapshot rebased the sequence number while we were settling:
			// this change was self-induced (e.g. delayed rendering forced by
			// our own reads), not a user copy.
			return false, true
		}
		w.seq = last
		return true, true
	}
}

// rebase re-syncs the tracked sequence number with the clipboard's current
// value. Snapshot calls it after CloseClipboard (and after the image decode) so
// the sequence bumps caused by its own reads are absorbed instead of being
// reported as new copies.
func (w *seqWatcher) rebase() {
	cur := w.getSeq()
	w.mu.Lock()
	w.seq = cur
	w.mu.Unlock()
}
