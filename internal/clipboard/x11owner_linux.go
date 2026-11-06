//go:build linux

package clipboard

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// x11Owner is a minimal X11 CLIPBOARD selection owner that serves both a
// plain-text rendition (UTF8_STRING / text/plain) and a rich-text rendition
// (text/html) for the same copy. It replaces xclip for the rich-text write
// path because xclip can only own one target at a time: pasting into a rich
// editor or a plain editor must both work from a single clipboard.
//
// The owner holds an X connection open for as long as it owns the selection
// and answers conversion requests in a background goroutine. It exits when it
// loses ownership (SelectionClear) or when close is called.
type x11Owner struct {
	conn *xgb.Conn
	win  xproto.Window

	clipboard   xproto.Atom
	targets     xproto.Atom
	atom        xproto.Atom
	timestamp   xproto.Atom
	multiple    xproto.Atom
	saveTargets xproto.Atom
	utf8        xproto.Atom
	str         xproto.Atom
	text        xproto.Atom
	textPlain   xproto.Atom
	plainUTF8   xproto.Atom
	textHTML    xproto.Atom
	htmlUTF8    xproto.Atom
	incr        xproto.Atom

	// selectionTime is handed out for the TIMESTAMP target (ICCCM §2.6). It is
	// the Unix time at which this owner took the selection, not a server
	// timestamp: xgb exposes no cheap "current server time" request, and every
	// consumer of TIMESTAMP (Qt/GTK, clipboard managers) only needs a plausible
	// 32-bit timestamp to date the copy.
	selectionTime uint32

	plain []byte
	html  []byte

	mu        sync.Mutex
	transfers map[incrKey]*incrState

	done      chan struct{}
	closeOnce sync.Once
}

type incrKey struct {
	requestor xproto.Window
	property  xproto.Atom
}

type incrState struct {
	typ  xproto.Atom
	data []byte
	pos  int
	at   time.Time // when the transfer started, used to evict abandoned ones
}

// maxIncrTransfers bounds how many in-flight INCR transfers keep their payload
// alive. A requestor that announces an INCR transfer and then never deletes the
// property would otherwise pin its bytes until the owner is replaced; with a cap
// the oldest such transfer is dropped instead, costing at most one aborted
// transfer to a reader that stopped reading anyway.
const maxIncrTransfers = 16

// newX11Owner creates a window, takes ownership of the CLIPBOARD selection and
// starts the conversion-request loop. It returns an error when the X
// connection cannot be established or ownership cannot be taken.
func newX11Owner(plain, html string) (*x11Owner, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, fmt.Errorf("clipboard: x11 connect: %w", err)
	}
	o := &x11Owner{
		conn:      conn,
		plain:     []byte(plain),
		html:      []byte(html),
		transfers: make(map[incrKey]*incrState),
		done:      make(chan struct{}),
	}
	if err := o.init(); err != nil {
		conn.Close()
		return nil, err
	}
	go o.loop()
	return o, nil
}

