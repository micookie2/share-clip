package clipboard

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// cfhtmlOffsets holds the four offsets of a CF_HTML header.
type cfhtmlOffsets struct {
	startHTML     int
	endHTML       int
	startFragment int
	endFragment   int
}

// cfhtmlTestOffsets parses and validates the header of block: it must be the
// spec header (Version:0.9, 10-digit zero-padded byte offsets, CRLF lines and a
// blank line) and every offset must be ordered, in range and self-consistent.
func cfhtmlTestOffsets(t *testing.T, block []byte) cfhtmlOffsets {
	t.Helper()
	headerEnd := headerEndOffset(block)
	if headerEnd < 0 {
		t.Fatalf("block has no blank line terminating the header: %q", block)
	}
	header := block[:headerEnd]
	if !bytes.HasPrefix(block, []byte("Version:0.9\r\n")) {
		t.Fatalf("header must start with Version:0.9 CRLF, got %q", block[:20])
	}
	off := cfhtmlOffsets{
		startHTML:     parseHeaderOffset(header, "StartHTML:"),
		endHTML:       parseHeaderOffset(header, "EndHTML:"),
		startFragment: parseHeaderOffset(header, "StartFragment:"),
		endFragment:   parseHeaderOffset(header, "EndFragment:"),
	}
	if off.startHTML < headerEnd || off.endHTML > len(block) || off.startHTML >= off.endHTML {
		t.Fatalf("StartHTML/EndHTML not usable: %+v (block %d bytes, header %d)", off, len(block), headerEnd)
	}
	if off.startFragment < off.startHTML || off.endFragment > off.endHTML || off.startFragment >= off.endFragment {
		t.Fatalf("fragment offsets not inside document: %+v", off)
	}
	for _, kv := range []struct {
		key string
		val int
	}{
		{"StartHTML:", off.startHTML},
		{"EndHTML:", off.endHTML},
		{"StartFragment:", off.startFragment},
		{"EndFragment:", off.endFragment},
	} {
		want := fmt.Sprintf("%s%010d\r\n", kv.key, kv.val)
		if !bytes.Contains(header, []byte(want)) {
			t.Fatalf("header missing zero-padded %q:\n%s", want, header)
		}
	}
	return off
}

func TestHTMLToCFHTMLFragmentRoundTrip(t *testing.T) {
	frag := `<span style="color:red">你好</span>`
	block := htmlToCFHTML(frag)
	off := cfhtmlTestOffsets(t, block)

	if got := string(block[off.startFragment:off.endFragment]); got != frag {
		t.Fatalf("fragment bytes = %q, want %q", got, frag)
	}
	// A bare fragment is wrapped in the standard envelope.
	if !bytes.HasPrefix(block[off.startHTML:], []byte("<html><body>")) ||
		!bytes.HasSuffix(block[off.startHTML:off.endHTML], []byte("</body></html>")) {
		t.Fatalf("fragment not wrapped in the standard envelope: %q", block[off.startHTML:off.endHTML])
	}

	doc, ok := cfhtmlDocument(block)
	if !ok {
		t.Fatal("cfhtmlDocument rejected a generated block")
	}
	if !strings.Contains(doc, frag) {
		t.Fatalf("document %q does not contain the fragment", doc)
	}
	// Legacy fragment reader stays compatible.
	if got, ok := cfhtmlFragment(block); !ok || string(got) != frag {
		t.Fatalf("cfhtmlFragment = %q ok=%v, want %q", got, ok, frag)
	}
}

// excelLikeDocument mimics Excel's "HTML Format" clipboard payload: a full HTML
// document whose formatting context (<style> classes, ProgId/charset <meta>)
// lives outside the fragment markers.
const excelLikeDocument = `<html xmlns:o="urn:schemas-microsoft-com:office:office"
xmlns:x="urn:schemas-microsoft-com:office:excel"
xmlns="http://www.w3.org/TR/REC-html40">
<head>
<meta http-equiv=Content-Type content="text/html; charset=utf-8">
<meta name=ProgId content=Excel.Sheet>
<meta name=Generator content="Microsoft Excel 16">
<style>
<!--table
.xl65 {mso-style-parent:style0;font-weight:700;}
td {mso-number-format:"\@";}
-->
</style>
</head>
<body link="#0563C1">
<!--StartFragment-->
<table border=0 cellpadding=0 cellspacing=0>
 <tr><td class=xl65>Hello</td><td>世界</td></tr>
</table>
<!--EndFragment-->
</body>
</html>
`

