//go:build linux

package clipboard

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jezek/xgb"
	"github.com/jezek/xgb/xproto"
)

// This file covers the ICCCM contract of the native CLIPBOARD selection owner
// (x11owner_linux.go). The contract is not decoration: every real clipboard
// consumer speaks it.
//
//   - TARGETS must advertise TIMESTAMP, MULTIPLE and SAVE_TARGETS alongside the
//     data targets (ICCCM §2.6/2.7 and the SAVE_TARGETS convention). Clipboard
//     managers (CopyQ, klipper, clipman) decide whether an owner is worth
//     saving by reading that list.
//   - A TIMESTAMP conversion must succeed and carry an INTEGER (ICCCM §2.6).
//     Qt/GTK-based applications ask for it before accepting a selection; a
//     refusal makes them treat the clipboard as not offering rich content.
//   - MULTIPLE lets one requestor convert several targets in a single round
//     trip, which is how managers snapshot a copy's renditions together
//     (ICCCM §2.7).
//
// When any of these are missing, a clipboard manager cannot take over the
// selection, so the moment the owning process exits the rich rendition is
// gone: the user's formatted copy turns into plain text or disappears.
//
//	Xvfb :99 -screen 0 1024x768x24 &
//	SHARECLIP_X11_LIVE=1 DISPLAY=:99 go test -run TestX11OwnerICCCM -v ./internal/clipboard/
func TestX11OwnerICCCM(t *testing.T) {
	if os.Getenv("SHARECLIP_X11_LIVE") != "1" {
		t.Skip("set SHARECLIP_X11_LIVE=1 and DISPLAY to run against a live X server")
	}

	const (
		plain = "hello world"
		html  = `<b>hello</b> world`
	)
	owner, err := newX11Owner(plain, html)
	if err != nil {
		t.Fatalf("newX11Owner: %v", err)
	}
	defer owner.close()

	probe, err := newX11Probe()
	if err != nil {
		t.Fatalf("probe connect: %v", err)
	}
	defer probe.close()

	clipboard := probe.atom("CLIPBOARD")

	// 1. TARGETS must list the protocol targets, not only the data targets.
	raw, typ, err := probe.convert(clipboard, probe.atom("TARGETS"))
	if err != nil {
		t.Fatalf("convert TARGETS: %v", err)
	}
	if typ != xproto.AtomAtom {
		t.Fatalf("TARGETS property type = %d, want ATOM", typ)
	}
	offered := make(map[string]bool)
	for i := 0; i+4 <= len(raw); i += 4 {
		offered[probe.atomName(xproto.Atom(xgb.Get32(raw[i:])))] = true
	}
	var missing []string
	for _, name := range []string{"TIMESTAMP", "MULTIPLE", "SAVE_TARGETS",
		"UTF8_STRING", "text/plain", "text/html"} {
		if !offered[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("TARGETS is missing protocol targets %v; a clipboard manager cannot save the selection "+
			"and the rich rendition is lost when this process exits. Offered: %v",
			missing, sortedKeys(offered))
	}

	// 2. TIMESTAMP must convert, as an INTEGER (ICCCM 2.6).
	raw, typ, err = probe.convert(clipboard, probe.atom("TIMESTAMP"))
	if err != nil {
		t.Errorf("convert TIMESTAMP: %v (Qt/GTK applications reject a selection whose owner refuses it)", err)
	} else if typ != xproto.AtomInteger || len(raw) != 4 {
		t.Errorf("TIMESTAMP = type %d len %d, want INTEGER/32 on 4 bytes", typ, len(raw))
	}

	// 3. Plain-text renditions real applications request must all work.
	for _, target := range []string{"UTF8_STRING", "text/plain", "STRING", "TEXT", "text/plain;charset=utf-8"} {
		got, _, err := probe.convert(clipboard, probe.atom(target))
		if err != nil {
			t.Errorf("convert %s: %v", target, err)
			continue
		}
		if !strings.Contains(string(got), "hello") {
			t.Errorf("%s = %q, want the plain rendition", target, got)
		}
	}
	for _, target := range []string{"text/html", "text/html;charset=utf-8"} {
		got, _, err := probe.convert(clipboard, probe.atom(target))
		if err != nil {
			t.Errorf("convert %s: %v", target, err)
			continue
		}
		if string(got) != html {
			t.Errorf("%s = %q, want %q", target, got, html)
		}
	}

	// 4. MULTIPLE: several renditions converted in one request, the way a
	//    clipboard manager snapshots a copy's formats. ICCCM 2.7.2 puts the
	//    (target, property) pairs into the property named by the conversion
	//    request itself, so they are seeded into the probe's conversion
	//    property rather than an unrelated one.
	pairs := [][2]string{{"text/html", "MULTI_HTML"}, {"UTF8_STRING", "MULTI_TEXT"}}
	var pairBytes []byte
	for _, p := range pairs {
		pairBytes = append(pairBytes, make([]byte, 8)...)
		xgb.Put32(pairBytes[len(pairBytes)-8:], uint32(probe.atom(p[0])))
		xgb.Put32(pairBytes[len(pairBytes)-4:], uint32(probe.atom(p[1])))
	}
	multiple := probe.atom("MULTIPLE")
	if _, _, err := probe.convertMulti(clipboard, multiple, pairBytes); err != nil {
		t.Errorf("convert MULTIPLE: %v (clipboard managers snapshot the formats this way)", err)
	} else {
		for _, p := range pairs {
			got, _, err := probe.readProp(probe.atom(p[1]))
			if err != nil || len(got) == 0 {
				t.Errorf("MULTIPLE did not fill %s for %s: %q err=%v", p[1], p[0], got, err)
			}
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// x11Probe is a throwaway X client that speaks ICCCM to the selection owner the
// way a real toolkit or clipboard manager does.
type x11Probe struct {
	conn   *xgb.Conn
	win    xproto.Window
	events chan xproto.SelectionNotifyEvent
	done   chan struct{}
}

func newX11Probe() (*x11Probe, error) {
	conn, err := xgb.NewConn()
	if err != nil {
		return nil, err
	}
	wid, err := conn.NewId()
	if err != nil {
		conn.Close()
		return nil, err
	}
	p := &x11Probe{conn: conn, win: xproto.Window(wid),
		events: make(chan xproto.SelectionNotifyEvent, 8), done: make(chan struct{})}
	root := xproto.Setup(conn).DefaultScreen(conn).Root
	if err := xproto.CreateWindowChecked(conn, 0, p.win, root, 0, 0, 1, 1, 0,
		xproto.WindowClassCopyFromParent, 0, 0, nil).Check(); err != nil {
		conn.Close()
		return nil, err
	}
	go func() {
		defer close(p.done)
		for {
			ev, xerr := conn.WaitForEvent()
			if xerr != nil {
				continue
			}
			if ev == nil {
				return
			}
			if n, ok := ev.(xproto.SelectionNotifyEvent); ok && n.Requestor == p.win {
				select {
				case p.events <- n:
				default:
				}
			}
		}
	}()
	return p, nil
}

func (p *x11Probe) close() {
	p.conn.Close()
	<-p.done
}

func (p *x11Probe) atom(name string) xproto.Atom {
	r, err := xproto.InternAtom(p.conn, false, uint16(len(name)), name).Reply()
	if err != nil || r == nil {
		return 0
	}
	return r.Atom
}

func (p *x11Probe) atomName(a xproto.Atom) string {
	r, err := xproto.GetAtomName(p.conn, a).Reply()
	if err != nil || r == nil {
		return fmt.Sprintf("atom%d", a)
	}
	return string(r.Name)
}

func (p *x11Probe) deleteProp(name xproto.Atom) {
	_ = xproto.DeleteProperty(p.conn, p.win, name).Check()
}

func (p *x11Probe) readProp(name xproto.Atom) ([]byte, xproto.Atom, error) {
	r, err := xproto.GetProperty(p.conn, false, p.win, name, 0, 0, 1<<22).Reply()
	if err != nil {
		return nil, 0, err
	}
	if r.Format == 0 || len(r.Value) == 0 {
		return nil, 0, fmt.Errorf("property %s empty", p.atomName(name))
	}
	return r.Value, r.Type, nil
}

// convert performs one ICCCM conversion and returns the resulting bytes and
// property type. A refusal (SelectionNotify with Property None) is an error.
func (p *x11Probe) convert(sel, target xproto.Atom) ([]byte, xproto.Atom, error) {
	return p.convertMulti(sel, target, nil)
}

// convertMulti is convert for targets that need data seeded into the conversion
// property before the request is issued: MULTIPLE reads its (target, property)
// pair list from exactly that property (ICCCM 2.7.2). A nil seed means "no
// pre-seeded data", which is what every ordinary conversion wants.
func (p *x11Probe) convertMulti(sel, target xproto.Atom, seed []byte) ([]byte, xproto.Atom, error) {
	prop := p.atom("PROBE_PROP")
	p.deleteProp(prop)
	// Drain stale notifications from earlier conversions.
	for {
		select {
		case <-p.events:
			continue
		default:
		}
		break
	}
	if seed != nil {
		if err := xproto.ChangePropertyChecked(p.conn, xproto.PropModeReplace, p.win,
			prop, xproto.AtomAtom, 32, uint32(len(seed)/4), seed).Check(); err != nil {
			return nil, 0, err
		}
	}
	if err := xproto.ConvertSelectionChecked(p.conn, p.win, sel, target, prop,
		xproto.TimeCurrentTime).Check(); err != nil {
		return nil, 0, err
	}
	for {
		select {
		case n := <-p.events:
			if n.Target != target {
				continue
			}
			if n.Property == 0 {
				return nil, 0, fmt.Errorf("owner refused target %s", p.atomName(target))
			}
			return p.readProp(n.Property)
		case <-time.After(3 * time.Second):
			return nil, 0, fmt.Errorf("timeout waiting for SelectionNotify for %s", p.atomName(target))
		}
	}
}
