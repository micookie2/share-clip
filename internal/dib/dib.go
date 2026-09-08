// Package dib converts between Windows Device Independent Bitmaps (the data
// stored under the CF_DIB clipboard format) and image.Image / PNG, in pure Go.
//
// The codec is platform independent and fully unit tested; only the Win32
// clipboard plumbing in package clipboard is Windows-specific.
package dib

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math/bits"
)

// Constants from wingdi.h.
const (
	compBI_RGB            = 0
	compBI_BITFIELDS      = 3
	compBI_ALPHABITFIELDS = 6

	headerBITMAPINFOHEADER = 40
)

var errUnsupported = errors.New("dib: unsupported bitmap")

type dibInfo struct {
	width     int
	height    int // absolute value; sign recorded separately
	topDown   bool
	bitCount  int
	comp      uint32
	redMask   uint32
	greenMask uint32
	blueMask  uint32
	alphaMask uint32
	hasAlpha  bool
	dataStart int
	palette   []byte // raw BGRA entries, nil when none
}

func parseInfo(data []byte) (*dibInfo, error) {
	if len(data) < headerBITMAPINFOHEADER {
		return nil, errors.New("dib: data too short for BITMAPINFOHEADER")
	}
	size := int(binary.LittleEndian.Uint32(data[0:4]))
	if size < headerBITMAPINFOHEADER {
		return nil, fmt.Errorf("dib: unsupported header size %d (BITMAPCOREHEADER not supported)", size)
	}
	if len(data) < size {
		return nil, errors.New("dib: data shorter than declared header")
	}

	info := &dibInfo{}

	w := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	h := int(int32(binary.LittleEndian.Uint32(data[8:12])))
	if w <= 0 {
		return nil, fmt.Errorf("dib: bad width %d", w)
	}
	if h == 0 {
		return nil, errors.New("dib: zero height")
	}
	if h < 0 {
		info.topDown = true
		info.height = -h
	} else {
		info.height = h
	}
	info.width = w

	planes := binary.LittleEndian.Uint16(data[12:14])
	if planes != 1 {
		return nil, fmt.Errorf("dib: bad planes %d", planes)
	}
	info.bitCount = int(binary.LittleEndian.Uint16(data[14:16]))
	switch info.bitCount {
	case 1, 4, 8, 16, 24, 32:
	default:
		return nil, fmt.Errorf("dib: unsupported bit count %d", info.bitCount)
	}

	info.comp = binary.LittleEndian.Uint32(data[16:20])
	switch info.comp {
	case compBI_RGB, compBI_BITFIELDS, compBI_ALPHABITFIELDS:
	default:
		return nil, fmt.Errorf("dib: unsupported compression %d", info.comp)
	}
	if info.bitCount < 16 && info.comp != compBI_RGB {
		return nil, fmt.Errorf("dib: palette formats must use BI_RGB, got %d", info.comp)
	}
	if info.bitCount < 32 && info.comp == compBI_ALPHABITFIELDS {
		return nil, errors.New("dib: BI_ALPHABITFIELDS requires 32bpp")
	}

	// Work out where the pixel data starts. Masks for BI_BITFIELDS /
	// BI_ALPHABITFIELDS live at offset 40 regardless of whether they were
	// stored inside a V4/V5 header or appended to a 40-byte header, so we can
	// read them from the same place either way.
	offset := size
	switch info.comp {
	case compBI_BITFIELDS, compBI_ALPHABITFIELDS:
		if len(data) < 52 {
			return nil, errors.New("dib: missing color masks")
		}
		info.redMask = binary.LittleEndian.Uint32(data[40:44])
		info.greenMask = binary.LittleEndian.Uint32(data[44:48])
		info.blueMask = binary.LittleEndian.Uint32(data[48:52])
		if info.redMask == 0 && info.greenMask == 0 && info.blueMask == 0 {
			return nil, errors.New("dib: empty color masks")
		}
		if info.comp == compBI_ALPHABITFIELDS {
			if len(data) < 56 {
				return nil, errors.New("dib: missing alpha mask")
			}
			info.alphaMask = binary.LittleEndian.Uint32(data[52:56])
			info.hasAlpha = info.alphaMask != 0
		}
		if offset < 52 {
			offset = 52
		}
		if info.comp == compBI_ALPHABITFIELDS && offset < 56 {
			offset = 56
		}
	case compBI_RGB:
		switch info.bitCount {
		case 16: // BI_RGB 16bpp is stored as XRGB 5-5-5.
			info.redMask, info.greenMask, info.blueMask = 0x7C00, 0x03E0, 0x001F
		case 32: // BGRA byte order, alpha byte ignored (often garbage/0).
			info.redMask, info.greenMask, info.blueMask = 0x00FF0000, 0x0000FF00, 0x000000FF
		}
	}

	// Palette (<= 8bpp) follows the header.
	if info.bitCount <= 8 {
		clrUsed := binary.LittleEndian.Uint32(data[32:36])
		n := int(clrUsed)
		if n == 0 {
			n = 1 << uint(info.bitCount)
		}
		if n < 0 || n > 1<<20 {
			return nil, errors.New("dib: unreasonable palette size")
		}
		if offset+n*4 > len(data) {
			return nil, errors.New("dib: truncated palette")
		}
		info.palette = data[offset : offset+n*4]
		offset += n * 4
	}

	// Guard against absurd dimensions before allocating.
	if info.width > 1<<16 || info.height > 1<<16 {
		return nil, errors.New("dib: image dimensions too large")
	}
	if info.width*info.height > 1<<26 {
		return nil, errors.New("dib: image area too large")
	}
	stride := rowStride(info.width, info.bitCount)
	need := stride * info.height
	if len(data) < offset+need {
		return nil, fmt.Errorf("dib: truncated pixel data: have %d need %d", len(data)-offset, need)
	}
	info.dataStart = offset
	return info, nil
}

