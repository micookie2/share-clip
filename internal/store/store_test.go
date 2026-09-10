package store

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/png"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T, limit int) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"), limit)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAddAndRecent(t *testing.T) {
	s := openTest(t, 500)
	id, stored, err := s.Add(KindText, "text/plain", "a", "host-a", []byte("hello 你好"))
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if !stored {
		t.Fatal("first Add reported as duplicate")
	}
	if id != 1 {
		t.Fatalf("id = %d", id)
	}
	if _, _, err := s.Add(KindImage, "image/png", "b", "host-b", bytes.Repeat([]byte{1, 2, 3}, 100)); err != nil {
		t.Fatalf("Add image: %v", err)
	}

	items, err := s.Recent(50, 0)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("len = %d", len(items))
	}
	// Newest first: the image entry.
	if items[0].Kind != KindImage || items[0].Size != 300 || items[0].SourceName != "host-b" {
		t.Fatalf("items[0] = %+v", items[0])
	}
	if items[1].Kind != KindText || items[1].TextPreview != "hello 你好" {
		t.Fatalf("items[1] = %+v", items[1])
	}
}

func TestContent(t *testing.T) {
	s := openTest(t, 500)
	payload := []byte{0x89, 'P', 'N', 'G', 1, 2, 3}
	id, _, err := s.Add(KindImage, "image/png", "a", "h", payload)
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	e, got, err := s.Content(id)
	if err != nil {
		t.Fatalf("Content: %v", err)
	}
	if !bytes.Equal(got, payload) || e.Kind != KindImage {
		t.Fatalf("got %v (%s)", got, e.Kind)
	}
	if _, _, err := s.Content(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPruneBeyondLimit(t *testing.T) {
	s := openTest(t, 3)
	for i := 0; i < 10; i++ {
		if _, _, err := s.Add(KindText, "text/plain", "a", "h", []byte("x"+string(rune('0'+i)))); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	n, err := s.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Fatalf("count = %d, want 3", n)
	}
	items, _ := s.Recent(50, 0)
	if items[0].ID != 10 || items[2].ID != 8 {
		t.Fatalf("kept wrong entries: %d..%d", items[2].ID, items[0].ID)
	}
}

func TestPagination(t *testing.T) {
	s := openTest(t, 500)
	for i := 0; i < 5; i++ {
		if _, _, err := s.Add(KindText, "text/plain", "a", "h", []byte{byte('a' + i)}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	page1, _ := s.Recent(2, 0) // ids 5,4
	page2, _ := s.Recent(2, 2) // ids 3,2
	page3, _ := s.Recent(2, 4) // id 1
	if page1[0].ID != 5 || page2[0].ID != 3 || len(page3) != 1 || page3[0].ID != 1 {
		t.Fatalf("pagination wrong: %+v %+v %+v", page1, page2, page3)
	}
}

func TestAddIgnoresConsecutiveDuplicates(t *testing.T) {
	s := openTest(t, 500)
	if _, stored, _ := s.Add(KindText, "text/plain", "a", "h", []byte("same")); !stored {
		t.Fatal("first Add reported as duplicate")
	}
	// Exact repeat of the latest entry: ignored, count unchanged.
	if id, stored, err := s.Add(KindText, "text/plain", "b", "host-b", []byte("same")); stored || id != 0 || err != nil {
		t.Fatalf("repeat: id=%d stored=%v err=%v", id, stored, err)
	}
	// Same text again as a *different* kind is not a duplicate.
	if _, stored, _ := s.Add(KindImage, "image/png", "a", "h", []byte("same")); !stored {
		t.Fatal("image with same bytes reported as duplicate")
	}
	// A different entry breaks the streak; repeating the old value now stores.
	if _, stored, _ := s.Add(KindText, "text/plain", "a", "h", []byte("other")); !stored {
		t.Fatal("different text reported as duplicate")
	}
	if _, stored, _ := s.Add(KindText, "text/plain", "a", "h", []byte("same")); !stored {
		t.Fatal("stale same-value text reported as duplicate")
	}
	// Only the latest stored entry counts; the ignored ones left no rows.
	items, _ := s.Recent(50, 0)
	if len(items) != 4 {
		t.Fatalf("expected 4 stored rows, got %d", len(items))
	}
	// A stored duplicate-free text that differs only by source name is still
	// a duplicate (origin does not count).
	if _, stored, _ := s.Add(KindText, "text/plain", "z", "host-z", []byte("same")); stored {
		t.Fatal("same content from another host should be ignored")
	}
}

// TestAddIgnoresReencodedDuplicateImage covers the image duplicate filter:
// two PNG encodings of the *same picture* count as duplicates even though
// their bytes differ (a Windows CF_DIB round-trip re-encodes a received PNG,
// so a byte-only comparison would let the echo through).
func TestAddIgnoresReencodedDuplicateImage(t *testing.T) {
	s := openTest(t, 500)
	orig := testPNG(t, 0)
	canon := canonicalPNG(t, orig)
	if bytes.Equal(orig, canon) {
		t.Fatal("test PNGs must differ byte-wise to model a re-encoding")
	}
	if _, stored, err := s.Add(KindImage, "image/png", "a", "host-a", orig); !stored || err != nil {
		t.Fatalf("first image Add: stored=%v err=%v", stored, err)
	}
	// Re-encoding of the same picture right after: ignored, no row added.
	if id, stored, err := s.Add(KindImage, "image/png", "b", "host-b", canon); stored || id != 0 || err != nil {
		t.Fatalf("re-encoded duplicate stored: id=%d stored=%v err=%v", id, stored, err)
	}
	// A genuinely different picture stores.
	other := testPNG(t, 1)
	if bytes.Equal(other, orig) || bytes.Equal(other, canon) {
		t.Fatal("test pictures must differ")
	}
	if _, stored, _ := s.Add(KindImage, "image/png", "a", "host-a", other); !stored {
		t.Fatal("different picture reported as duplicate")
	}
	// Another encoding of the older picture after an interrupting entry
	// stores again (only consecutive duplicates are filtered).
	if _, stored, _ := s.Add(KindImage, "image/png", "a", "host-a", canon); !stored {
		t.Fatal("stale same-picture image reported as duplicate")
	}
	items, _ := s.Recent(50, 0)
	if len(items) != 3 {
		t.Fatalf("expected 3 stored rows, got %d", len(items))
	}
}

// testPNG renders a small paletted picture; different shift values produce
// different pixel patterns.
func testPNG(t *testing.T, shift int) []byte {
	t.Helper()
	pal := color.Palette{
		color.RGBA{R: 255, A: 255},
		color.RGBA{G: 255, A: 255},
		color.RGBA{B: 255, A: 255},
		color.RGBA{R: 255, G: 255, A: 255},
	}
	pm := image.NewPaletted(image.Rect(0, 0, 4, 3), pal)
	for y := 0; y < 3; y++ {
		for x := 0; x < 4; x++ {
			pm.SetColorIndex(x, y, uint8((x+y+shift)%4))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, pm); err != nil {
		t.Fatalf("encode: %v", err)
	}
	return buf.Bytes()
}

// canonicalPNG re-encodes a PNG as an opaque RGBA image, modelling how the
// same picture comes back from a Windows CF_DIB round-trip.
func canonicalPNG(t *testing.T, data []byte) []byte {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	rgba := image.NewRGBA(img.Bounds())
	for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
		for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
			rgba.Set(x, y, img.At(x, y))
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		t.Fatalf("encode canonical: %v", err)
	}
	return buf.Bytes()
}

func TestClear(t *testing.T) {
	s := openTest(t, 500)
	for i := 0; i < 4; i++ {
		s.Add(KindText, "text/plain", "a", "h", []byte{byte('a' + i)})
	}
	if err := s.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	n, _ := s.Count()
	if n != 0 {
		t.Fatalf("count after clear = %d", n)
	}
}
