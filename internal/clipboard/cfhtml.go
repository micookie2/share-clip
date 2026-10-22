package clipboard

import (
	"bytes"
	"fmt"
)

// This file implements the Microsoft "HTML Format" (CF_HTML) container used by
// the Windows clipboard for rich text, following
// https://learn.microsoft.com/en-us/windows/win32/dataxchg/html-clipboard-format
// and observing Excel's shape of the format (a full document with the real
// formatting context outside the fragment markers).
//
// On Windows the clipboard stores HTML inside a header block that records
// byte offsets; on Linux the same content travels as a bare text/html target.
// These helpers convert between the two so a rich-text copy survives a
// Windows<->Linux round trip.
//
// The important detail is that a CF_HTML block is a FULL HTML document: Excel,
// Word and browsers put <style> rules (classes such as .xl65), <meta
// name=ProgId content=Excel.Sheet> and <meta charset> into the document head,
// and only the visible content sits between <!--StartFragment--> and
// <!--EndFragment-->. Everything outside the markers is context the receiving
// application needs to resolve formatting, so it must be transported, not just
// the fragment. htmlToCFHTML and cfhtmlDocument therefore preserve whole
// documents; cfhtmlFragment remains for callers that genuinely want only the
// fragment range.

const (
	// cfhtmlVersion is the only version the HTML Clipboard Format defines.
	cfhtmlVersion = "Version:0.9"
)

var (
	cfhtmlStartMarker = []byte("<!--StartFragment-->")
	cfhtmlEndMarker   = []byte("<!--EndFragment-->")

	// Envelope used when the input is a bare fragment with no document markup.
	cfhtmlEnvelopeOpen  = []byte("<html><body>")
	cfhtmlEnvelopeClose = []byte("</body></html>")
)

// htmlToCFHTML encodes HTML as a spec-compliant CF_HTML block. It accepts any
// of the three shapes a rich-text copy can have:
//   - a full document that already carries StartFragment/EndFragment markers
//     (the Excel/browser shape): the document is reused byte-for-byte and only
//     the header offsets are recomputed, so <style>/<meta> context survives;
//   - a full document without markers: the markers are injected at the body
//     content boundaries and the rest of the document is left untouched;
//   - a bare fragment: it is wrapped in <html><body>...</body></html>.
//
// The header follows the specification: Version:0.9, StartHTML, EndHTML,
// StartFragment and EndFragment as 10-digit zero-padded UTF-8 BYTE offsets from
// the start of the block, CRLF line endings and a blank line before the HTML.
func htmlToCFHTML(html string) []byte {
	doc, startFragment, endFragment := cfhtmlEnsureMarkers([]byte(html))

	// The header is fixed width: every offset is zero-padded to 10 digits and
	// it is terminated by a blank line, so its length never depends on the
	// offset values.
	headerLen := len(cfhtmlHeader(0, 0, 0, 0))
	startHTML := 0
	if i := cfhtmlHTMLTagOffset(doc); i >= 0 {
		// Prefer the <html> tag when it opens the document content; otherwise
		// StartHTML points at the beginning of the document block.
		startHTML = i
	}
	endHTML := len(doc)

	out := make([]byte, 0, headerLen+len(doc))
	out = append(out, cfhtmlHeader(headerLen+startHTML, headerLen+endHTML,
		headerLen+startFragment, headerLen+endFragment)...)
	out = append(out, doc...)
	return out
}

// cfhtmlHeader builds the fixed-width CF_HTML header. All offsets are byte
// offsets from the start of the block and are zero-padded to 10 digits so the
// header length is stable.
func cfhtmlHeader(startHTML, endHTML, startFragment, endFragment int) []byte {
	return []byte(fmt.Sprintf(
		"%s\r\n"+
			"StartHTML:%010d\r\n"+
			"EndHTML:%010d\r\n"+
			"StartFragment:%010d\r\n"+
			"EndFragment:%010d\r\n\r\n",
		cfhtmlVersion, startHTML, endHTML, startFragment, endFragment))
}