func rowStride(width, bitCount int) int {
	return ((width*bitCount + 31) / 32) * 4
}

// Decode converts a DIB byte blob (the CF_DIB clipboard payload) into an
// image.Image. Supported: 1/4/8 bpp paletted, 16/24/32 bpp, BI_RGB,
// BI_BITFIELDS and BI_ALPHABITFIELDS, top-down and bottom-up storage.
func Decode(data []byte) (image.Image, error) {
	info, err := parseInfo(data)
	if err != nil {
		return nil, err
	}
	w, h := info.width, info.height
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	stride := rowStride(w, info.bitCount)
	pix := data[info.dataStart:]

	for row := 0; row < h; row++ {
		srcRow := row
		if !info.topDown {
			srcRow = h - 1 - row // bottom-up: first stored row is the bottom one
		}
		line := pix[srcRow*stride : srcRow*stride+stride]
		for x := 0; x < w; x++ {
			r, g, b, a := info.pixelAt(line, x)
			img.SetRGBA(x, row, color.RGBA{R: r, G: g, B: b, A: a})
		}
	}
	return img, nil
}

// pixelAt returns the RGBA color of pixel x on one decoded scanline.
func (i *dibInfo) pixelAt(line []byte, x int) (r, g, b, a uint8) {
	switch i.bitCount {
	case 32:
		p := x * 4
		if i.comp == compBI_RGB {
			// B,G,R,A with alpha ignored for compatibility.
			return line[p+2], line[p+1], line[p], 255
		}
		v := binary.LittleEndian.Uint32(line[p : p+4])
		r, g, b, a = maskColor(v, i.redMask, i.greenMask, i.blueMask, i.alphaMask, i.hasAlpha)
		return
	case 24:
		p := x * 3
		return line[p+2], line[p+1], line[p], 255
	case 16:
		v := binary.LittleEndian.Uint16(line[x*2 : x*2+2])
		r, g, b, _ = maskColor(uint32(v), i.redMask, i.greenMask, i.blueMask, 0, false)
		return r, g, b, 255
	default:
		idx, ok := i.paletteIndex(line, x)
		if !ok || idx*4+3 >= len(i.palette) {
			return 0, 0, 0, 255
		}
		// Palette entries are B,G,R,X.
		return i.palette[idx*4+2], i.palette[idx*4+1], i.palette[idx*4], 255
	}
}