func TestHTMLToCFHTMLPreservesExcelDocument(t *testing.T) {
	block := htmlToCFHTML(excelLikeDocument)
	off := cfhtmlTestOffsets(t, block)

	// Every byte of the original document must survive: head, <style>, <meta>,
	// fragment markers and trailing whitespace.
	if got := string(block[off.startHTML:off.endHTML]); got != excelLikeDocument {
		t.Fatalf("document not preserved byte-for-byte:\n got %q\nwant %q", got, excelLikeDocument)
	}
	if off.startHTML != len(block)-len(excelLikeDocument) {
		t.Fatalf("StartHTML = %d, want %d (end of header)", off.startHTML, len(block)-len(excelLikeDocument))
	}
	if off.endHTML != len(block) {
		t.Fatalf("EndHTML = %d, want %d (end of block)", off.endHTML, len(block))
	}
	// The marker positions come from the document itself.
	wantFragStart := strings.Index(excelLikeDocument, "<!--StartFragment-->") + len("<!--StartFragment-->")
	wantFragEnd := strings.Index(excelLikeDocument, "<!--EndFragment-->")
	if got := string(block[off.startFragment:off.endFragment]); got != excelLikeDocument[wantFragStart:wantFragEnd] {
		t.Fatalf("fragment = %q, want %q", got, excelLikeDocument[wantFragStart:wantFragEnd])
	}

	doc, ok := cfhtmlDocument(block)
	if !ok {
		t.Fatal("cfhtmlDocument rejected an Excel-like block")
	}
	if doc != excelLikeDocument {
		t.Fatalf("cfhtmlDocument lost formatting context:\n got %q\nwant %q", doc, excelLikeDocument)
	}
	if !strings.Contains(doc, "<style>") || !strings.Contains(doc, "content=Excel.Sheet") {
		t.Fatal("formatting context (<style>/ProgId meta) must be preserved")
	}
}

func TestHTMLToCFHTMLCJKByteOffsets(t *testing.T) {
	// Offsets in CF_HTML are UTF-8 BYTE offsets, not rune offsets. The CJK text
	// makes the two differ, so a rune-based computation would slice wrongly.
	frag := `<span>你好，世界</span>`
	if len(frag) == len([]rune(frag)) {
		t.Fatal("test fragment must be multi-byte")
	}
	block := htmlToCFHTML(frag)
	off := cfhtmlTestOffsets(t, block)

	headerEnd := headerEndOffset(block)
	wantStart := bytes.Index(block, []byte("<!--StartFragment-->")) + len("<!--StartFragment-->")
	if off.startFragment != wantStart {
		t.Fatalf("StartFragment = %d, want byte offset %d (header ends at %d, %d bytes vs %d runes)",
			off.startFragment, wantStart, headerEnd, len(frag), len([]rune(frag)))
	}
	if got := string(block[off.startFragment:off.endFragment]); got != frag {
		t.Fatalf("multibyte fragment sliced wrongly: %q", got)
	}
	if off.endFragment-off.startFragment != len(frag) {
		t.Fatalf("fragment length = %d bytes, want %d", off.endFragment-off.startFragment, len(frag))
	}
	// A full document with CJK content must round-trip byte-identically too.
	doc := "<html><head><meta charset=utf-8></head><body><!--StartFragment-->" + frag + "<!--EndFragment--></body></html>"
	if got, ok := cfhtmlDocument(htmlToCFHTML(doc)); !ok || got != doc {
		t.Fatalf("CJK document round trip failed: ok=%v got=%q", ok, got)
	}
}

func TestHTMLToCFHTMLInjectsMarkersIntoDocumentWithoutMarkers(t *testing.T) {
	doc := `<!DOCTYPE html><html><head><style>.xl65{font-weight:700}</style></head>` +
		`<body class="sheet"><table><tr><td class=xl65>Hi</td></tr></table></body></html>`
	block := htmlToCFHTML(doc)
	off := cfhtmlTestOffsets(t, block)

	want := strings.Replace(doc, `<body class="sheet">`, `<body class="sheet"><!--StartFragment-->`, 1)
	want = strings.Replace(want, `</body>`, `<!--EndFragment--></body>`, 1)
	if got := string(block[off.startHTML:off.endHTML]); got != want {
		t.Fatalf("markers not injected at body boundaries:\n got %q\nwant %q", got, want)
	}
	// Everything except the injected markers is preserved.
	if got := string(block[off.startFragment:off.endFragment]); got != `<table><tr><td class=xl65>Hi</td></tr></table>` {
		t.Fatalf("fragment = %q", got)
	}
	if !strings.Contains(string(block[off.startHTML:off.endHTML]), "<style>.xl65{font-weight:700}</style>") {
		t.Fatal("<style> context lost while injecting markers")
	}
	docOut, ok := cfhtmlDocument(block)
	if !ok || docOut != want {
		t.Fatalf("cfhtmlDocument = %q ok=%v, want %q", docOut, ok, want)
	}
}

