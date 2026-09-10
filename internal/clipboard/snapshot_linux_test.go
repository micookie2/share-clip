//go:build linux

package clipboard

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"testing"

	"golang.org/x/image/bmp"
	"golang.org/x/image/tiff"
)

func TestParseTargets(t *testing.T) {
	cases := []struct {
		out  string
		want int
	}{
		{"", 0},
		{"\n  \t\n", 0},
		{"TIMESTAMP\nTARGETS\nUTF8_STRING\nimage/png\n", 4},
		{"text/plain;charset=utf-8 image/png", 2}, // tolerate other separators
	}
	for _, c := range cases {
		if got := parseTargets(c.out); len(got) != c.want {
			t.Errorf("parseTargets(%q) = %v, want %d items", c.out, got, c.want)
		}
	}
}

func TestChooseImageTarget(t *testing.T) {
	cases := []struct {
		offered   []string
		wantExact string
		wantCanon string
		wantOK    bool
	}{
		{[]string{"UTF8_STRING"}, "", "", false},
		{[]string{"image/png"}, "image/png", "image/png", true},
		{[]string{"text/plain", "image/x-png"}, "image/x-png", "image/png", true},
		// Preference order: png before jpeg, jpeg before bmp.
		{[]string{"image/jpeg", "image/png"}, "image/png", "image/png", true},
		{[]string{"image/bmp", "image/jpg"}, "image/jpg", "image/jpeg", true},
		{[]string{"image/tif", "image/png"}, "image/png", "image/png", true},
	}
	for _, c := range cases {
		exact, canon, ok := chooseImageTarget(offerSet(c.offered))
		if ok != c.wantOK || exact != c.wantExact || canon != c.wantCanon {
			t.Errorf("chooseImageTarget(%v) = (%q,%q,%v), want (%q,%q,%v)",
				c.offered, exact, canon, ok, c.wantExact, c.wantCanon, c.wantOK)
		}
	}
}

func TestChooseTextTarget(t *testing.T) {
	cases := []struct {
		offered []string
		want    string
	}{
		{[]string{"image/png"}, ""},
		{[]string{"text/plain;charset=utf-8"}, "text/plain;charset=utf-8"},
		{[]string{"UTF8_STRING", "text/plain"}, "UTF8_STRING"},
		{[]string{"text/html"}, ""}, // rich text alone must not be treated as plain text
	}
	for _, c := range cases {
		if got := chooseTextTarget(offerSet(c.offered)); got != c.want {
			t.Errorf("chooseTextTarget(%v) = %q, want %q", c.offered, got, c.want)
		}
	}
}

