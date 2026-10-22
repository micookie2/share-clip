package clipboard

// This file holds the platform-neutral half of the Windows clipboard read
// path: the format preference policy and the byte/CF_HTML decoding helpers.
// It deliberately carries no build tag so the policy can be unit tested on
// any OS; the Win32 syscall plumbing (EnumClipboardFormats, OpenClipboard,
// GetClipboardData, ...) stays in clipboard_windows.go. The filename must stay
// free of a *_windows suffix: Go applies an implicit GOOS constraint to such
// names even without a build tag, which would hide this policy from the Linux
// test build.
//
// Policy (text-first): Excel and other Office apps put CF_UNICODETEXT,
// "HTML Format" and a rendered picture (CF_DIB) on the clipboard for a plain
// cell/range copy. The picture is a rendition, not the user's intent, and
// requesting it can even force Excel to render a delayed picture. So usable
// plain text always wins and the image format is then never read. A bitmap is
// only shared when there is no usable text (screenshots, image editors,
// browsers) — which is also why a failed text read falls through to the image
// path instead of aborting the snapshot.

import (
	"bytes"
	"fmt"
	"image/png"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/micookie2/share-clip/internal/dib"
)

// Win32 clipboard format identifiers (winuser.h).
const (
	cfText        = 1  // CF_TEXT
	cfBitmap      = 2  // CF_BITMAP
	cfDIB         = 8  // CF_DIB
	cfUnicodeText = 13 // CF_UNICODETEXT
)

// clipboardSource is what snapshotFromFormats needs to know about the current
// clipboard: which formats the owner advertises (seen, plus the enumeration
// order for diagnostics), the id registered for "HTML Format" (0 when it is
// unavailable) and a read function that copies one format's raw bytes out.
type clipboardSource struct {
	seen       map[uint32]bool
	order      []uint32
	htmlFormat uint32
	read       func(format uint32) ([]byte, error)
}

// snapshotFromFormats applies the text-first policy to the formats advertised
// by a clipboard source. It must be called while the clipboard is open, since
// it reads formats through src.read, and it deliberately returns the raw CF_DIB
// bytes instead of decoding them: decoding and PNG-encoding a large image can be
// expensive and must happen after CloseClipboard (see windowsSnapshotContent),
// so the clipboard is not held open across it.
//
// The caller must pass the returned rawDIB to windowsSnapshotContent once the
// clipboard has been closed.
//
// Per-format tolerance: a failing format never aborts the whole snapshot on
// its own. Only when nothing usable could be read and at least one read
// failed is an error returned.
func snapshotFromFormats(src clipboardSource) (textContent Content, rawDIB []byte, err error) {
	var textReadErr error
	var text string

	if src.seen[cfUnicodeText] {
		raw, err := src.read(cfUnicodeText)
		if err != nil {
			// Keep the error for the "nothing else worked" case, and let the
			// image path below have a chance (some apps advertise a broken
			// text format next to a perfectly good bitmap).
			textReadErr = fmt.Errorf("clipboard: read CF_UNICODETEXT: %w", err)
			warnf("clipboard: %v; trying image formats", textReadErr)
		} else if decoded := utf16BytesToString(raw); decoded != "" && !IsFileArtifact(decoded) {
			text = decoded
		}
	}

	if text != "" {
		// Excel-style copies advertise both a text rendition and a picture.
		// Text wins and CF_DIB is not read at all (requesting it may force the
		// source app to render the delayed picture).
		if src.seen[cfDIB] || src.seen[cfBitmap] {
			warnf("clipboard: text and image formats both offered (%s); sharing text, not the image rendition",
				src.formatList())
		}
		var html string
		if src.htmlFormat != 0 && src.seen[src.htmlFormat] {
			raw, err := src.read(src.htmlFormat)
			if err != nil {
				// Rich text is a bonus; plain text is already usable.
				warnf("clipboard: read HTML Format failed; sharing plain text only: %v", err)
			} else if fragment, ok := htmlFromCFHTML(raw); ok {
				html = fragment
			}
		}
		return Content{Kind: KindText, Text: text, HTML: html}, nil, nil
	}

	if src.seen[cfDIB] {
		raw, err := src.read(cfDIB)
		if err != nil {
			return Content{}, nil, fmt.Errorf("clipboard: read CF_DIB: %w", err)
		}
		// Keep the bytes raw: dib.Decode + png.Encode happen in
		// windowsSnapshotContent, after the clipboard has been closed.
		return Content{}, raw, nil
	}

	if textReadErr != nil {
		return Content{}, nil, textReadErr
	}
	return Content{}, nil, nil
}