// cfhtmlEnsureMarkers returns the HTML document that should be stored in the
// block plus the byte offsets of the fragment inside it. An input that already
// carries both markers is returned unchanged (the caller only recomputes the
// header offsets); a document without markers gets them injected at its body
// boundaries; anything else is treated as a bare fragment and wrapped.
func cfhtmlEnsureMarkers(doc []byte) (out []byte, startFragment, endFragment int) {
	if s, e, ok := cfhtmlLocateFragment(doc); ok {
		return doc, s, e
	}
	if cfhtmlLooksLikeDocument(doc) {
		return cfhtmlInjectMarkers(doc)
	}

	out = make([]byte, 0, len(cfhtmlEnvelopeOpen)+len(cfhtmlStartMarker)+len(doc)+
		len(cfhtmlEndMarker)+len(cfhtmlEnvelopeClose))
	out = append(out, cfhtmlEnvelopeOpen...)
	out = append(out, cfhtmlStartMarker...)
	startFragment = len(out)
	out = append(out, doc...)
	endFragment = len(out)
	out = append(out, cfhtmlEndMarker...)
	out = append(out, cfhtmlEnvelopeClose...)
	return out, startFragment, endFragment
}

// cfhtmlInjectMarkers inserts the fragment markers around the body content of a
// full HTML document, leaving every other byte (head, <style>, <meta>, ...) in
// place. Missing <body>/</body> boundaries degrade gracefully to the <html>
// tag or the ends of the document.
func cfhtmlInjectMarkers(doc []byte) (out []byte, startFragment, endFragment int) {
	start := 0
	if i := cfhtmlIndexFold(doc, []byte("<body")); i >= 0 {
		if j := bytes.IndexByte(doc[i:], '>'); j >= 0 {
			start = i + j + 1
		}
	} else if i := cfhtmlIndexFold(doc, []byte("<html")); i >= 0 {
		if j := bytes.IndexByte(doc[i:], '>'); j >= 0 {
			start = i + j + 1
		}
	}

	end := len(doc)
	if i := cfhtmlIndexFoldLast(doc, []byte("</body")); i >= 0 {
		end = i
	} else if i := cfhtmlIndexFoldLast(doc, []byte("</html")); i >= 0 {
		end = i
	}
	if end < start {
		end = len(doc)
	}

	out = make([]byte, 0, len(doc)+len(cfhtmlStartMarker)+len(cfhtmlEndMarker))
	out = append(out, doc[:start]...)
	out = append(out, cfhtmlStartMarker...)
	startFragment = len(out)
	out = append(out, doc[start:end]...)
	endFragment = len(out)
	out = append(out, cfhtmlEndMarker...)
	out = append(out, doc[end:]...)
	return out, startFragment, endFragment
}

// cfhtmlLooksLikeDocument reports whether doc looks like a complete HTML
// document (rather than a fragment). Only the opening markup is inspected.
func cfhtmlLooksLikeDocument(doc []byte) bool {
	trimmed := bytes.TrimLeft(doc, " \t\r\n")
	if len(trimmed) >= 5 && bytes.EqualFold(trimmed[:5], []byte("<html")) {
		return true
	}
	return len(trimmed) >= 9 && bytes.EqualFold(trimmed[:9], []byte("<!doctype"))
}

// cfhtmlHTMLTagOffset returns the byte offset of the document's <html tag when
// it starts the document content (only leading whitespace may precede it), or
// -1 when the document starts with something else (e.g. a doctype), in which
// case the beginning of the document block is the better StartHTML.
func cfhtmlHTMLTagOffset(doc []byte) int {
	trimmed := bytes.TrimLeft(doc, " \t\r\n")
	if len(trimmed) >= 5 && bytes.EqualFold(trimmed[:5], []byte("<html")) {
		return len(doc) - len(trimmed)
	}
	return -1
}

// cfhtmlLocateFragment finds the <!--StartFragment--> and <!--EndFragment-->
// markers and returns the byte range of the fragment itself: the position just
// after the start comment and the position where the end comment begins.
func cfhtmlLocateFragment(doc []byte) (startFragment, endFragment int, ok bool) {
	_, startEnd, startOK := cfhtmlFindComment(doc, "StartFragment")
	endAt, _, endOK := cfhtmlFindComment(doc, "EndFragment")
	if !startOK || !endOK || endAt < startEnd {
		return 0, 0, false
	}
	return startEnd, endAt, true
}

// cfhtmlFindComment finds an HTML comment whose trimmed body equals name
// (case-insensitively) and returns the offsets of the comment start and of the
// position just past "-->" so callers can slice around it.
func cfhtmlFindComment(doc []byte, name string) (commentStart, commentEnd int, ok bool) {
	for i := 0; i < len(doc); {
		j := bytes.Index(doc[i:], []byte("<!--"))
		if j < 0 {
			return 0, 0, false
		}
		j += i
		k := bytes.Index(doc[j+4:], []byte("-->"))
		if k < 0 {
			return 0, 0, false
		}
		k += j + 4
		if bytes.EqualFold(bytes.TrimSpace(doc[j+4:k]), []byte(name)) {
			return j, k + 3, true
		}
		i = k + 3
	}
	return 0, 0, false
}