func (o *x11Owner) init() error {
	o.clipboard = internAtom(o.conn, "CLIPBOARD")
	o.targets = internAtom(o.conn, "TARGETS")
	o.atom = internAtom(o.conn, "ATOM")
	o.timestamp = internAtom(o.conn, "TIMESTAMP")
	o.multiple = internAtom(o.conn, "MULTIPLE")
	o.saveTargets = internAtom(o.conn, "SAVE_TARGETS")
	o.utf8 = internAtom(o.conn, "UTF8_STRING")
	o.str = internAtom(o.conn, "STRING")
	o.text = internAtom(o.conn, "TEXT")
	o.textPlain = internAtom(o.conn, "text/plain")
	o.plainUTF8 = internAtom(o.conn, "text/plain;charset=utf-8")
	o.textHTML = internAtom(o.conn, "text/html")
	o.htmlUTF8 = internAtom(o.conn, "text/html;charset=utf-8")
	o.incr = internAtom(o.conn, "INCR")
	for _, a := range []xproto.Atom{o.clipboard, o.targets, o.atom, o.timestamp, o.multiple,
		o.saveTargets, o.utf8, o.str, o.text, o.textPlain, o.plainUTF8, o.textHTML,
		o.htmlUTF8, o.incr} {
		if a == 0 {
			return errors.New("clipboard: x11 intern atom failed")
		}
	}

	wid, err := o.conn.NewId()
	if err != nil {
		return fmt.Errorf("clipboard: x11 resource id: %w", err)
	}
	o.win = xproto.Window(wid)
	root := xproto.Setup(o.conn).DefaultScreen(o.conn).Root
	if err := xproto.CreateWindowChecked(o.conn, 0, o.win, root, 0, 0, 1, 1, 0,
		xproto.WindowClassCopyFromParent, 0, 0, nil).Check(); err != nil {
		return fmt.Errorf("clipboard: x11 create window: %w", err)
	}
	if err := xproto.SetSelectionOwnerChecked(o.conn, o.win, o.clipboard, xproto.TimeCurrentTime).Check(); err != nil {
		return fmt.Errorf("clipboard: x11 set selection owner: %w", err)
	}
	// Taken after ownership succeeds so TIMESTAMP dates the copy, not the
	// connection attempt.
	o.selectionTime = uint32(time.Now().Unix())
	return nil
}

// loop dispatches X events until the connection dies or ownership is lost.
func (o *x11Owner) loop() {
	defer close(o.done)
	defer o.conn.Close()
	for {
		ev, xerr := o.conn.WaitForEvent()
		if xerr != nil {
			continue // benign X error, keep serving
		}
		if ev == nil {
			return // connection closed
		}
		switch e := ev.(type) {
		case xproto.SelectionRequestEvent:
			o.handleSelectionRequest(e)
		case xproto.SelectionClearEvent:
			return // a new owner took over
		case xproto.PropertyNotifyEvent:
			o.handlePropertyNotify(e)
		}
	}
}

// close tears the owner down and waits for the loop to exit. It is idempotent
// and safe against concurrent callers.
func (o *x11Owner) close() {
	o.closeOnce.Do(func() {
		o.conn.Close()
		<-o.done
	})
}

func (o *x11Owner) handleSelectionRequest(req xproto.SelectionRequestEvent) {
	// ICCCM §2.2: a request whose property is None means "use the target atom
	// as the property name". Real toolkits always pass a real property, but
	// honouring the rule keeps old clients working.
	property := req.Property
	if property == 0 {
		property = req.Target
	}

	// MULTIPLE is not a rendition: it asks for several conversions in one
	// round trip and must be answered with a single SelectionNotify
	// (ICCCM §2.7.2), so it cannot go through the ordinary write path.
	if req.Target == o.multiple {
		o.handleMultiple(req, property)
		return
	}

	data, typ, format, ok := o.dataForTarget(req.Target)
	if !ok {
		// SAVE_TARGETS is advertised but not implemented: ICCCM lets an owner
		// refuse a conversion it does not support, and advertising it is what
		// lets a clipboard manager consider this owner worth saving.
		o.sendSelectionNotify(req, 0)
		return
	}
	o.writeData(req.Requestor, property, typ, format, data)
	o.sendSelectionNotify(req, property)
}

// dataForTarget maps a requested target atom to the bytes, the property type
// and the property format to serve for it. TARGETS and TIMESTAMP are 32-bit
// properties (an atom list and an INTEGER respectively); every text rendition
// is 8-bit bytes tagged with the requested target atom as its type.
func (o *x11Owner) dataForTarget(target xproto.Atom) ([]byte, xproto.Atom, byte, bool) {
	switch target {
	case o.targets:
		return o.targetsList(), o.atom, 32, true
	case o.timestamp:
		// ICCCM §2.6: TIMESTAMP is the time the owner acquired the selection,
		// as a 32-bit INTEGER. Consumers use it to date the copy and to
		// distinguish a fresh selection from a stale one; refusing it makes
		// Qt/GTK clients treat the selection as unusable.
		return o.selectionTimeBytes(), xproto.AtomInteger, 32, true
	case o.utf8, o.str, o.text, o.textPlain, o.plainUTF8:
		// STRING/TEXT are the legacy ISO-8859-1 spellings of the same text.
		// Share-clip only ever holds UTF-8, and every consumer that asks for
		// STRING also accepts UTF8_STRING, so serving the same bytes under the
		// requested atom keeps both worlds working.
		return o.plain, target, 8, true
	case o.textHTML, o.htmlUTF8:
		return o.html, target, 8, true
	}
	return nil, 0, 0, false
}

