// Package imgorient reads the EXIF Orientation tag from an image file and
// bakes that orientation into the pixels of a decoded image. It exists because
// Go's standard image decoders (image/jpeg, and the x/image codecs) ignore the
// EXIF Orientation tag entirely: they hand back the pixels exactly as stored,
// so a portrait photo shot on a camera that recorded "rotate 90°" in EXIF
// decodes sideways. Anything that re-encodes such an image (export
// recompression today; thumbnail generation in a follow-up) and does NOT copy
// the original EXIF forward would otherwise produce a permanently-rotated file
// with no tag left to correct it.
//
// The package is intentionally standalone and dependency-light (image + goexif
// only) so it can be reused wherever a decoded image needs to be made upright.
package imgorient

import (
	"image"
	"os"

	"github.com/rwcarlsen/goexif/exif"
)

// ReadOrientation returns the EXIF Orientation of the image at path as a value
// in [1, 8]. It returns 1 (the "already upright, no transform" value) whenever
// the file can't be opened, has no EXIF, has no Orientation tag, or carries an
// out-of-range value — so callers can always treat the result as a safe no-op
// default and never need to handle an error for the common "no EXIF" case.
func ReadOrientation(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 1
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil {
		return 1
	}
	tag, err := x.Get(exif.Orientation)
	if err != nil {
		return 1
	}
	o, err := tag.Int(0)
	if err != nil || o < 1 || o > 8 {
		return 1
	}
	return o
}

// Apply returns img transformed so its pixels are upright for the given EXIF
// orientation. Orientation 1 (and any out-of-range value) is the identity and
// returns img unchanged with no allocation. The four "transpose" orientations
// (5–8) swap width and height, so the returned image's bounds differ from the
// input's for those; the caller should re-read Bounds() afterwards.
//
// The eight EXIF orientation values and the transform each one needs to become
// upright:
//
//	1 = identity
//	2 = mirror horizontal
//	3 = rotate 180°
//	4 = mirror vertical
//	5 = transpose      (reflect across the main / top-left→bottom-right diagonal)
//	6 = rotate 90° CW
//	7 = transverse     (reflect across the anti-diagonal)
//	8 = rotate 90° CCW
//
// For the two packed 4-byte-per-pixel layouts the decode/scale pipeline
// actually produces — *image.RGBA (from draw.Scale) and *image.NRGBA (from the
// PNG decoder) — Apply remaps whole pixels through the Pix slice, which skips
// the per-pixel interface dispatch and (for a YCbCr JPEG source) colour
// conversion that At/Set would pay. Any other image type falls back to the
// generic At/Set remap. Both are O(w·h); callers should orient *after*
// downscaling so the remap runs at output resolution, not source resolution.
func Apply(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	switch src := img.(type) {
	case *image.RGBA:
		dst := image.NewRGBA(orientedRect(b, orientation))
		remapPix(src.Pix, src.Stride, dst.Pix, dst.Stride, w, h, orientation)
		return dst
	case *image.NRGBA:
		dst := image.NewNRGBA(orientedRect(b, orientation))
		remapPix(src.Pix, src.Stride, dst.Pix, dst.Stride, w, h, orientation)
		return dst
	}

	// Generic fallback for any other image.Image (e.g. a small, already-≤max
	// YCbCr JPEG handed over without a downscale): a plain per-pixel At/Set
	// remap. Correct for every image type, just slower.
	dst := image.NewRGBA(orientedRect(b, orientation))
	for y := range h {
		for x := range w {
			dx, dy := destXY(orientation, x, y, w, h)
			dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}

// orientedRect returns the origin-anchored bounds of the upright image an
// orientation produces from a source with bounds b. Orientations 5–8 reflect
// across a diagonal, swapping the axes.
func orientedRect(b image.Rectangle, orientation int) image.Rectangle {
	if orientation >= 5 {
		return image.Rect(0, 0, b.Dy(), b.Dx())
	}
	return image.Rect(0, 0, b.Dx(), b.Dy())
}

// destXY returns the destination coordinate that source pixel (x, y) — given
// in local [0,w)×[0,h) coordinates — lands on under the given orientation.
func destXY(orientation, x, y, w, h int) (int, int) {
	switch orientation {
	case 2: // mirror horizontal
		return w - 1 - x, y
	case 3: // rotate 180°
		return w - 1 - x, h - 1 - y
	case 4: // mirror vertical
		return x, h - 1 - y
	case 5: // transpose (main diagonal)
		return y, x
	case 6: // rotate 90° CW
		return h - 1 - y, x
	case 7: // transverse (anti-diagonal)
		return h - 1 - y, w - 1 - x
	case 8: // rotate 90° CCW
		return y, w - 1 - x
	}
	return x, y
}

// remapPix copies 4-byte pixels from a packed source (spix/sstride, the top-left
// pixel at spix[0]) into a packed destination (dpix/dstride, sized per
// orientedRect and origin-anchored) under the given orientation. It handles the
// RGBA and NRGBA cases identically because both share the same 4-byte layout;
// copying raw bytes preserves the exact colour, so this is a pure spatial remap.
func remapPix(spix []uint8, sstride int, dpix []uint8, dstride, w, h, orientation int) {
	for y := range h {
		so := y * sstride
		for x := range w {
			dx, dy := destXY(orientation, x, y, w, h)
			di := dy*dstride + dx*4
			copy(dpix[di:di+4], spix[so:so+4])
			so += 4
		}
	}
}