// cfhtmlIndexFold is bytes.Index with ASCII case folding.
func cfhtmlIndexFold(haystack, needle []byte) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if bytes.EqualFold(haystack[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// cfhtmlIndexFoldLast is cfhtmlIndexFold for the last occurrence.
func cfhtmlIndexFoldLast(haystack, needle []byte) int {
	for i := len(haystack) - len(needle); i >= 0; i-- {
		if bytes.EqualFold(haystack[i:i+len(needle)], needle) {
			return i
		}
	}
	return -1
}

// cfhtmlDocument extracts the FULL HTML document from a CF_HTML block: the
// StartHTML..EndHTML range when the header is parsable and in range, the
// StartFragment..EndFragment range as a fallback, and finally the body after
// the header. Keeping the whole document (not just the fragment) is what lets
// the receiving side resolve <style>/<meta> formatting context. It reports
// false when data is not a recognisable CF_HTML block.
func cfhtmlDocument(raw []byte) (string, bool) {
	headerEnd := headerEndOffset(raw)
	if headerEnd < 0 {
		return "", false
	}
	header := raw[:headerEnd]
	if cfhtmlIndexFold(header, []byte("version:")) < 0 {
		return "", false
	}
	if start, end, ok := cfhtmlHeaderRange(header, raw, headerEnd, "StartHTML:", "EndHTML:"); ok {
		return string(raw[start:end]), true
	}
	if start, end, ok := cfhtmlHeaderRange(header, raw, headerEnd, "StartFragment:", "EndFragment:"); ok {
		return string(raw[start:end]), true
	}
	// A recognisable header with unusable offsets: fall back to the raw body
	// after the blank line (some apps and hand-rolled CF_HTML omit them).
	return string(raw[headerEnd:]), true
}

// cfhtmlHeaderRange parses a start/end offset pair from header and validates it
// against the block: both must lie in the HTML body (after the blank line) and
// be ordered.
func cfhtmlHeaderRange(header, raw []byte, headerEnd int, startKey, endKey string) (start, end int, ok bool) {
	start = parseHeaderOffset(header, startKey)
	end = parseHeaderOffset(header, endKey)
	if start < headerEnd || end <= start || end > len(raw) {
		return 0, 0, false
	}
	return start, end, true
}

// cfhtmlFragment extracts only the HTML fragment (the StartFragment..EndFragment
// range) from a CF_HTML block. It is the legacy fragment-only reader; new code
// that wants the full document (and the formatting context it carries) should
// use cfhtmlDocument instead. It falls back to the StartHTML..EndHTML range
// when the fragment markers are absent, and to the body after the header when
// the offsets are unusable. It reports false when data is not a recognisable
// CF_HTML block.
func cfhtmlFragment(data []byte) ([]byte, bool) {
	headerEnd := headerEndOffset(data)
	if headerEnd < 0 {
		return nil, false
	}
	header := data[:headerEnd]
	start := parseHeaderOffset(header, "StartFragment:")
	end := parseHeaderOffset(header, "EndFragment:")
	if start < 0 || end <= start {
		start = parseHeaderOffset(header, "StartHTML:")
		end = parseHeaderOffset(header, "EndHTML:")
	}
	if start < 0 || end <= start || start > len(data) || end > len(data) {
		// The header is recognisable but its offsets are unusable: some apps
		// (and hand-rolled CF_HTML) omit or mangle them. Use the raw body.
		if bytes.Contains(header, []byte("Version:")) {
			return data[headerEnd:], true
		}
		return nil, false
	}
	return data[start:end], true
}

// headerEndOffset returns the offset just past the CF_HTML header (the blank
// line separating the header from the HTML body), or -1 when there is none.
func headerEndOffset(data []byte) int {
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		return i + 4
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return i + 2
	}
	return -1
}

// parseHeaderOffset finds "key" inside the header and parses the decimal
// offset that follows it, or returns -1.
func parseHeaderOffset(header []byte, key string) int {
	i := bytes.Index(header, []byte(key))
	if i < 0 {
		return -1
	}
	i += len(key)
	n := 0
	started := false
	for i < len(header) && header[i] >= '0' && header[i] <= '9' {
		n = n*10 + int(header[i]-'0')
		i++
		started = true
	}
	if !started {
		return -1
	}
	return n
}
