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
// The transform is a plain per-pixel remap via At/Set so it works for any
// image.Image. That is O(w·h) with interface calls — fine for the one-shot
// export/thumbnail use here, and the hot orientation-1 path is free.
func Apply(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()

	// Orientations 5–8 rotate/reflect across a diagonal, which swaps the axes;
	// the destination is then h wide and w tall.
	var dst *image.RGBA
	if orientation >= 5 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// (dx, dy) is where source pixel (x, y) lands after the transform.
			var dx, dy int
			switch orientation {
			case 2: // mirror horizontal
				dx, dy = w-1-x, y
			case 3: // rotate 180°
				dx, dy = w-1-x, h-1-y
			case 4: // mirror vertical
				dx, dy = x, h-1-y
			case 5: // transpose (main diagonal)
				dx, dy = y, x
			case 6: // rotate 90° CW
				dx, dy = h-1-y, x
			case 7: // transverse (anti-diagonal)
				dx, dy = h-1-y, w-1-x
			case 8: // rotate 90° CCW
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
