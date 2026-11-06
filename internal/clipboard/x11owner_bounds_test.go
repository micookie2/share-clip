//go:build linux

package clipboard

import (
	"testing"
	"time"

	"github.com/jezek/xgb/xproto"
)

// TestEvictOldestTransfer covers the bound on in-flight INCR transfers. A
// requestor can announce an INCR transfer and then never delete the property;
// without eviction that entry keeps the whole payload alive until the owner is
// replaced. These tests exercise the eviction helper directly, without an X
// server, because it is pure bookkeeping.
func TestEvictOldestTransfer(t *testing.T) {
	base := time.Now()
	newOwner := func() *x11Owner {
		return &x11Owner{transfers: make(map[incrKey]*incrState)}
	}

	t.Run("drops the earliest-started transfer", func(t *testing.T) {
		o := newOwner()
		o.transfers[incrKey{1, 10}] = &incrState{data: []byte("old"), at: base}
		o.transfers[incrKey{2, 20}] = &incrState{data: []byte("mid"), at: base.Add(time.Second)}
		o.transfers[incrKey{3, 30}] = &incrState{data: []byte("new"), at: base.Add(2 * time.Second)}

		o.evictOldestTransferLocked()

		if _, ok := o.transfers[incrKey{1, 10}]; ok {
			t.Error("earliest transfer was not evicted")
		}
		if len(o.transfers) != 2 {
			t.Errorf("want 2 transfers left, got %d", len(o.transfers))
		}
	})

	t.Run("keeps the map within the cap", func(t *testing.T) {
		o := newOwner()
		for i := 0; i < maxIncrTransfers+5; i++ {
			key := incrKey{xproto.Window(i), xproto.Atom(i)}
			if len(o.transfers) >= maxIncrTransfers {
				o.evictOldestTransferLocked()
			}
			o.transfers[key] = &incrState{at: base.Add(time.Duration(i) * time.Millisecond)}
		}
		if len(o.transfers) > maxIncrTransfers {
			t.Errorf("transfers grew to %d, want at most %d", len(o.transfers), maxIncrTransfers)
		}
		if len(o.transfers) != maxIncrTransfers {
			t.Errorf("want the map to settle at %d entries, got %d", maxIncrTransfers, len(o.transfers))
		}
	})

	t.Run("empty map is a no-op", func(t *testing.T) {
		o := newOwner()
		o.evictOldestTransferLocked() // must not panic or insert anything
		if len(o.transfers) != 0 {
			t.Errorf("eviction on an empty map added entries: %d", len(o.transfers))
		}
	})
}
