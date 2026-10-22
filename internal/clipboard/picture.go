package clipboard

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// maxPictureArea bounds how many pixels PictureDigest will decode. It mirrors
// the guard used when reading clipboard images (and the server store's
// duplicate detection): decoding a larger picture just to fingerprint it would
// cost too much CPU and memory.
const maxPictureArea = 1 << 26

// PictureDigest returns a canonical digest of the decoded pixels of a PNG, so
// that two encodings of the same picture hash equal. It is the client-side
// counterpart of the server store's picture duplicate detection: a Windows
// CF_DIB round trip (or a web push of a re-encoded copy) produces different
// bytes for the same picture, and echo suppression must see through that.
//
// The digest is sha256 over the 8-byte big-endian width and height followed by
// the canonical non-premultiplied 8-bit RGBA pixels (color.NRGBA byte order,
// as produced by color.NRGBAModel). ok is false when the payload cannot be
// decoded as a PNG, is empty, has invalid dimensions, or exceeds
// maxPictureArea pixels.
//
// Canonicalisation (dims prefix, opaque-NRGBA bulk hash, per-pixel NRGBA
// conversion) must stay in sync with internal/store.pictureID: the server
// dedupes images by the same notion of "same picture", and the two sides must
// agree.
func PictureDigest(pngBytes []byte) (id [32]byte, ok bool) {
	cfg, err := png.DecodeConfig(bytes.NewReader(pngBytes))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 ||
		cfg.Width*cfg.Height > maxPictureArea {
		return id, false
	}
	img, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return id, false
	}
	b := img.Bounds()
	h := sha256.New()
	var dim [8]byte
	binary.BigEndian.PutUint32(dim[0:4], uint32(b.Dx()))
	binary.BigEndian.PutUint32(dim[4:8], uint32(b.Dy()))
	h.Write(dim[:])

	// Every 8-bit opaque picture that decodes to an *image.NRGBA already has
	// its pixels in the canonical form; hash them in bulk.
	if n, isNRGBA := img.(*image.NRGBA); isNRGBA && opaqueNRGBA(n) {
		h.Write(n.Pix)
		copy(id[:], h.Sum(nil))
		return id, true
	}

	// Paletted, grayscale, 16-bit or transparent pictures go pixel by pixel
	// through the NRGBA model, the format PNG decoders everywhere agree on
	// (non-premultiplied 8-bit color).
	var px [4]byte
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			px[0], px[1], px[2], px[3] = c.R, c.G, c.B, c.A
			h.Write(px[:])
		}
	}
	copy(id[:], h.Sum(nil))
	return id, true
}

// opaqueNRGBA reports whether every pixel of an NRGBA image has alpha 255.
func opaqueNRGBA(m *image.NRGBA) bool {
	for i := 3; i < len(m.Pix); i += 4 {
		if m.Pix[i] != 255 {
			return false
		}
	}
	return true
}
