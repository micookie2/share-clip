//go:build linux

package clipboard

import (
	"errors"
	"fmt"
	"sync"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xfixes"
	"github.com/jezek/xgb/xproto"
)

// xfixesEvents turns X11 clipboard copies into events. It subscribes to
// XFixes selection-owner notifications for the CLIPBOARD selection — the same
// mechanism every X11 clipboard manager (CopyQ, parcellite, clipnotify, …)
// uses — and signals a buffered channel each time the owner changes. This
// makes monitoring event driven: no polling latency, and re-copying
// identical content is observable again (each copy re-owns the selection).
type xfixesEvents struct {
	conn      *xgb.Conn
	changed   chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// newXfixesEvents opens a dedicated X connection, subscribes to
// selection-owner changes on the CLIPBOARD selection and starts the reader
// goroutine. It returns an error (caller falls back to polling) when the X
// connection or the XFixes extension is unavailable.
func newXfixesEvents() (*xfixesEvents, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("clipboard: x11 connect: %w", err)
	}
	fail := func(err error) (*xfixesEvents, error) {
		conn.Close()
		return nil, err
	}
	if err := xfixes.Init(conn); err != nil {
		return fail(fmt.Errorf("clipboard: xfixes unavailable: %w", err))
	}
	reply, err := xfixes.QueryVersion(conn, 5, 0).Reply()
	if err != nil {
		return fail(fmt.Errorf("clipboard: xfixes version: %w", err))
	}
	// Selection-owner notification events were introduced in XFixes 3.0;
	// anything older (pre-2006 X servers) cannot report copies.
	if reply.MajorVersion < 3 {
		return fail(errors.New("clipboard: xfixes too old to report selection changes"))
	}

	clipboard := internAtom(conn, "CLIPBOARD")
	if clipboard == 0 {
		return fail(errors.New("clipboard: intern atom CLIPBOARD failed"))
	}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	// XFixesSetSelectionOwnerNotify is event bit 0 of the selection mask.
	if err := xfixes.SelectSelectionInputChecked(conn, root, clipboard,
		xfixes.SelectionEventMaskSetSelectionOwner).Check(); err != nil {
		return fail(fmt.Errorf("clipboard: select selection input: %w", err))
	}

	w := &xfixesEvents{
		conn:    conn,
		changed: make(chan struct{}, 16),
		done:    make(chan struct{}),
	}
	go w.readLoop()
	return w, nil
}

// readLoop forwards XFixes selection notifications until the connection is
// closed (by close) or dies on its own.
func (w *xfixesEvents) readLoop() {
	defer close(w.done)
	for {
		ev, xerr := w.conn.WaitForEvent()
		if xerr != nil {
			continue // a benign X error, keep listening
		}
		if ev == nil {
			return // connection closed
		}
		sel, ok := ev.(xfixes.SelectionNotifyEvent)
		if !ok || sel.Subtype != xfixes.SelectionEventSetSelectionOwner {
			continue
		}
		select {
		case w.changed <- struct{}{}:
		default: // channel full: coalesce, WaitForEvent will drain soon
		}
	}
}

// close tears the subscription down and unblocks the reader goroutine. It is
// idempotent and safe against concurrent callers.
func (w *xfixesEvents) close() {
	w.closeOnce.Do(func() {
		w.conn.Close()
		<-w.done
	})
}

func internAtom(c *xgb.Conn, name string) xproto.Atom {
	reply, err := xproto.InternAtom(c, false, uint16(len(name)), name).Reply()
	if err != nil {
		return 0
	}
	return reply.Atom
}
