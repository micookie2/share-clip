package clipboard

// Tests for the platform-neutral Windows snapshot policy in
// snapshot_windows_policy.go. They run on every OS (no build tag) and use a fake
// format source, so the text-first decision is verified without touching the
// Win32 clipboard: in particular that a CF_DIB rendition is never even read
// when usable plain text is present.
//
// The policy has two steps (raw bytes while the clipboard is open, DIB
// decode/PNG encode after it closed); winSnapshot mirrors winBackend.Snapshot,
// while the image tests also inspect the intermediate rawDIB value to pin the
// split.

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
	"unicode/utf16"

	"github.com/micookie2/share-clip/internal/dib"
)

// winTestHTMLFormat is a stand-in for the id RegisterClipboardFormatW would
// return for "HTML Format" (real ids start at 0xC000).
const winTestHTMLFormat uint32 = 0xC001

// winSource is a fake clipboardSource: it records every format the policy
// tries to read and serves canned bytes or errors.
type winSource struct {
	seen       map[uint32]bool
	order      []uint32
	htmlFormat uint32
	data       map[uint32][]byte
	errs       map[uint32]error
	requested  []uint32
}

func newWinSource(formats ...uint32) *winSource {
	s := &winSource{
		seen: make(map[uint32]bool),
		data: make(map[uint32][]byte),
		errs: make(map[uint32]error),
	}
	for _, f := range formats {
		s.offer(f)
	}
	return s
}

// offer advertises a format, preserving enumeration order.
func (s *winSource) offer(format uint32) *winSource {
	if !s.seen[format] {
		s.seen[format] = true
		s.order = append(s.order, format)
	}
	return s
}

// offerHTML advertises a registered "HTML Format" and remembers its id.
func (s *winSource) offerHTML() *winSource {
	s.htmlFormat = winTestHTMLFormat
	return s.offer(winTestHTMLFormat)
}

func (s *winSource) source() clipboardSource {
	return clipboardSource{
		seen:       s.seen,
		order:      s.order,
		htmlFormat: s.htmlFormat,
		read:       s.read,
	}
}

func (s *winSource) read(format uint32) ([]byte, error) {
	s.requested = append(s.requested, format)
	if err, ok := s.errs[format]; ok {
		return nil, err
	}
	if data, ok := s.data[format]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("winSource: format %d not offered", format)
}

// asked reports whether the policy read the given format.
func (s *winSource) asked(format uint32) bool {
	for _, f := range s.requested {
		if f == format {
			return true
		}
	}
	return false
}

// winWarnLog captures warnf diagnostics (synchronized: warnf is package
// global and may be invoked from any goroutine).
type winWarnLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *winWarnLog) add(format string, args ...any) {
	l.mu.Lock()
	l.msgs = append(l.msgs, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *winWarnLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.msgs...)
}

// winInstallWarnf records warnf output for the duration of the test and
// restores the previous handler afterwards.
func winInstallWarnf(t *testing.T) *winWarnLog {
	t.Helper()
	log := &winWarnLog{}
	prev := warnf
	SetWarnf(log.add)
	t.Cleanup(func() { SetWarnf(prev) })
	return log
}

// winSnapshot mirrors winBackend.Snapshot: decide + read raw bytes while the
// clipboard is "open", then decode/encode the DIB after it closed.
func winSnapshot(t *testing.T, s *winSource) (Content, error) {
	t.Helper()
	textContent, rawDIB, err := snapshotFromFormats(s.source())
	if err != nil {
		return Content{}, err
	}
	return windowsSnapshotContent(textContent, rawDIB)
}

// winOpenSnapshot runs only the clipboard-open step.
func winOpenSnapshot(t *testing.T, s *winSource) (Content, []byte) {
	t.Helper()
	textContent, rawDIB, err := snapshotFromFormats(s.source())
	if err != nil {
		t.Fatalf("snapshotFromFormats (clipboard open): %v", err)
	}
	return textContent, rawDIB
}