// windowsSnapshotContent finishes a Windows snapshot after the clipboard has
// been closed. It converts the raw CF_DIB bytes collected by
// snapshotFromFormats into PNG content, or returns the text content unchanged
// when no bitmap was advertised. Keeping the decode/encode here avoids holding
// the clipboard open across the expensive part of a large image snapshot.
func windowsSnapshotContent(textContent Content, rawDIB []byte) (Content, error) {
	if len(rawDIB) == 0 {
		return textContent, nil
	}
	img, err := dib.Decode(rawDIB)
	if err != nil {
		return Content{}, fmt.Errorf("clipboard: decode dib: %w", err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return Content{}, fmt.Errorf("clipboard: encode png: %w", err)
	}
	return Content{Kind: KindImage, PNG: buf.Bytes()}, nil
}

// formatList renders the advertised formats in enumeration order for
// diagnostics, e.g. `cfText, cfUnicodeText, cfDIB, HTML Format`.
func (src clipboardSource) formatList() string {
	formats := src.order
	if len(formats) == 0 {
		formats = make([]uint32, 0, len(src.seen))
		for format := range src.seen {
			formats = append(formats, format)
		}
		sort.Slice(formats, func(i, j int) bool { return formats[i] < formats[j] })
	}
	names := make([]string, len(formats))
	for i, format := range formats {
		names[i] = clipboardFormatName(format, src.htmlFormat)
	}
	return strings.Join(names, ", ")
}

// clipboardFormatName renders one Win32 clipboard format id in a form a
// support ticket can be read from. htmlFormat is the id registered for
// "HTML Format" (0 when unknown).
func clipboardFormatName(format, htmlFormat uint32) string {
	switch format {
	case cfText:
		return "cfText"
	case cfBitmap:
		return "cfBitmap"
	case cfDIB:
		return "cfDIB"
	case cfUnicodeText:
		return "cfUnicodeText"
	}
	if htmlFormat != 0 && format == htmlFormat {
		return "HTML Format"
	}
	return fmt.Sprintf("format#%d", format)
}

// htmlFromCFHTML extracts the fragment from a CF_HTML block, returning "" when
// the block is empty or not recognisable.
func htmlFromCFHTML(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	frag, ok := cfhtmlFragment(raw)
	if !ok {
		return "", false
	}
	return string(frag), true
}

// utf16BytesToString decodes little-endian UTF-16 clipboard bytes (as stored
// by CF_UNICODETEXT) and trims at the first NUL terminator.
func utf16BytesToString(raw []byte) string {
	if len(raw)%2 != 0 {
		raw = raw[:len(raw)-1]
	}
	n := len(raw) / 2
	u16 := make([]uint16, n)
	for i := 0; i < n; i++ {
		u16[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	// Trim at the first NUL terminator, if any.
	for i, u := range u16 {
		if u == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}

// uint16Bytes encodes UTF-16 code units as little-endian bytes, the layout
// expected for CF_UNICODETEXT.
func uint16Bytes(u16 []uint16) []byte {
	raw := make([]byte, len(u16)*2)
	for i, u := range u16 {
		raw[2*i] = byte(u)
		raw[2*i+1] = byte(u >> 8)
	}
	return raw
}