func TestHTMLToCFHTMLNoDocumentNoMarkersWraps(t *testing.T) {
	frag := `<b>just a fragment</b>`
	block := htmlToCFHTML(frag)
	off := cfhtmlTestOffsets(t, block)
	wantDoc := "<html><body><!--StartFragment-->" + frag + "<!--EndFragment--></body></html>"
	if got := string(block[off.startHTML:off.endHTML]); got != wantDoc {
		t.Fatalf("envelope = %q, want %q", got, wantDoc)
	}
}

func TestCFHTMLDocumentFallbacks(t *testing.T) {
	body := "<html><body><!--StartFragment-->frag<!--EndFragment--></body></html>"
	fragStart := strings.Index(body, "<!--StartFragment-->") + len("<!--StartFragment-->")
	fragEnd := strings.Index(body, "<!--EndFragment-->")
	// Header length is fixed (all offsets are 10-digit zero-padded).
	const headerFormat = "Version:0.9\r\nStartHTML:%010d\r\nEndHTML:%010d\r\nStartFragment:%010d\r\nEndFragment:%010d\r\n\r\n"
	headerLen := len(fmt.Sprintf(headerFormat, 0, 0, 0, 0))

	manual := func(startHTML, endHTML, startFragment, endFragment int) []byte {
		header := fmt.Sprintf(headerFormat, startHTML, endHTML, startFragment, endFragment)
		return append([]byte(header), body...)
	}

	t.Run("StartHTML range wins over fragment", func(t *testing.T) {
		block := manual(headerLen, headerLen+len(body), headerLen+fragStart, headerLen+fragEnd)
		got, ok := cfhtmlDocument(block)
		if !ok || got != body {
			t.Fatalf("got %q ok=%v, want the full document", got, ok)
		}
	})

	t.Run("fragment range when StartHTML is unusable", func(t *testing.T) {
		block := manual(9999999999, 0, headerLen+fragStart, headerLen+fragEnd)
		if got, ok := cfhtmlDocument(block); !ok || got != "frag" {
			t.Fatalf("got %q ok=%v, want fragment fallback", got, ok)
		}
	})

	t.Run("body after the header when all offsets are missing", func(t *testing.T) {
		block := []byte("Version:0.9\r\nSourceURL:https://example.com\r\n\r\n<b>x</b>")
		if got, ok := cfhtmlDocument(block); !ok || got != "<b>x</b>" {
			t.Fatalf("got %q ok=%v, want the raw body", got, ok)
		}
	})

	t.Run("body may be empty", func(t *testing.T) {
		if got, ok := cfhtmlDocument([]byte("Version:0.9\r\n\r\n")); !ok || got != "" {
			t.Fatalf("got %q ok=%v, want empty body", got, ok)
		}
	})

	t.Run("non CF_HTML rejected", func(t *testing.T) {
		for _, raw := range [][]byte{
			[]byte("<b>no header</b>"),
			[]byte("Version:0.9\r\nStartHTML:0000000103\r\n"), // no blank line
			[]byte("\r\n\r\n<b>x</b>"),                        // blank line but no header
		} {
			if got, ok := cfhtmlDocument(raw); ok {
				t.Fatalf("cfhtmlDocument(%q) = %q, ok, want false", raw, got)
			}
		}
	})
}

// TestCFHTMLFragmentBrowserShape keeps the legacy fragment reader compatible
// with the shape browsers produce on Windows: extra header lines (SourceURL),
// CRLF line endings and fragment markers.
func TestCFHTMLFragmentBrowserShape(t *testing.T) {
	frag := `<p style="color:red">Hi</p>`
	body := "<html><body><!--StartFragment-->" + frag + "<!--EndFragment--></body></html>"
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
		t.Fatalf("cfhtmlFragment = %q ok=%v, want %q", got, ok, frag)
	}
	// The document reader must return the whole document, SourceURL context
	// included, for the same block.
	if doc, ok := cfhtmlDocument(block); !ok || doc != body {
		t.Fatalf("cfhtmlDocument = %q ok=%v, want the full document", doc, ok)
	}
}

func TestCFHTMLFragmentRejectsNonCFHTML(t *testing.T) {
	if _, ok := cfhtmlFragment([]byte("<b>no header</b>")); ok {
		t.Fatal("bare HTML must not be treated as CF_HTML")
	}
	if _, ok := cfhtmlDocument([]byte("<b>no header</b>")); ok {
		t.Fatal("cfhtmlDocument must reject non-CF_HTML input")
	}
}

func TestHTMLToCFHTMLDeterministic(t *testing.T) {
	inputs := []string{
		`<span>frag</span>`,
		excelLikeDocument, // already has markers
		`<html><body><p>no markers</p></body></html>`,
	}
	for _, in := range inputs {
		first := htmlToCFHTML(in)
		second := htmlToCFHTML(in)
		if !bytes.Equal(first, second) {
			t.Fatalf("htmlToCFHTML(%q) is not deterministic", in)
		}
	}
}
