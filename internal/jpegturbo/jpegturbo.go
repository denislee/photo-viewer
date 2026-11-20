// Package jpegturbo decodes JPEGs with libjpeg-turbo's TurboJPEG 3 API in
// process. It is 2.5–3× faster than image/jpeg, can decode straight to a
// reduced size through DCT scaling (1/2, 1/4, 1/8 …), and writes RGBA directly,
// so the result needs no further conversion before it is handed to Gio.
//
// The cgo binding is built by default. Building with the nojpegturbo tag (or
// with cgo disabled) swaps in a stub whose Available reports false, so callers
// fall back to image/jpeg and the binary no longer links libturbojpeg.
package jpegturbo

import (
	"errors"
	"image"
)

// ErrUnavailable is returned by DecodeFit when the package was built without
// libjpeg-turbo.
var ErrUnavailable = errors.New("jpegturbo: built without libjpeg-turbo")

// IsJPEG reports whether data starts with a JPEG SOI marker.
func IsJPEG(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF
}

// Config is a JPEG's stored (unrotated) pixel size.
type Config struct {
	Width, Height int
}

// scaledSize mirrors libjpeg-turbo's TJSCALED: ceil(dim × num / denom).
func scaledSize(dim, num, denom int) int {
	return (dim*num + denom - 1) / denom
}

// pickScale returns the smallest num/denom from factors (all assumed ≤ 1, any
// order) whose scaled w×h is at least minW×minH. A zero minimum on both axes,
// or one larger than the image, selects the full size (1/1).
func pickScale(factors [][2]int, w, h, minW, minH int) (num, denom int) {
	num, denom = 1, 1
	if minW <= 0 && minH <= 0 {
		return
	}
	bestW := w
	for _, f := range factors {
		n, d := f[0], f[1]
		if n > d {
			continue
		}
		sw, sh := scaledSize(w, n, d), scaledSize(h, n, d)
		if sw >= minW && sh >= minH && sw < bestW {
			num, denom, bestW = n, d, sw
		}
	}
	return
}

// DecodeFit decodes a JPEG into an origin-anchored *image.RGBA, applying the
// strongest DCT downscale that still keeps the output at least minW×minH (in
// stored orientation), so a 12 MP photo shown in a 1080p window decodes at 3/8
// scale and costs a fraction of a full decode. The output is never upscaled.
//
// fit receives the image's stored width and height and returns the minimum
// output size; a nil fit, or a zero minimum on both axes, means full size. The
// scale is chosen from the image's own stored size with a single header parse.
//
// DecodeFit returns ErrUnavailable when built without libjpeg-turbo, and an
// error for anything libjpeg-turbo can't convert to RGBA (for example CMYK
// JPEGs); callers then fall back to image/jpeg. The returned Config is the
// stored size whenever the header could be read, even when the decode itself
// then fails (for example a CMYK JPEG), so a caller falling back to
// image/jpeg still knows the native size.
func DecodeFit(data []byte, fit func(w, h int) (minW, minH int)) (*image.RGBA, Config, error) {
	return decodeFit(data, fit)
}

// EncodeRGBA compresses img as a JPEG with 4:2:0 chroma subsampling at the
// given quality (1..100; values outside the range are clamped). libjpeg-turbo
// encodes roughly 10× faster than image/jpeg (T5-03), which matters because
// every display rendition and export is re-encoded.
//
// EncodeRGBA returns ErrUnavailable when built without libjpeg-turbo; callers
// then fall back to image/jpeg.
func EncodeRGBA(img *image.RGBA, quality int) ([]byte, error) {
	return encodeRGBA(img, quality)
}

// Available reports whether libjpeg-turbo was linked in.
func Available() bool { return available }
