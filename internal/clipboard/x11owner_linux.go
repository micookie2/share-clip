//go:build linux

package clipboard

import (
	"errors"
	"fmt"
	"sync"

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

	clipboard xproto.Atom
	targets   xproto.Atom
	atom      xproto.Atom
	utf8      xproto.Atom
	textPlain xproto.Atom
	textHTML  xproto.Atom
	incr      xproto.Atom

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
}

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
	o.utf8 = internAtom(o.conn, "UTF8_STRING")
	o.textPlain = internAtom(o.conn, "text/plain")
	o.textHTML = internAtom(o.conn, "text/html")
	o.incr = internAtom(o.conn, "INCR")
	if o.clipboard == 0 || o.targets == 0 || o.atom == 0 || o.utf8 == 0 || o.textPlain == 0 || o.textHTML == 0 || o.incr == 0 {
		return errors.New("clipboard: x11 intern atom failed")
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
	data, typ, format, ok := o.dataForTarget(req.Target)
	if !ok {
		o.sendSelectionNotify(req, 0) // refuse: unsupported target
		return
	}
	o.writeTargetData(req, typ, format, data)
}

// dataForTarget maps a requested target atom to the bytes, the property type
// and the property format to serve for it. TARGETS is an atom list (format
// 32); text renditions are bytes (format 8).
func (o *x11Owner) dataForTarget(target xproto.Atom) ([]byte, xproto.Atom, byte, bool) {
	switch target {
	case o.targets:
		return o.targetsList(), o.atom, 32, true
	case o.utf8, o.textPlain:
		return o.plain, target, 8, true
	case o.textHTML:
		return o.html, target, 8, true
	}
	return nil, 0, 0, false
}

// targetsList returns the atom list advertised under TARGETS.
func (o *x11Owner) targetsList() []byte {
	atoms := []xproto.Atom{o.targets, o.utf8, o.textPlain, o.textHTML}
	out := make([]byte, 4*len(atoms))
	for i, a := range atoms {
		xgb.Put32(out[i*4:], uint32(a))
	}
	return out
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

func (o *x11Owner) writeTargetData(req xproto.SelectionRequestEvent, typ xproto.Atom, format byte, data []byte) {
	if len(data) <= o.maxPropBytes() {
		_ = xproto.ChangePropertyChecked(o.conn, xproto.PropModeReplace, req.Requestor,
			req.Property, typ, format, propItems(len(data), format), data).Check()
		o.sendSelectionNotify(req, req.Property)
		return
	}

	// INCR: announce the transfer size, then stream chunks as the requestor
	// deletes each one (see handlePropertyNotify).
	key := incrKey{req.Requestor, req.Property}
	o.mu.Lock()
	o.transfers[key] = &incrState{typ: typ, data: data}
	o.mu.Unlock()

	// The announcement is a single 32-bit value: format 32, one item.
	var lower [4]byte
	xgb.Put32(lower[:], uint32(len(data)))
	_ = xproto.ChangePropertyChecked(o.conn, xproto.PropModeReplace, req.Requestor,
		req.Property, o.incr, 32, 1, lower[:]).Check()
	// Select PropertyChange events on the requestor window so the owner is
	// notified each time the requestor deletes a chunk. ValueMask selects
	// which window attributes to change (CWEventMask), while the value list
	// carries the event mask itself.
	_ = xproto.ChangeWindowAttributesChecked(o.conn, req.Requestor,
		cwEventMask, []uint32{xproto.EventMaskPropertyChange}).Check()
	o.sendSelectionNotify(req, req.Property)
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
