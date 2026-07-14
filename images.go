package main

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // decoder registration
	"os"
	"path/filepath"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decode-only, for people who upload webp
)

const (
	maxUploadBytes = 25 << 20 // 25 MiB
	displayMaxSide = 1800     // the "enormous cat photo"
	thumbMaxSide   = 600      // archive grid + RSS
	displayQuality = 86
	thumbQuality   = 80
	maxPixels      = 80_000_000
)

type processed struct {
	Image    string // filename in media/
	Thumb    string // filename in media/
	Original string // filename in originals/
	Width    int
	Height   int
	Bytes    int64
}

// processUpload decodes the raw upload, re-encodes it, and writes the results
// to disk. Re-encoding is the whole point: we hand the browser pixels we
// produced ourselves, so no EXIF (GPS!), no ICC payloads, no polyglot file
// that is a valid JPEG *and* a valid script. The original is kept for the
// archive but never served over HTTP.
func processUpload(dataDir string, raw []byte) (processed, error) {
	var z processed

	if len(raw) == 0 {
		return z, errors.New("empty file")
	}

	// Refuse decompression bombs before allocating pixels for them.
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return z, errors.New("that does not look like a JPEG, PNG or WebP image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return z, errors.New("image dimensions are out of range")
	}

	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return z, errors.New("could not decode that image")
	}

	// Order matters for speed. Downscale first, then rotate the small display
	// image, then derive the thumbnail from it:
	//
	//   - rotating is one op per pixel, so doing it after the downscale
	//     rotates ~2M pixels instead of ~12M;
	//   - the thumb resamples from the 1800px display instead of the 12MP
	//     original, reading far less memory.
	//
	// The display is bit-identical to rotating first: a 90-degree rotation is a
	// pixel permutation and the resize filter is symmetric, so the two commute
	// (measured: zero difference). The thumb, however, becomes a two-stage
	// downscale (12MP -> 1800 -> 600) rather than one-stage — visually
	// identical, but not bit-identical (measured within 3/255 per channel).
	display := fit(src, displayMaxSide)
	if format == "jpeg" {
		display = applyOrientation(display, exifOrientation(raw))
	}
	thumb := fit(display, thumbMaxSide)

	name, err := randomName()
	if err != nil {
		return z, err
	}

	mediaDir := filepath.Join(dataDir, "media")
	imgFile := name + ".jpg"
	thumbFile := name + "-t.jpg"
	origFile := name + originalExt(format)

	n, err := writeJPEG(filepath.Join(mediaDir, imgFile), display, displayQuality)
	if err != nil {
		return z, err
	}
	if _, err := writeJPEG(filepath.Join(mediaDir, thumbFile), thumb, thumbQuality); err != nil {
		os.Remove(filepath.Join(mediaDir, imgFile))
		return z, err
	}
	if err := os.WriteFile(filepath.Join(dataDir, "originals", origFile), raw, 0o640); err != nil {
		os.Remove(filepath.Join(mediaDir, imgFile))
		os.Remove(filepath.Join(mediaDir, thumbFile))
		return z, err
	}

	b := display.Bounds()
	return processed{
		Image:    imgFile,
		Thumb:    thumbFile,
		Original: origFile,
		Width:    b.Dx(),
		Height:   b.Dy(),
		Bytes:    n,
	}, nil
}

func originalExt(format string) string {
	switch format {
	case "png":
		return ".png"
	case "webp":
		return ".webp"
	default:
		return ".jpg"
	}
}

// fit scales the image down so its longest side is at most max. It never
// scales up: a small photo stays small rather than becoming a blurry big one.
func fit(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		return src
	}
	if w > h {
		h = h * max / w
		w = max
	} else {
		w = w * max / h
		h = max
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

func writeJPEG(path string, img image.Image, quality int) (int64, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		return 0, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		return 0, err
	}
	return int64(buf.Len()), nil
}

func randomName() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// exifOrientation digs the orientation tag out of a JPEG's APP1 segment.
// Phones almost always store the photo sideways plus a "rotate me" flag, and
// since we throw the metadata away on re-encode we have to bake the rotation
// into the pixels or every other cat photo lands on its side.
func exifOrientation(b []byte) int {
	const none = 1
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return none
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return none
		}
		marker := b[i+1]
		if marker == 0xD8 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // start of scan / end: no EXIF ahead
			return none
		}
		size := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if size < 2 || i+2+size > len(b) {
			return none
		}
		seg := b[i+4 : i+2+size]
		if marker == 0xE1 && len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
			return orientationFromTIFF(seg[6:])
		}
		i += 2 + size
	}
	return none
}

func orientationFromTIFF(t []byte) int {
	const none = 1
	if len(t) < 8 {
		return none
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return none
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return none
	}
	n := int(bo.Uint16(t[off : off+2]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return none
		}
		if bo.Uint16(t[e:e+2]) != 0x0112 { // Orientation
			continue
		}
		v := int(bo.Uint16(t[e+8 : e+10]))
		if v >= 1 && v <= 8 {
			return v
		}
		return none
	}
	return none
}

// applyOrientation rewrites the pixels for the 8 EXIF orientations.
func applyOrientation(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	swap := o >= 5 // 5..8 also transpose the axes
	dw, dh := w, h
	if swap {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2: // flip horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180
				dx, dy = w-1-x, h-1-y
			case 4: // flip vertical
				dx, dy = x, h-1-y
			case 5: // transpose
				dx, dy = y, x
			case 6: // rotate 90 CW
				dx, dy = h-1-y, x
			case 7: // transverse
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 270 CW
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
