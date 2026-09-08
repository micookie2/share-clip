package dib

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 7, 5))
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			img.SetRGBA(x, y, color.RGBA{R: uint8(x * 30), G: uint8(y * 40), B: uint8(100 + x), A: 90})
		}
	}
	dib, err := Encode(img)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := Decode(dib)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for y := 0; y < 5; y++ {
		for x := 0; x < 7; x++ {
			// Alpha is intentionally forced opaque by Encode.
			want := color.RGBA{R: uint8(x * 30), G: uint8(y * 40), B: uint8(100 + x), A: 255}
			if got.At(x, y) != color.RGBAModel.Convert(want) {
				t.Fatalf("pixel (%d,%d) = %v, want %v", x, y, got.At(x, y), want)
			}
		}
	}
}

// writeHeader writes a BITMAPINFOHEADER (+ optional appended masks) into buf.
func writeHeader(buf *bytes.Buffer, bitCount, w, h int, topDown bool, comp uint32, masks []uint32) {
	var hdr [40]byte
	binary.LittleEndian.PutUint32(hdr[0:4], 40)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(w))
	hh := int32(h)
	if topDown {
		hh = -hh
	}
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(hh))
	binary.LittleEndian.PutUint16(hdr[12:14], 1)
	binary.LittleEndian.PutUint16(hdr[14:16], uint16(bitCount))
	binary.LittleEndian.PutUint32(hdr[16:20], comp)
	buf.Write(hdr[:])
	for _, m := range masks {
		var mb [4]byte
		binary.LittleEndian.PutUint32(mb[:], m)
		buf.Write(mb[:])
	}
}

func TestDecode24bppBottomUp(t *testing.T) {
	// 3x2, 24bpp bottom-up: the first stored row is the bottom scanline (y=1).
	// Pixel bytes are B,G,R; rows padded to a 4-byte boundary (stride 12).
	buf := &bytes.Buffer{}
	writeHeader(buf, 24, 3, 2, false, compBI_RGB, nil)
	bottom := []byte{10, 20, 30, 40, 50, 60, 70, 80, 90, 0, 0, 0}
	top := []byte{100, 110, 120, 130, 140, 150, 160, 170, 180, 0, 0, 0}
	buf.Write(bottom)
	buf.Write(top)

	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	check := func(x, y int, r, g, b uint8) {
		c := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
		if c.R != r || c.G != g || c.B != b || c.A != 255 {
			t.Fatalf("pixel(%d,%d) = %v, want RGB(%d,%d,%d)", x, y, c, r, g, b)
		}
	}
	check(0, 0, 120, 110, 100)
	check(1, 0, 150, 140, 130)
	check(0, 1, 30, 20, 10)
	check(2, 1, 90, 80, 70)
}

func TestDecode24bppTopDown(t *testing.T) {
	buf := &bytes.Buffer{}
	writeHeader(buf, 24, 2, 2, true, compBI_RGB, nil)
	buf.Write([]byte{1, 2, 3, 4, 5, 6, 0, 0}) // y=0 first
	buf.Write([]byte{7, 8, 9, 10, 11, 12, 0, 0})
	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if c.R != 3 || c.G != 2 || c.B != 1 {
		t.Fatalf("top pixel = %v", c)
	}
	c = color.RGBAModel.Convert(img.At(1, 1)).(color.RGBA)
	if c.R != 12 || c.G != 11 || c.B != 10 {
		t.Fatalf("bottom pixel = %v", c)
	}
}

func TestDecode32bppAlphaIgnored(t *testing.T) {
	buf := &bytes.Buffer{}
	writeHeader(buf, 32, 2, 1, false, compBI_RGB, nil)
	buf.Write([]byte{0xAA, 0xBB, 0xCC, 0x00, 0x10, 0x20, 0x30, 0xFF})
	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c0 := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if c0.R != 0xCC || c0.G != 0xBB || c0.B != 0xAA || c0.A != 255 {
		t.Fatalf("pixel0 = %v", c0)
	}
	c1 := color.RGBAModel.Convert(img.At(1, 0)).(color.RGBA)
	if c1.R != 0x30 || c1.G != 0x20 || c1.B != 0x10 || c1.A != 255 {
		t.Fatalf("pixel1 = %v", c1)
	}
}

func TestDecode8bppPalette(t *testing.T) {
	buf := &bytes.Buffer{}
	writeHeader(buf, 8, 2, 1, false, compBI_RGB, nil)
	// Palette: idx0 = red, idx1 = blue (B,G,R,X entries); biClrUsed defaults to
	// 256 entries for 8bpp, so pad the rest with zeros.
	pal := make([]byte, 256*4)
	copy(pal[0:4], []byte{0, 0, 255, 0}) // idx0: red
	copy(pal[4:8], []byte{255, 0, 0, 0}) // idx1: blue
	buf.Write(pal)
	buf.Write([]byte{1, 0, 0, 0}) // pixels: idx1, idx0 (+2 padding)
	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c0 := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if c0.R != 0 || c0.G != 0 || c0.B != 255 {
		t.Fatalf("pixel0 = %v, want blue", c0)
	}
	c1 := color.RGBAModel.Convert(img.At(1, 0)).(color.RGBA)
	if c1.R != 255 || c1.G != 0 || c1.B != 0 {
		t.Fatalf("pixel1 = %v, want red", c1)
	}
}