// TestSnapshotWindowsExcelLikeTextWinsTextNotPicture is the regression test
// for the Excel ticket: a cell/range copy advertises CF_UNICODETEXT, HTML
// Format and a rendered CF_DIB. The snapshot must be the text (with its HTML)
// and the CF_DIB rendition must never even be requested.
func TestSnapshotWindowsExcelLikeTextWinsTextNotPicture(t *testing.T) {
	log := winInstallWarnf(t)

	s := newWinSource(cfText, cfUnicodeText, cfDIB).offerHTML()
	s.data[cfUnicodeText] = winUTF16Bytes("sheet1!A1 单元格")
	s.data[winTestHTMLFormat] = htmlToCFHTML("<b>sheet1!A1 单元格</b>")
	s.data[cfDIB] = winDIBBytes(t, winTestImage())

	text, rawDIB := winOpenSnapshot(t, s)
	if text.Kind != KindText {
		t.Fatalf("Excel-style copy must be shared as text, got kind %v", text.Kind)
	}
	if text.Text != "sheet1!A1 单元格" {
		t.Fatalf("text = %q", text.Text)
	}
	if text.HTML != "<b>sheet1!A1 单元格</b>" {
		t.Fatalf("HTML = %q", text.HTML)
	}
	if rawDIB != nil {
		t.Fatal("CF_DIB must not be read when usable text wins (would force Excel's rendered picture)")
	}
	if s.asked(cfDIB) {
		t.Fatal("CF_DIB was requested on the text-first path")
	}
	if len(s.requested) != 2 || s.requested[0] != cfUnicodeText || s.requested[1] != winTestHTMLFormat {
		t.Fatalf("requested formats = %v, want [CF_UNICODETEXT HTML Format]", s.requested)
	}

	// The post-close step must pass text through untouched.
	c, err := windowsSnapshotContent(text, rawDIB)
	if err != nil {
		t.Fatalf("windowsSnapshotContent: %v", err)
	}
	if c.Kind != KindText || c.Text != text.Text || c.HTML != text.HTML {
		t.Fatalf("text content changed by the post-close step: %+v", c)
	}

	// One diagnostic line listing the advertised formats.
	msgs := log.all()
	if len(msgs) != 1 {
		t.Fatalf("want exactly one warning, got %d: %v", len(msgs), msgs)
	}
	for _, want := range []string{"cfUnicodeText", "cfDIB", "HTML Format"} {
		if !strings.Contains(msgs[0], want) {
			t.Errorf("warning %q does not mention %q", msgs[0], want)
		}
	}
}

// TestSnapshotWindowsImageOnly pins the two-step split: the clipboard-open step
// returns the DIB bytes undecoded, and only windowsSnapshotContent decodes and
// PNG-encodes them.
func TestSnapshotWindowsImageOnly(t *testing.T) {
	want := winTestImage()
	dibBytes := winDIBBytes(t, want)
	s := newWinSource(cfDIB)
	s.data[cfDIB] = dibBytes

	text, rawDIB := winOpenSnapshot(t, s)
	if !text.Empty() {
		t.Fatalf("no text was offered, got %+v", text)
	}
	if !bytes.Equal(rawDIB, dibBytes) {
		t.Fatalf("clipboard-open step must return the raw DIB undecoded (%d bytes, want %d)", len(rawDIB), len(dibBytes))
	}
	if len(s.requested) != 1 || s.requested[0] != cfDIB {
		t.Fatalf("requested formats = %v, want only CF_DIB", s.requested)
	}

	c, err := windowsSnapshotContent(text, rawDIB)
	if err != nil {
		t.Fatalf("windowsSnapshotContent: %v", err)
	}
	if c.Kind != KindImage || len(c.PNG) == 0 {
		t.Fatalf("want image content, got %+v", c)
	}
	winAssertSamePixels(t, winDecodePNG(t, c.PNG), want)
}

func TestSnapshotWindowsTextOnly(t *testing.T) {
	log := winInstallWarnf(t)
	s := newWinSource(cfUnicodeText)
	s.data[cfUnicodeText] = winUTF16Bytes("hello")

	text, rawDIB := winOpenSnapshot(t, s)
	c, err := windowsSnapshotContent(text, rawDIB)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if c.Kind != KindText || c.Text != "hello" || c.HTML != "" {
		t.Fatalf("got %+v", c)
	}
	if len(s.requested) != 1 || s.requested[0] != cfUnicodeText {
		t.Fatalf("requested formats = %v", s.requested)
	}
	if msgs := log.all(); len(msgs) != 0 {
		t.Fatalf("plain text copy should not warn, got %v", msgs)
	}
}

func TestSnapshotWindowsEmptyTextFallsBackToImage(t *testing.T) {
	want := winTestImage()
	s := newWinSource(cfUnicodeText, cfDIB)
	s.data[cfUnicodeText] = winUTF16Bytes("") // NUL terminator only
	s.data[cfDIB] = winDIBBytes(t, want)

	c, err := winSnapshot(t, s)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if c.Kind != KindImage {
		t.Fatalf("empty text must fall back to the image, got kind %v", c.Kind)
	}
	winAssertSamePixels(t, winDecodePNG(t, c.PNG), want)
	if !s.asked(cfUnicodeText) || !s.asked(cfDIB) {
		t.Fatalf("requested formats = %v", s.requested)
	}
}

