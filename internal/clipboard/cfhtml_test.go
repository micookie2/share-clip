package clipboard

import (
	"bytes"
	"fmt"
	"testing"
)

func TestHTMLToCFHTMLRoundTrip(t *testing.T) {
	frag := `<span style="color:red">你好</span>`
	block := htmlToCFHTML(frag)

	got, ok := cfhtmlFragment(block)
	if !ok {
		t.Fatal("cfhtmlFragment failed")
	}
	if string(got) != frag {
		t.Fatalf("round trip = %q, want %q", got, frag)
	}
}

func TestCFHTMLFragmentFallbackToDocument(t *testing.T) {
	// A header with only StartHTML/EndHTML (no fragment markers) must yield
	// the document range.
	doc := "<html>hello</html>"
	placeholder := "Version:0.9\r\nStartHTML:0000000000\r\nEndHTML:0000000000\r\n\r\n"
	startHTML := len(placeholder)
	endHTML := startHTML + len(doc)
	header := fmt.Sprintf(
		"Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\n\r\n",
		startHTML, endHTML)
	block := append([]byte(header), doc...)

	got, ok := cfhtmlFragment(block)
	if !ok || string(got) != doc {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

// TestCFHTMLFragmentBrowserShape mimics the shape browsers produce on Windows:
// extra header lines (SourceURL), CRLF line endings and fragment markers.
func TestCFHTMLFragmentBrowserShape(t *testing.T) {
	frag := `<p style="color:red">Hi</p>`
	body := "<html><body><!--StartFragment-->" + frag + "<!--EndFragment--></body></html>"
	// Build the header with a SourceURL line and dummy offsets first to learn
	// the (fixed-width) header length, then fill in the real offsets.
	placeholder := "Version:0.9\r\n" +
		"StartHTML:0000000000\r\n" +
		"EndHTML:0000000000\r\n" +
		"StartFragment:0000000000\r\n" +
		"EndFragment:0000000000\r\n" +
		"SourceURL:https://example.com/page\r\n" +
		"\r\n"
	startHTML := len(placeholder)
	startFragment := startHTML + len("<html><body><!--StartFragment-->")
	endFragment := startFragment + len(frag)
	endHTML := len(placeholder) + len(body)
	header := fmt.Sprintf(
		"Version:0.9\r\n"+
			"StartHTML:%010d\r\n"+
			"EndHTML:%010d\r\n"+
			"StartFragment:%010d\r\n"+
			"EndFragment:%010d\r\n"+
			"SourceURL:https://example.com/page\r\n\r\n",
		startHTML, endHTML, startFragment, endFragment)
	block := append([]byte(header), body...)

	got, ok := cfhtmlFragment(block)
	if !ok || string(got) != frag {
		t.Fatalf("got %q ok=%v, want %q", got, ok, frag)
	}
}

// TestCFHTMLFragmentHeaderOnlyFallback covers a CF_HTML block whose offsets
// are missing entirely: the body after the header must still be recovered.
func TestCFHTMLFragmentHeaderOnlyFallback(t *testing.T) {
	block := []byte("Version:0.9\r\nSourceURL:https://example.com\r\n\r\n<b>x</b>")
	got, ok := cfhtmlFragment(block)
	if !ok || string(got) != "<b>x</b>" {
		t.Fatalf("got %q ok=%v", got, ok)
	}
}

func TestCFHTMLFragmentRejectsNonCFHTML(t *testing.T) {
	if _, ok := cfhtmlFragment([]byte("<b>no header</b>")); ok {
		t.Fatal("bare HTML must not be treated as CF_HTML")
	}
}

func TestHTMLToCFHTMLIsStable(t *testing.T) {
	a := htmlToCFHTML("abc")
	b := htmlToCFHTML("abc")
	if !bytes.Equal(a, b) {
		t.Fatal("same fragment must produce identical bytes")
	}
	if !bytes.HasPrefix(a, []byte("Version:0.9\r\n")) {
		t.Fatalf("missing header: %q", a[:30])
	}
}
