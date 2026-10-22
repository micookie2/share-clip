package clipboard

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// testPixels returns a deterministic opaque picture used by the digest tests.
// It is small enough to encode quickly and varied enough that re-encoding it
// with a different compression level cannot produce the same bytes.
func testPixels() *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < m.Rect.Dy(); y++ {
		for x := 0; x < m.Rect.Dx(); x++ {
			i := y*m.Stride + x*4
			m.Pix[i+0] = uint8(x * 31)
			m.Pix[i+1] = uint8(y * 31)
			m.Pix[i+2] = uint8((x + y) * 17)
			m.Pix[i+3] = 255
		}
	}
	return m
}

// encodeTestPNG encodes m as a PNG with the given compression level.
func encodeTestPNG(t *testing.T, m image.Image, level png.CompressionLevel) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: level}
	if err := enc.Encode(&buf, m); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

// palettedCopy re-encodes src as a paletted image carrying the same pixels, so
// PNG color type 3 (indexed) can be compared against color type 2 (truecolor).
func palettedCopy(t *testing.T, src *image.NRGBA) *image.Paletted {
	t.Helper()
	seen := make(map[color.NRGBA]struct{})
	var pal color.Palette
	for y := 0; y < src.Rect.Dy(); y++ {
		for x := 0; x < src.Rect.Dx(); x++ {
			c := src.NRGBAAt(x, y)
			if _, ok := seen[c]; !ok {
				seen[c] = struct{}{}
				pal = append(pal, c)
			}
		}
	}
	dst := image.NewPaletted(src.Rect, pal)
	for y := 0; y < src.Rect.Dy(); y++ {
		for x := 0; x < src.Rect.Dx(); x++ {
			dst.Set(x, y, src.NRGBAAt(x, y))
		}
	}
	return dst
}

// falselyTransparent hides the fact that every pixel is opaque, forcing the
// PNG encoder to use color type 6 (truecolor + alpha). Decoding it yields an
// *image.NRGBA whose pixels are all opaque, which is exactly the bulk-hash
// fast path in PictureDigest.
type falselyTransparent struct{ *image.NRGBA }

func (falselyTransparent) Opaque() bool { return false }

func TestPictureDigestSamePictureDifferentEncoding(t *testing.T) {
	m := testPixels()
	fast := encodeTestPNG(t, m, png.BestSpeed)
	slow := encodeTestPNG(t, m, png.BestCompression)
	if bytes.Equal(fast, slow) {
		t.Fatal("compression levels produced identical bytes; test cannot prove encoding independence")
	}
	// Indexed color (color type 3) decodes to an *image.Paletted and plain
	// truecolor (color type 2) to an *image.RGBA; neither is the opaque-NRGBA
	// fast path, so both go through the per-pixel conversion path.
	indexed := encodeTestPNG(t, palettedCopy(t, m), png.DefaultCompression)
	// Color type 6 with every alpha 255 decodes to an *image.NRGBA and takes
	// the bulk-hash fast path, which must agree with the per-pixel path.
	withAlpha := encodeTestPNG(t, falselyTransparent{m}, png.DefaultCompression)
	if dec, err := png.Decode(bytes.NewReader(withAlpha)); err != nil {
		t.Fatalf("test setup: decode truecolor+alpha: %v", err)
	} else if n, ok := dec.(*image.NRGBA); !ok || !opaqueNRGBA(n) {
		t.Fatalf("test setup: expected an opaque *image.NRGBA, got %T", dec)
	}

	want, ok := PictureDigest(fast)
	if !ok {
		t.Fatal("PictureDigest rejected a valid PNG")
	}
	for name, enc := range map[string][]byte{
		"different compression": slow,
		"paletted":              indexed,
		"truecolor with alpha":  withAlpha,
	} {
		if bytes.Equal(fast, enc) {
			t.Errorf("%s: encoding produced the original bytes, test is not exercising re-encoding", name)
			continue
		}
		got, ok := PictureDigest(enc)
		if !ok {
			t.Errorf("%s: PictureDigest rejected a valid PNG", name)
			continue
		}
		if got != want {
			t.Errorf("%s: digest %x != %x for the same picture", name, got, want)
		}
	}
}