// paletteIndex extracts the palette index of pixel x (1/4/8 bpp formats).
func (i *dibInfo) paletteIndex(line []byte, x int) (int, bool) {
	switch i.bitCount {
	case 1:
		byteIdx := x / 8
		if byteIdx >= len(line) {
			return 0, false
		}
		bit := 7 - uint(x%8) // MSB first, like BMP
		return int(line[byteIdx] >> bit & 1), true
	case 4:
		byteIdx := x / 2
		if byteIdx >= len(line) {
			return 0, false
		}
		if x%2 == 0 {
			return int(line[byteIdx] >> 4), true
		}
		return int(line[byteIdx] & 0x0F), true
	case 8:
		if x >= len(line) {
			return 0, false
		}
		return int(line[x]), true
	}
	return 0, false
}

// maskColor extracts channel values from a packed pixel using bit masks,
// scaling them to full 8-bit range. When the masks are byte-aligned BGRA the
// result is exact; otherwise a simple linear scale is applied.
func maskColor(v, rm, gm, bm, am uint32, hasAlpha bool) (r, g, b, a uint8) {
	r = scaleMask(v, rm)
	g = scaleMask(v, gm)
	b = scaleMask(v, bm)
	if hasAlpha {
		a = scaleMask(v, am)
	} else {
		a = 255
	}
	return
}

func scaleMask(v, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := bits.TrailingZeros32(mask)
	width := bits.OnesCount32(mask)
	val := (v & mask) >> uint(shift)
	if width >= 8 {
		return uint8(val)
	}
	max := uint32(1)<<uint(width) - 1
	return uint8(int(val*255) / int(max))
}

// Encode converts an image into a 32 bpp BI_RGB DIB (bottom-up, BGRX byte
// order) suitable for SetClipboardData with CF_DIB. Alpha is forced to 255:
// many Windows applications mis-render transparent DIBs (e.g. black boxes in
// Office), so transparency is dropped for maximum compatibility.
func Encode(img image.Image) ([]byte, error) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errors.New("dib: empty image")
	}
	if w > 1<<16 || h > 1<<16 {
		return nil, errors.New("dib: image dimensions too large")
	}
	if w*h > 1<<26 {
		return nil, errors.New("dib: image area too large")
	}

	stride := rowStride(w, 32)
	out := make([]byte, headerBITMAPINFOHEADER+stride*h)

	le := binary.LittleEndian
	le.PutUint32(out[0:4], headerBITMAPINFOHEADER)
	le.PutUint32(out[4:8], uint32(w))
	le.PutUint32(out[8:12], uint32(h)) // positive => bottom-up
	le.PutUint16(out[12:14], 1)
	le.PutUint16(out[14:16], 32)
	le.PutUint32(out[16:20], compBI_RGB)
	le.PutUint32(out[20:24], uint32(stride*h))

	dst := out[headerBITMAPINFOHEADER:]
	for y := 0; y < h; y++ {
		srcY := h - 1 - y // bottom-up
		line := dst[y*stride : y*stride+stride]
		for x := 0; x < w; x++ {
			c := color.RGBAModel.Convert(img.At(b.Min.X+x, b.Min.Y+srcY)).(color.RGBA)
			p := x * 4
			line[p+0] = c.B
			line[p+1] = c.G
			line[p+2] = c.R
			line[p+3] = 255
		}
	}
	return out, nil
}
