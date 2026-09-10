package clipboard

import (
	"bytes"
	"fmt"
)

// This file implements the Microsoft "HTML Format" (CF_HTML) container used
// by the Windows clipboard for rich text. On Windows the clipboard stores
// HTML inside a header block that records byte offsets; on Linux the same
// content travels as a bare text/html target. These helpers convert between
// the two so a rich-text copy survives a Windows<->Linux round trip.

// htmlToCFHTML wraps a bare HTML fragment in the CF_HTML container. The
// fragment is embedded as the StartFragment..EndFragment range, which is what
// applications insert when the user pastes.
func htmlToCFHTML(fragment string) []byte {
	htmlOpen := []byte("<html><body><!--StartFragment-->")
	htmlClose := []byte("<!--EndFragment--></body></html>")
	frag := []byte(fragment)

	// The header is fixed width: every offset is zero-padded to 10 digits and
	// it is terminated by a blank line, so its length never changes with the
	// offset values.
	header := func(startHTML, endHTML, startFragment, endFragment int) []byte {
		return []byte(fmt.Sprintf(
			"Version:0.9\r\n"+
				"StartHTML:%010d\r\n"+
				"EndHTML:%010d\r\n"+
				"StartFragment:%010d\r\n"+
				"EndFragment:%010d\r\n\r\n",
			startHTML, endHTML, startFragment, endFragment))
	}

	placeholder := header(0, 0, 0, 0)
	startHTML := len(placeholder)
	startFragment := startHTML + len(htmlOpen)
	endFragment := startFragment + len(frag)
	endHTML := endFragment + len(htmlClose)

	out := make([]byte, 0, len(placeholder)+len(htmlOpen)+len(frag)+len(htmlClose))
	out = append(out, header(startHTML, endHTML, startFragment, endFragment)...)
	out = append(out, htmlOpen...)
	out = append(out, frag...)
	out = append(out, htmlClose...)
	return out
}

// cfhtmlFragment extracts the HTML fragment (the StartFragment..EndFragment
// range) from a CF_HTML block. It falls back to the StartHTML..EndHTML range
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
	for i < len(header) && header[i] >= '0' && header[i] <= '9' {
		n = n*10 + int(header[i]-'0')
		i++
	}
	return n
}