// selectionTimeBytes renders the selection stamp as the 32-bit INTEGER
// TIMESTAMP must carry.
func (o *x11Owner) selectionTimeBytes() []byte {
	out := make([]byte, 4)
	xgb.Put32(out, o.selectionTime)
	return out
}

// targetsList returns the atom list advertised under TARGETS. It must include
// the protocol targets (TIMESTAMP, MULTIPLE, SAVE_TARGETS) next to the
// renditions: clipboard managers read this list to decide whether an owner is
// worth saving, and a list without them makes the rich rendition disappear as
// soon as the owning process goes away.
func (o *x11Owner) targetsList() []byte {
	atoms := []xproto.Atom{
		o.targets,
		o.timestamp,
		o.multiple,
		o.saveTargets,
		o.utf8,
		o.str,
		o.text,
		o.textPlain,
		o.plainUTF8,
		o.textHTML,
		o.htmlUTF8,
	}
	out := make([]byte, 4*len(atoms))
	for i, a := range atoms {
		xgb.Put32(out[i*4:], uint32(a))
	}
	return out
}

// handleMultiple implements the MULTIPLE target (ICCCM §2.7.2). The requestor
// puts a list of (target, property) ATOM pairs into the property named by the
// request, on its own window; for every pair the owner converts the target and
// stores the result in the pair's property. A pair that cannot be converted is
// reported by rewriting its property name in the list to None. Exactly one
// SelectionNotify answers the whole request — sending one per pair would make
// the requestor see notifications for conversions it never asked for.
func (o *x11Owner) handleMultiple(req xproto.SelectionRequestEvent, property xproto.Atom) {
	if property == 0 {
		o.sendSelectionNotify(req, 0)
		return
	}
	pairs, err := xproto.GetProperty(o.conn, false, req.Requestor, property,
		xproto.AtomAtom, 0, 1<<20).Reply()
	if err != nil || pairs == nil || len(pairs.Value) < 8 || pairs.Format != 32 {
		o.sendSelectionNotify(req, 0)
		return
	}

	// Work on a copy: the list lives on the requestor's window and is being
	// rewritten below to report the pairs that could not be converted.
	value := append([]byte(nil), pairs.Value...)
	failed := false
	for i := 0; i+8 <= len(value); i += 8 {
		target := xproto.Atom(xgb.Get32(value[i:]))
		dest := xproto.Atom(xgb.Get32(value[i+4:]))
		data, typ, format, ok := o.dataForTarget(target)
		if !ok || dest == 0 {
			// Mark this pair as failed by rewriting the property atom to None
			// (ICCCM §2.7.2); the other pairs are still converted.
			xgb.Put32(value[i+4:], 0)
			failed = true
			continue
		}
		o.writeData(req.Requestor, dest, typ, format, data)
	}
	if failed {
		// The rewritten list is the failure report, so it must reach the
		// requestor even though the request as a whole succeeded.
		_ = xproto.ChangePropertyChecked(o.conn, xproto.PropModeReplace,
			req.Requestor, property, xproto.AtomAtom, 32,
			propItems(len(value), 32), value).Check()
	}
	o.sendSelectionNotify(req, property)
}

// propItems converts a byte count to the item count xgb's ChangeProperty wants
// for the given format (8, 16 or 32 bits per item).
func propItems(n int, format byte) uint32 {
	if format == 0 {
		return 0
	}
	return uint32(n) * 8 / uint32(format)
}

// maxPropBytes is the largest payload a single ChangeProperty request can
// carry on this connection; anything bigger goes through INCR.
func (o *x11Owner) maxPropBytes() int {
	maxReq := int(xproto.Setup(o.conn).MaximumRequestLength) * 4
	if maxReq <= 128 {
		return 4096
	}
	return maxReq - 64
}

