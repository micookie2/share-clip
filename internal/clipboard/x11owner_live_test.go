//go:build linux

package clipboard

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestX11OwnerLive exercises the native CLIPBOARD selection owner against a
// real X server. It is skipped unless SHARECLIP_X11_LIVE=1 so the ordinary
// suite (and CI without a display) stays green. Run it against a virtual X
// server like this:
//
//	Xvfb :99 -screen 0 1024x768x24 &
//	SHARECLIP_X11_LIVE=1 DISPLAY=:99 go test -run TestX11OwnerLive -v ./internal/clipboard/
func TestX11OwnerLive(t *testing.T) {
	if os.Getenv("SHARECLIP_X11_LIVE") != "1" {
		t.Skip("set SHARECLIP_X11_LIVE=1 and DISPLAY to run against a live X server")
	}

	read := func(target string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "xclip", "-selection", "clipboard", "-t", target, "-o").Output()
		return string(out), err
	}

	// Small rich text: served with a single ChangeProperty request.
	plain := "hello world"
	html := `<b>hello</b> world`
	o, err := newX11Owner(plain, html)
	if err != nil {
		t.Fatalf("newX11Owner: %v", err)
	}
	defer o.close()
	time.Sleep(150 * time.Millisecond)

	targets, err := read("TARGETS")
	if err != nil {
		t.Fatalf("read TARGETS: %v", err)
	}
	t.Logf("TARGETS = %q", targets)
	if !strings.Contains(targets, "text/html") {
		t.Errorf("TARGETS does not advertise text/html: %q", targets)
	}
	if !strings.Contains(targets, "UTF8_STRING") {
		t.Errorf("TARGETS does not advertise UTF8_STRING: %q", targets)
	}

	if got, err := read("text/html"); err != nil || got != html {
		t.Errorf("text/html = %q err=%v, want %q", got, err, html)
	}
	if got, err := read("UTF8_STRING"); err != nil || got != plain {
		t.Errorf("UTF8_STRING = %q err=%v, want %q", got, err, plain)
	}
	if got, err := read("text/plain"); err != nil || got != plain {
		t.Errorf("text/plain = %q err=%v, want %q", got, err, plain)
	}

	// Large HTML: forces the INCR streaming path.
	big := strings.Repeat("<p>0123456789abcdef</p>", 30000) // ~660 KB
	o2, err := newX11Owner("plain", big)
	if err != nil {
		t.Fatalf("newX11Owner big: %v", err)
	}
	defer o2.close()
	time.Sleep(150 * time.Millisecond)
	got, err := read("text/html")
	if err != nil {
		t.Fatalf("read big text/html: %v", err)
	}
	if got != big {
		t.Errorf("big text/html round trip mismatch: got %d bytes, want %d", len(got), len(big))
	}
}