func TestSnapshotWindowsFileArtifactTextOnly(t *testing.T) {
	log := winInstallWarnf(t)
	s := newWinSource(cfUnicodeText)
	s.data[cfUnicodeText] = winUTF16Bytes("file:///C:/Users/me/report.xlsx")

	c, err := winSnapshot(t, s)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !c.Empty() {
		t.Fatalf("file copy must not be shared, got %+v", c)
	}
	if msgs := log.all(); len(msgs) != 0 {
		t.Fatalf("file artifact should not warn, got %v", msgs)
	}
}

// TestSnapshotWindowsHTMLOnlyFallsBackToImage pins that plain text is the
// criterion: "HTML Format" alone (no CF_UNICODETEXT) is not usable text and
// must not be read, so the CF_DIB rendition is shared instead.
func TestSnapshotWindowsHTMLOnlyFallsBackToImage(t *testing.T) {
	log := winInstallWarnf(t)
	want := winTestImage()
	s := newWinSource(cfDIB).offerHTML()
	s.data[winTestHTMLFormat] = htmlToCFHTML("<b>html only, no plain text</b>")
	s.data[cfDIB] = winDIBBytes(t, want)

	text, rawDIB := winOpenSnapshot(t, s)
	if !text.Empty() {
		t.Fatalf("HTML alone must not be treated as usable plain text, got %+v", text)
	}
	if len(rawDIB) == 0 {
		t.Fatal("CF_DIB rendition must be read when there is no usable plain text")
	}
	if s.asked(winTestHTMLFormat) {
		t.Fatal("HTML Format must not be read when there is no usable plain text")
	}

	c, err := windowsSnapshotContent(text, rawDIB)
	if err != nil {
		t.Fatalf("windowsSnapshotContent: %v", err)
	}
	if c.Kind != KindImage {
		t.Fatalf("want image fallback, got kind %v", c.Kind)
	}
	winAssertSamePixels(t, winDecodePNG(t, c.PNG), want)
	if msgs := log.all(); len(msgs) != 0 {
		t.Fatalf("no text-wins diagnostic expected, got %v", msgs)
	}
}

// TestSnapshotWindowsBitmapOnlyIsEmpty locks in the edge case where only
// CF_BITMAP (a GDI handle, not readable as bytes) is offered: there is nothing
// decodable and no text, so the snapshot stays empty.
func TestSnapshotWindowsBitmapOnlyIsEmpty(t *testing.T) {
	log := winInstallWarnf(t)
	s := newWinSource(cfBitmap)

	text, rawDIB := winOpenSnapshot(t, s)
	if !text.Empty() || rawDIB != nil {
		t.Fatalf("CF_BITMAP-only clipboard must be empty, got text=%+v rawDIB=%d bytes", text, len(rawDIB))
	}
	if len(s.requested) != 0 {
		t.Fatalf("nothing readable was offered, requested formats = %v", s.requested)
	}
	c, err := windowsSnapshotContent(text, rawDIB)
	if err != nil || !c.Empty() {
		t.Fatalf("want empty content without error, got %+v err=%v", c, err)
	}
	if msgs := log.all(); len(msgs) != 0 {
		t.Fatalf("nothing failed, no warning expected, got %v", msgs)
	}
}

func TestSnapshotWindowsTextReadErrorFallsBackToImage(t *testing.T) {
	log := winInstallWarnf(t)
	want := winTestImage()
	s := newWinSource(cfUnicodeText, cfDIB)
	s.errs[cfUnicodeText] = errors.New("text boom")
	s.data[cfDIB] = winDIBBytes(t, want)

	c, err := winSnapshot(t, s)
	if err != nil {
		t.Fatalf("one unreadable format must not abort the snapshot: %v", err)
	}
	if c.Kind != KindImage {
		t.Fatalf("want image fallback, got kind %v", c.Kind)
	}
	winAssertSamePixels(t, winDecodePNG(t, c.PNG), want)
	msgs := log.all()
	if len(msgs) == 0 || !strings.Contains(msgs[0], "CF_UNICODETEXT") {
		t.Fatalf("failed text read must be reported, got %v", msgs)
	}
}

func TestSnapshotWindowsTextReadErrorNoImage(t *testing.T) {
	log := winInstallWarnf(t)
	s := newWinSource(cfUnicodeText)
	s.errs[cfUnicodeText] = errors.New("text boom")

	_, err := winSnapshot(t, s)
	if err == nil {
		t.Fatal("want an error when the only readable format failed")
	}
	if !strings.Contains(err.Error(), "CF_UNICODETEXT") {
		t.Fatalf("error %q should name the failed format", err)
	}
	if msgs := log.all(); len(msgs) == 0 {
		t.Fatal("failed text read must be reported")
	}
}