// writeData stores data in the requestor's property, switching to INCR when
// the payload is larger than a single ChangeProperty request can carry. It
// never sends SelectionNotify: the caller does that once the whole conversion
// is set up (MULTIPLE needs several conversions before its single reply).
func (o *x11Owner) writeData(requestor xproto.Window, property, typ xproto.Atom, format byte, data []byte) {
	if len(data) <= o.maxPropBytes() {
		_ = xproto.ChangePropertyChecked(o.conn, xproto.PropModeReplace, requestor,
			property, typ, format, propItems(len(data), format), data).Check()
		return
	}

	// INCR: announce the transfer size, then stream chunks as the requestor
	// deletes each one (see handlePropertyNotify).
	key := incrKey{requestor, property}
	o.mu.Lock()
	if len(o.transfers) >= maxIncrTransfers {
		o.evictOldestTransferLocked()
	}
	o.transfers[key] = &incrState{typ: typ, data: data, at: time.Now()}
	o.mu.Unlock()

	// The announcement is a single 32-bit value: format 32, one item.
	var lower [4]byte
	xgb.Put32(lower[:], uint32(len(data)))
	_ = xproto.ChangePropertyChecked(o.conn, xproto.PropModeReplace, requestor,
		property, o.incr, 32, 1, lower[:]).Check()
	// Select PropertyChange events on the requestor window so the owner is
	// notified each time the requestor deletes a chunk. ValueMask selects
	// which window attributes to change (CWEventMask), while the value list
	// carries the event mask itself.
	_ = xproto.ChangeWindowAttributesChecked(o.conn, requestor,
		cwEventMask, []uint32{xproto.EventMaskPropertyChange}).Check()
}

// evictOldestTransferLocked drops the INCR transfer that has been outstanding
// the longest, releasing its payload reference. It is how an abandoned transfer
// (a requestor that announced INCR and never deleted the property) is reclaimed.
// The caller must hold o.mu.
func (o *x11Owner) evictOldestTransferLocked() {
	var (
		oldestKey incrKey
		oldest    time.Time
		found     bool
	)
	for k, st := range o.transfers {
		if !found || st.at.Before(oldest) {
			oldestKey, oldest, found = k, st.at, true
		}
	}
	if found {
		delete(o.transfers, oldestKey)
	}
}

// cwEventMask is the X11 CWEventMask attribute bit for ChangeWindowAttributes
// (xproto does not generate the CW* constants).
const cwEventMask = 1 << 11

func (o *x11Owner) handlePropertyNotify(ev xproto.PropertyNotifyEvent) {
	const propertyDelete = 1
	if ev.State != propertyDelete {
		return
	}
	key := incrKey{ev.Window, ev.Atom}
	o.mu.Lock()
	st := o.transfers[key]
	if st == nil {
		o.mu.Unlock()
		return
	}
	if st.pos >= len(st.data) {
		delete(o.transfers, key)
		typ := st.typ
		o.mu.Unlock()
		// A zero-length chunk signals the end of the transfer.
		_ = xproto.ChangePropertyChecked(o.conn, xproto.PropModeReplace, ev.Window,
			ev.Atom, typ, 8, 0, nil).Check()
		return
	}
	n := len(st.data) - st.pos
	if n > o.maxPropBytes() {
		n = o.maxPropBytes()
	}
	chunk := st.data[st.pos : st.pos+n]
	st.pos += n
	typ := st.typ
	o.mu.Unlock()
	_ = xproto.ChangePropertyChecked(o.conn, xproto.PropModeReplace, ev.Window,
		ev.Atom, typ, 8, propItems(len(chunk), 8), chunk).Check()
}

func (o *x11Owner) sendSelectionNotify(req xproto.SelectionRequestEvent, property xproto.Atom) {
	ev := xproto.SelectionNotifyEvent{
		Time:      req.Time,
		Requestor: req.Requestor,
		Selection: req.Selection,
		Target:    req.Target,
		Property:  property,
	}
	_ = xproto.SendEvent(o.conn, false, req.Requestor, 0, string(ev.Bytes()))
}