func TestDecode1bppMSBFirst(t *testing.T) {
	buf := &bytes.Buffer{}
	writeHeader(buf, 1, 9, 1, false, compBI_RGB, nil)
	// Palette idx0 = white, idx1 = red.
	buf.Write([]byte{255, 255, 255, 0, 0, 0, 255, 0})
	// Bits are MSB first within each byte. Pattern: 1 0 1 0 1 0 1 0 | 1 (pad to 4-byte stride)
	buf.Write([]byte{0b10101010, 0b10000000, 0, 0})
	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if c := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA); c.R != 255 || c.G != 0 || c.B != 0 {
		t.Fatalf("pixel0 = %v, want red", c)
	}
	if c := color.RGBAModel.Convert(img.At(1, 0)).(color.RGBA); c.R != 255 || c.G != 255 || c.B != 255 {
		t.Fatalf("pixel1 = %v, want white", c)
	}
	if c := color.RGBAModel.Convert(img.At(8, 0)).(color.RGBA); c.R != 255 || c.G != 0 || c.B != 0 {
		t.Fatalf("pixel8 = %v, want red", c)
	}
}

func TestDecode16bppRGB555(t *testing.T) {
	buf := &bytes.Buffer{}
	writeHeader(buf, 16, 2, 1, false, compBI_RGB, nil)
	// BI_RGB 16bpp is interpreted as XRGB 5-5-5.
	pack := func(r, g, b uint16) uint16 { return r<<10 | g<<5 | b }
	v0 := pack(31, 16, 8) // ~ (255, 131, 65) after linear scaling
	v1 := pack(0, 31, 31) // (0, 255, 255)
	le := binary.LittleEndian
	var row [4]byte
	le.PutUint16(row[0:2], v0)
	le.PutUint16(row[2:4], v1)
	buf.Write(row[:])

	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c0 := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if c0.R != 255 || c0.G != 131 || c0.B != 65 {
		t.Fatalf("pixel0 = %v, want RGB(255,131,65)", c0)
	}
	c1 := color.RGBAModel.Convert(img.At(1, 0)).(color.RGBA)
	if c1.R != 0 || c1.G != 255 || c1.B != 255 {
		t.Fatalf("pixel1 = %v, want RGB(0,255,255)", c1)
	}
}

func TestDecode16bpp565Bitfields(t *testing.T) {
	buf := &bytes.Buffer{}
	masks := []uint32{0xF800, 0x07E0, 0x001F}
	writeHeader(buf, 16, 1, 1, false, compBI_BITFIELDS, masks)
	le := binary.LittleEndian
	var row [4]byte // stride is 4 for 16bpp 1px wide
	le.PutUint16(row[0:2], 0xFFFF)
	buf.Write(row[:])
	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if c.R != 255 || c.G != 255 || c.B != 255 {
		t.Fatalf("pixel = %v, want white", c)
	}
}

func TestDecode32bppAlphaBitfields(t *testing.T) {
	buf := &bytes.Buffer{}
	// V4 header (108 bytes) carries the masks inline at offset 40.
	var hdr [108]byte
	binary.LittleEndian.PutUint32(hdr[0:4], 108)
	binary.LittleEndian.PutUint32(hdr[4:8], 1)
	binary.LittleEndian.PutUint32(hdr[8:12], 1)
	binary.LittleEndian.PutUint16(hdr[12:14], 1)
	binary.LittleEndian.PutUint16(hdr[14:16], 32)
	binary.LittleEndian.PutUint32(hdr[16:20], compBI_ALPHABITFIELDS)
	binary.LittleEndian.PutUint32(hdr[40:44], 0x00FF0000) // red
	binary.LittleEndian.PutUint32(hdr[44:48], 0x0000FF00) // green
	binary.LittleEndian.PutUint32(hdr[48:52], 0x000000FF) // blue
	binary.LittleEndian.PutUint32(hdr[52:56], 0xFF000000) // alpha
	buf.Write(hdr[:])
	var pixel [4]byte
	binary.LittleEndian.PutUint32(pixel[:], 0x80123456)
	buf.Write(pixel[:])

	img, err := Decode(buf.Bytes())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c := color.RGBAModel.Convert(img.At(0, 0)).(color.RGBA)
	if c.R != 0x12 || c.G != 0x34 || c.B != 0x56 || c.A != 0x80 {
		t.Fatalf("pixel = %v, want RGBA(0x12,0x34,0x56,0x80)", c)
	}
}

func TestDecodeErrors(t *testing.T) {
	cases := map[string][]byte{
		"too short":       {0, 0, 0, 0},
		"core header":     {12, 0, 0, 0, 1, 0, 1, 0, 24, 0, 0, 0, 0},
		"zero width":      headerOnly(0, 2),
		"zero height":     headerOnly(2, 0),
		"bad compression": {40, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 24, 0, 99, 0, 0, 0},
		"truncated":       {40, 0, 0, 0, 100, 0, 0, 0, 100, 0, 0, 0, 1, 0, 24, 0, 0, 0, 0, 0},
	}
	for name, data := range cases {
		if _, err := Decode(data); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}
}

func headerOnly(w, h int) []byte {
	var hdr [40]byte
	binary.LittleEndian.PutUint32(hdr[0:4], 40)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(w))
	binary.LittleEndian.PutUint32(hdr[8:12], uint32(h))
	binary.LittleEndian.PutUint16(hdr[12:14], 1)
	binary.LittleEndian.PutUint16(hdr[14:16], 24)
	return hdr[:]
}