func TestSnapshotFromTargets(t *testing.T) {
	img := testRGBAImage()
	pngBytes := encodePng(t, img)
	jpegBytes := encodeJpeg(t, img)

	// fetch returns the bytes for targets present in this map, errNoTarget for
	// everything else; every requested target is recorded.
	newFetch := func(data map[string][]byte, requested *[]string) func(string) ([]byte, error) {
		return func(mime string) ([]byte, error) {
			*requested = append(*requested, mime)
			if b, ok := data[mime]; ok {
				return b, nil
			}
			return nil, errNoTarget
		}
	}

	t.Run("png passthrough keeps bytes identical", func(t *testing.T) {
		var req []string
		c, err := snapshotFromTargets(
			[]string{"image/png", "text/plain"},
			newFetch(map[string][]byte{"image/png": pngBytes}, &req))
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != KindImage || !bytes.Equal(c.PNG, pngBytes) {
			t.Fatalf("png not passed through: kind=%v equal=%v", c.Kind, bytes.Equal(c.PNG, pngBytes))
		}
		if len(req) != 1 || req[0] != "image/png" {
			t.Fatalf("image must win over text and be fetched alone, got %v", req)
		}
	})

	t.Run("image preferred over text", func(t *testing.T) {
		var req []string
		c, err := snapshotFromTargets(
			[]string{"text/plain", "image/jpeg"},
			newFetch(map[string][]byte{
				"text/plain": []byte("alt text"),
				"image/jpeg": jpegBytes,
			}, &req))
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != KindImage || !validPNG(c.PNG) {
			t.Fatalf("want a PNG image, got kind=%v", c.Kind)
		}
	})

	t.Run("non-png formats converted", func(t *testing.T) {
		for mime, enc := range map[string][]byte{
			"image/jpeg": jpegBytes,
			"image/bmp":  encodeBmp(t, img),
			"image/tiff": encodeTiff(t, img),
			"image/gif":  encodeGif(t, img),
		} {
			var req []string
			c, err := snapshotFromTargets([]string{mime},
				newFetch(map[string][]byte{mime: enc}, &req))
			if err != nil {
				t.Fatalf("%s: %v", mime, err)
			}
			if c.Kind != KindImage || !validPNG(c.PNG) {
				t.Fatalf("%s: not converted to a valid PNG (kind=%v)", mime, c.Kind)
			}
			if got := decodedSize(t, c.PNG); got != image.Rect(0, 0, 3, 2) {
				t.Fatalf("%s: converted size %v, want 3x2", mime, got)
			}
			if req[0] != mime {
				t.Fatalf("%s: fetched %q", mime, req[0])
			}
		}
	})

	t.Run("mislabelled image sniffed from bytes", func(t *testing.T) {
		// Advertised as png but actually jpeg bytes: sniffing must recover it.
		var req []string
		c, err := snapshotFromTargets([]string{"image/png"},
			newFetch(map[string][]byte{"image/png": jpegBytes}, &req))
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != KindImage || !validPNG(c.PNG) {
			t.Fatalf("sniffed image not shared, kind=%v", c.Kind)
		}
	})

	t.Run("corrupt image falls back to text", func(t *testing.T) {
		var req []string
		c, err := snapshotFromTargets(
			[]string{"image/jpeg", "text/plain"},
			newFetch(map[string][]byte{
				"image/jpeg": []byte("not an image at all"),
				"text/plain": []byte("hello"),
			}, &req))
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != KindText || c.Text != "hello" {
			t.Fatalf("want text fallback, got %+v", c)
		}
	})

	t.Run("text only", func(t *testing.T) {
		var req []string
		c, err := snapshotFromTargets(
			[]string{"text/plain;charset=utf-8", "text/html"},
			newFetch(map[string][]byte{"text/plain;charset=utf-8": []byte("hi")}, &req))
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind != KindText || c.Text != "hi" {
			t.Fatalf("got %+v", c)
		}
	})

	t.Run("file list ignored even with image preview", func(t *testing.T) {
		var req []string
		c, err := snapshotFromTargets(
			[]string{"text/uri-list", "image/png", "x-special/gnome-copied-files"},
			newFetch(map[string][]byte{
				"text/uri-list": []byte("file:///home/u/pic.png"),
				"image/png":     pngBytes,
			}, &req))
		if err != nil {
			t.Fatal(err)
		}
		if !c.Empty() || len(req) != 0 {
			t.Fatalf("file copy must not be shared, got %+v, fetch=%v", c, req)
		}
	})

	t.Run("empty and unsupported", func(t *testing.T) {
		for _, offered := range [][]string{{}, {"application/x-qt-image"}, {"text/html"}} {
			c, err := snapshotFromTargets(offered, func(string) ([]byte, error) {
				return nil, errors.New("must not fetch")
			})
			if err != nil || !c.Empty() {
				t.Fatalf("offered=%v: want empty without error, got %+v err=%v", offered, c, err)
			}
		}
	})

	t.Run("text content that is a file artifact", func(t *testing.T) {
		var req []string
		c, err := snapshotFromTargets(
			[]string{"text/plain"},
			newFetch(map[string][]byte{"text/plain": []byte("file:///etc/hosts")}, &req))
		if err != nil {
			t.Fatal(err)
		}
		if !c.Empty() {
			t.Fatalf("file artifact text must be ignored, got %+v", c)
		}
	})
}

func TestToPNGRoundTripsAndGuards(t *testing.T) {
	img := testRGBAImage()

	pngBytes := encodePng(t, img)
	if out, err := toPNG(pngBytes, "image/png"); err != nil || !bytes.Equal(out, pngBytes) {
		t.Fatalf("valid png must pass through untouched: err=%v", err)
	}
	if _, err := toPNG([]byte("garbage"), "image/png"); err == nil {
		t.Fatal("garbage must not decode")
	}

	// Corrupt png (valid header, invalid body) must be rejected.
	huge := make([]byte, len(pngBytes))
	copy(huge, pngBytes)
	huge[16] ^= 0xFF // corrupt a payload byte
	if _, err := toPNG(huge, "image/png"); err == nil {
		t.Fatal("corrupt png must not pass through")
	}
}

// TestDecodeGuard pins the pixel budget that keeps a malicious clipboard
// "image" (huge dimensions in the header) from ballooning into a full decode.
func TestDecodeGuard(t *testing.T) {
	fake := imageCodec{
		config: func(io.Reader) (image.Config, error) {
			return image.Config{Width: 1 << 20, Height: 1 << 20}, nil // 2^40 pixels
		},
		decode: func(io.Reader) (image.Image, error) { return nil, nil },
	}
	if _, err := decodeBounded(fake, nil); err == nil {
		t.Fatal("oversized image must be rejected before decoding")
	}
}

func TestFirstInteger(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"wl-clipboard 2.2.1\n", 2},
		{"wl-clipboard 1.3.0 (2021)\n", 1},
		{"wl-clipboard v2.0.0\n", 2},
		{"no digits here", -1},
	}
	for _, c := range cases {
		if got := firstInteger(c.in); got != c.want {
			t.Errorf("firstInteger(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// --- helpers ----------------------------------------------------------------

func testRGBAImage() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 3, 2))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	img.Set(1, 0, color.RGBA{G: 255, A: 255})
	img.Set(2, 0, color.RGBA{B: 255, A: 255})
	img.Set(0, 1, color.RGBA{R: 255, G: 255, A: 255})
	img.Set(1, 1, color.RGBA{G: 255, B: 255, A: 255})
	img.Set(2, 1, color.RGBA{R: 255, B: 255, A: 255})
	return img
}

func encodePng(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeJpeg(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeBmp(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := bmp.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeTiff(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := tiff.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeGif(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodedSize(t *testing.T, data []byte) image.Rectangle {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img.Bounds()
}
