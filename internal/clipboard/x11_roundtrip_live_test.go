//go:build linux

package clipboard

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestX11RichTextRoundTripLive checks the property that matters to the user:
// what share-clip WRITES to the local clipboard must survive being READ BACK by
// share-clip itself (the ApplyRemote write is followed by a Snapshot for echo
// suppression, and a peer that copies the result must be able to re-share it).
//
// Writing rich text goes through the native X11 selection owner, which serves a
// target set; reading goes through the format-aware snapshot with its own
// preference lists (textFormats / htmlFormats in clipboard_linux.go). The two
// must agree on the target names, otherwise the HTML is silently dropped and a
// rich copy degrades to plain text.
//
//	Xvfb :99 -screen 0 1024x768x24 &
//	SHARECLIP_X11_LIVE=1 DISPLAY=:99 go test -run TestX11RichTextRoundTripLive -v ./internal/clipboard/
func TestX11RichTextRoundTripLive(t *testing.T) {
	if os.Getenv("SHARECLIP_X11_LIVE") != "1" {
		t.Skip("set SHARECLIP_X11_LIVE=1 and DISPLAY to run against a live X server")
	}

	want := Content{
		Kind: KindText,
		Text: "banded cells",
		HTML: `<html><head><meta name=ProgId content=Excel.Sheet>` +
			`<style>.xl65{background:#DDEBF7}</style></head>` +
			`<body><!--StartFragment--><table class=xl65><tr><td>banded cells` +
			`</td></tr></table><!--EndFragment--></body></html>`,
	}

	b, err := NewBackend(0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if err := b.Write(want); err != nil {
		t.Fatalf("Write rich text: %v", err)
	}
	// Give the owner's first event loop iteration time to settle (the write
	// path installs the owner synchronously, this only guards the X round trip).
	time.Sleep(200 * time.Millisecond)

	got, err := b.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got.Kind != KindText {
		t.Fatalf("round trip kind = %v, want text (rich text was written)", got.Kind)
	}
	if got.Text != want.Text {
		t.Errorf("plain rendition round trip = %q, want %q", firstLine(got.Text), firstLine(want.Text))
	}
	if got.HTML == "" {
		t.Fatalf("HTML rendition LOST on round trip: wrote a rich copy, read back plain only. "+
			"The write path serves text/html but the read path asked for one of %v",
			htmlFormats)
	}
	// The receiving application needs the formatting context, not just the
	// fragment: <style>/<meta> must survive so the paste keeps its formatting.
	for _, must := range []string{"<style>", "xl65", "StartFragment"} {
		if !strings.Contains(got.HTML, must) {
			t.Errorf("HTML renditon lost %q on round trip; got %d bytes", must, len(got.HTML))
		}
	}
	if got.Digest() == ([32]byte{}) {
		t.Error("digest is zero")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestX11KeepsLocalRichOverPlainDowngradeLive proves the downgrade guard on a
// real X server: the rich copy already on the clipboard must survive a peer's
// plain-text-only rendition of the same text. This is the "share-clip turned my
// formatted copy into plain text" symptom at the clipboard boundary.
//
//	Xvfb :99 -screen 0 1024x768x24 &
//	SHARECLIP_X11_LIVE=1 DISPLAY=:99 go test -run TestX11KeepsLocalRichOverPlainDowngradeLive -v ./internal/clipboard/
func TestX11KeepsLocalRichOverPlainDowngradeLive(t *testing.T) {
	if os.Getenv("SHARECLIP_X11_LIVE") != "1" {
		t.Skip("set SHARECLIP_X11_LIVE=1 and DISPLAY to run against a live X server")
	}

	const plain, html = "hello world", "<b>hello</b> world"
	rich := Content{Kind: KindText, Text: plain, HTML: html}

	b, err := NewBackend(0)
	if err != nil {
		t.Fatalf("NewBackend: %v", err)
	}
	if err := b.Write(rich); err != nil {
		t.Fatalf("Write rich: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	w := NewWatcher(b, func(Content) {})
	err = w.ApplyRemote(Content{Kind: KindText, Text: plain})
	if !errors.Is(err, ErrKeptLocalRich) {
		t.Fatalf("ApplyRemote(plain downgrade) = %v, want ErrKeptLocalRich", err)
	}

	got, err := b.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if got.HTML != html {
		t.Errorf("local rich text was downgraded: html=%q want %q", got.HTML, html)
	}
}
