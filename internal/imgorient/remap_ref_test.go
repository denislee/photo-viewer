package imgorient

import (
	"bytes"
	"image"
	"math/rand/v2"
	"strconv"
	"testing"
)

// destXY is the reference mapping from a source pixel (x, y) in a w×h image to
// its destination pixel under an EXIF orientation. It is the plain switch the
// pixel-by-pixel implementation used before remapPix derived per-orientation
// affine steps; tests use it as the ground truth the production remap must
// reproduce exactly.
func destXY(orientation, x, y, w, h int) (int, int) {
	switch orientation {
	case 2: // mirror horizontal
		return w - 1 - x, y
	case 3: // rotate 180°
		return w - 1 - x, h - 1 - y
	case 4: // mirror vertical
		return x, h - 1 - y
	case 5: // transpose
		return y, x
	case 6: // rotate 90° CW
		return h - 1 - y, x
	case 7: // transverse
		return h - 1 - y, w - 1 - x
	case 8: // rotate 90° CCW
		return y, w - 1 - x
	default: // 1 (identity) and anything out of range
		return x, y
	}
}

// remapPixRef is the old, straightforward implementation of remapPix: one
// destXY switch call per pixel, no tiling and no row copies. It is kept here as
// the reference the optimised remapPix is checked byte-for-byte against.
func remapPixRef(spix []uint8, sstride int, dpix []uint8, dstride, w, h, orientation int) {
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

// fillPix fills every byte of p with deterministic pseudo-random data from a
// fixed-seed PCG, so a byte-for-byte mismatch can never be masked by runs of
// equal values.
func fillPix(p []byte) {
	r := rand.New(rand.NewPCG(1, 2))
	for i := range p {
		p[i] = uint8(r.Uint32())
	}
}

// TestRemapPixMatchesReference proves remapPix reproduces the reference
// pixel-by-pixel mapping for all eight orientations, across odd and prime sizes
// (which stress the 32×32 tiling boundaries), the 1-pixel degenerate cases, and
// both a tightly packed source and one whose Stride is wider than 4·w (the
// SubImage-of-a-larger-image layout a caller can hand us).
func TestRemapPixMatchesReference(t *testing.T) {
	sizes := [][2]int{{37, 23}, {77, 53}, {1, 1}, {1, 9}, {64, 32}, {33, 65}}
	for _, sz := range sizes {
		w, h := sz[0], sz[1]
		packed := image.NewRGBA(image.Rect(0, 0, w, h))
		fillPix(packed.Pix)
		padded := image.NewRGBA(image.Rect(0, 0, w+3, h)).SubImage(image.Rect(0, 0, w, h)).(*image.RGBA)
		fillPix(padded.Pix)
		sources := map[string]*image.RGBA{"packed": packed, "padded": padded}

		for name, src := range sources {
			for o := 1; o <= 8; o++ {
				rect := orientedRect(src.Rect, o)
				got := image.NewRGBA(rect)
				want := image.NewRGBA(rect)
				remapPix(src.Pix, src.Stride, got.Pix, got.Stride, w, h, o)
				remapPixRef(src.Pix, src.Stride, want.Pix, want.Stride, w, h, o)
				if !bytes.Equal(got.Pix, want.Pix) {
					t.Fatalf("size %dx%d orientation %d layout %s: remapPix output differs from reference",
						w, h, o, name)
				}
			}
		}
	}
}

// TestApplyMatchesReference checks the public entry point end to end: for both
// packed image types Apply handles specially, its returned pixels must equal the
// reference remap written into a freshly oriented destination of the same type.
func TestApplyMatchesReference(t *testing.T) {
	sizes := [][2]int{{37, 23}, {77, 53}}
	for _, sz := range sizes {
		w, h := sz[0], sz[1]

		nrgba := image.NewNRGBA(image.Rect(0, 0, w, h))
		fillPix(nrgba.Pix)
		rgba := image.NewRGBA(image.Rect(0, 0, w, h))
		fillPix(rgba.Pix)

		for o := 2; o <= 8; o++ {
			gotN := Apply(nrgba, o)
			n, ok := gotN.(*image.NRGBA)
			if !ok {
				t.Fatalf("NRGBA size %dx%d orientation %d: got %T, want *image.NRGBA", w, h, o, gotN)
			}
			wantN := image.NewNRGBA(orientedRect(nrgba.Rect, o))
			remapPixRef(nrgba.Pix, nrgba.Stride, wantN.Pix, wantN.Stride, w, h, o)
			if !bytes.Equal(n.Pix, wantN.Pix) {
				t.Fatalf("NRGBA size %dx%d orientation %d: Apply output differs from reference", w, h, o)
			}

			gotR := Apply(rgba, o)
			r, ok := gotR.(*image.RGBA)
			if !ok {
				t.Fatalf("RGBA size %dx%d orientation %d: got %T, want *image.RGBA", w, h, o, gotR)
			}
			wantR := image.NewRGBA(orientedRect(rgba.Rect, o))
			remapPixRef(rgba.Pix, rgba.Stride, wantR.Pix, wantR.Stride, w, h, o)
			if !bytes.Equal(r.Pix, wantR.Pix) {
				t.Fatalf("RGBA size %dx%d orientation %d: Apply output differs from reference", w, h, o)
			}
		}
	}
}

// BenchmarkRemapPix compares the production remap against the reference
// implementation on a 12 MP RGBA buffer (4000×3000, the photo-viewer preview
// case) for the three non-row-copy families: a 180° rotation, a clockwise
// quarter-turn and a counter-clockwise one.
func BenchmarkRemapPix(b *testing.B) {
	const w, h = 4000, 3000
	src := image.NewRGBA(image.Rect(0, 0, w, h))
	for _, o := range []int{3, 6, 8} {
		b.Run(strconv.Itoa(o), func(b *testing.B) {
			dst := image.NewRGBA(orientedRect(src.Rect, o))
			b.SetBytes(int64(len(src.Pix)))
			b.Run("new", func(b *testing.B) {
				b.SetBytes(int64(len(src.Pix)))
				for b.Loop() {
					remapPix(src.Pix, src.Stride, dst.Pix, dst.Stride, w, h, o)
				}
			})
			b.Run("ref", func(b *testing.B) {
				b.SetBytes(int64(len(src.Pix)))
				for b.Loop() {
					remapPixRef(src.Pix, src.Stride, dst.Pix, dst.Stride, w, h, o)
				}
			})
		})
	}
}