func TestPictureDigestDifferentPicture(t *testing.T) {
	base := testPixels()
	other := testPixels()
	other.SetNRGBA(3, 4, color.NRGBA{R: 1, G: 2, B: 3, A: 255})

	a := encodeTestPNG(t, base, png.DefaultCompression)
	b := encodeTestPNG(t, other, png.DefaultCompression)
	da, oka := PictureDigest(a)
	db, okb := PictureDigest(b)
	if !oka || !okb {
		t.Fatalf("PictureDigest rejected valid PNGs: ok=%v/%v", oka, okb)
	}
	if da == db {
		t.Fatalf("different pictures share digest %x", da)
	}
}

func TestPictureDigestAlphaMatters(t *testing.T) {
	opaque := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	opaque.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 255})
	transparent := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	transparent.SetNRGBA(0, 0, color.NRGBA{R: 10, G: 20, B: 30, A: 0})

	da, oka := PictureDigest(encodeTestPNG(t, transparent, png.DefaultCompression))
	db, okb := PictureDigest(encodeTestPNG(t, opaque, png.DefaultCompression))
	if !oka || !okb {
		t.Fatalf("PictureDigest rejected valid PNGs: ok=%v/%v", oka, okb)
	}
	if da == db {
		t.Fatal("opaque and transparent versions of a picture share a digest")
	}
}

func TestPictureDigestRejectsGarbage(t *testing.T) {
	valid := encodeTestPNG(t, testPixels(), png.DefaultCompression)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"nil", nil},
		{"empty", []byte{}},
		{"text", []byte("not a png at all")},
		{"truncated", valid[:len(valid)/2]},
	} {
		if _, ok := PictureDigest(tc.data); ok {
			t.Errorf("%s: PictureDigest accepted garbage", tc.name)
		}
	}
}

func TestPictureDigestRejectsOversizedPicture(t *testing.T) {
	// 10000x10000 = 100M pixels exceeds the 1<<26 guard. DecodeConfig still
	// parses this header (and, since it is not paletted, returns without
	// touching pixel data), so a rejection proves the area guard fired; a
	// genuinely decodable 10000x10000 PNG would need ~400 MB of input here.
	const side = 10000
	fake := fakePNGHeader(side, side)
	if _, err := png.DecodeConfig(bytes.NewReader(fake)); err != nil {
		t.Fatalf("test setup: DecodeConfig rejected the crafted header: %v", err)
	}
	if _, ok := PictureDigest(fake); ok {
		t.Fatal("PictureDigest accepted a picture above the pixel-area guard")
	}
	// A small valid picture is of course still accepted.
	if _, ok := PictureDigest(encodeTestPNG(t, testPixels(), png.DefaultCompression)); !ok {
		t.Fatal("PictureDigest rejected a small valid picture")
	}
}

// fakePNGHeader builds a PNG signature plus a valid IHDR chunk declaring a
// truecolor picture of the given size (and an IEND). The header is all
// DecodeConfig needs; there is no pixel data.
func fakePNGHeader(w, h uint32) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], w)
	binary.BigEndian.PutUint32(ihdr[4:8], h)
	ihdr[8] = 8 // bit depth
	ihdr[9] = 2 // color type: truecolor, no palette needed
	writePNGChunk(&buf, "IHDR", ihdr)
	writePNGChunk(&buf, "IEND", nil)
	return buf.Bytes()
}

func writePNGChunk(buf *bytes.Buffer, typ string, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	buf.Write(length[:])
	buf.WriteString(typ)
	buf.Write(data)
	crc := crc32.NewIEEE()
	crc.Write([]byte(typ))
	crc.Write(data)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc.Sum32())
	buf.Write(sum[:])
}