// TestSnapshotWindowsUnreadableDIB proves the decode happens after the
// clipboard-open step: reading succeeds (raw bytes are returned), and only
// windowsSnapshotContent reports the undecodable bitmap.
func TestSnapshotWindowsUnreadableDIB(t *testing.T) {
	s := newWinSource(cfDIB)
	s.data[cfDIB] = []byte("this is not a device independent bitmap")

	text, rawDIB := winOpenSnapshot(t, s)
	if !text.Empty() {
		t.Fatalf("no text expected, got %+v", text)
	}
	if len(rawDIB) == 0 {
		t.Fatal("clipboard-open step must return the raw bytes and defer the decode")
	}

	_, err := windowsSnapshotContent(text, rawDIB)
	if err == nil {
		t.Fatal("want an error for an undecodable DIB without usable text")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "dib") {
		t.Fatalf("error %q should mention dib", err)
	}
}

func TestSnapshotWindowsHTMLReadErrorTolerated(t *testing.T) {
	log := winInstallWarnf(t)
	s := newWinSource(cfUnicodeText).offerHTML()
	s.data[cfUnicodeText] = winUTF16Bytes("plain")
	s.errs[winTestHTMLFormat] = errors.New("html boom")

	c, err := winSnapshot(t, s)
	if err != nil {
		t.Fatalf("HTML read failure must be tolerated: %v", err)
	}
	if c.Kind != KindText || c.Text != "plain" || c.HTML != "" {
		t.Fatalf("got %+v", c)
	}
	var sawHTMLWarn bool
	for _, msg := range log.all() {
		if strings.Contains(msg, "HTML") {
			sawHTMLWarn = true
		}
	}
	if !sawHTMLWarn {
		t.Fatalf("HTML read failure must be logged, got %v", log.all())
	}
}

func TestClipboardFormatName(t *testing.T) {
	cases := []struct {
		format uint32
		want   string
	}{
		{cfText, "cfText"},
		{cfBitmap, "cfBitmap"},
		{cfDIB, "cfDIB"},
		{cfUnicodeText, "cfUnicodeText"},
		{winTestHTMLFormat, "HTML Format"},
		{0xC002, "format#49154"},
	}
	for _, c := range cases {
		if got := clipboardFormatName(c.format, winTestHTMLFormat); got != c.want {
			t.Errorf("clipboardFormatName(%d) = %q, want %q", c.format, got, c.want)
		}
	}
	// Without a registered HTML format id the same value is just a number.
	if got := clipboardFormatName(winTestHTMLFormat, 0); got != "format#49153" {
		t.Errorf("clipboardFormatName with no HTML format = %q", got)
	}
}

// --- helpers ----------------------------------------------------------------

// winTestImage returns a small fully opaque picture that survives a
// DIB round-trip unchanged (dib.Encode forces alpha to 255).
func winTestImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{G: 255, A: 255})
	img.Set(2, 0, color.RGBA{B: 255, A: 255})
	img.Set(0, 1, color.RGBA{R: 255, G: 255, A: 255})
	img.Set(1, 1, color.RGBA{G: 255, B: 255, A: 255})
	img.Set(2, 1, color.RGBA{R: 255, B: 255, A: 255})
	return img
}

func winDIBBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	data, err := dib.Encode(img)
	if err != nil {
		t.Fatalf("dib.Encode: %v", err)
	}
	return data
}

func winDecodePNG(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}
	return img
}

func winAssertSamePixels(t *testing.T, got, want image.Image) {
	t.Helper()
	if got.Bounds() != want.Bounds() {
		t.Fatalf("bounds = %v, want %v", got.Bounds(), want.Bounds())
	}
	b := want.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			gr, gg, gb, ga := got.At(x, y).RGBA()
			wr, wg, wb, wa := want.At(x, y).RGBA()
			if gr != wr || gg != wg || gb != wb || ga != wa {
				t.Fatalf("pixel (%d,%d) = (%d,%d,%d,%d), want (%d,%d,%d,%d)",
					x, y, gr, gg, gb, ga, wr, wg, wb, wa)
			}
		}
	}
}

// winUTF16Bytes encodes s as NUL-terminated little-endian UTF-16, the layout
// of CF_UNICODETEXT (NUL terminator included on purpose).
func winUTF16Bytes(s string) []byte {
	return uint16Bytes(append(utf16.Encode([]rune(s)), 0))
}
