package imgorient

import (
	"image"
	"image/color"
	"testing"
)

// makeImage builds a w×h RGBA image whose every pixel encodes its own (x, y)
// coordinates in the R and G channels, so a transformed copy can be checked
// pixel-for-pixel against the expected remap.
func makeImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0, A: 255})
		}
	}
	return img
}

// TestApplyIdentity: orientation 1 (and out-of-range values) must return the
// input untouched with no allocation-driven change in bounds or pixels.
func TestApplyIdentity(t *testing.T) {
	src := makeImage(2, 3)
	for _, o := range []int{0, 1, 9, -1} {
		got := Apply(src, o)
		if got.Bounds() != src.Bounds() {
			t.Fatalf("orientation %d: bounds = %v, want %v", o, got.Bounds(), src.Bounds())
		}
		if got.At(1, 2) != src.At(1, 2) {
			t.Fatalf("orientation %d: pixel changed on identity", o)
		}
	}
}

// TestApplyDimensionSwap: the four transpose orientations (5–8) must swap width
// and height; the other in-plane transforms (2–4) must preserve them.
func TestApplyDimensionSwap(t *testing.T) {
	src := makeImage(2, 3) // non-square so a swap is observable
	swaps := map[int]bool{5: true, 6: true, 7: true, 8: true}
	for o := 2; o <= 8; o++ {
		got := Apply(src, o).Bounds()
		wantW, wantH := 2, 3
		if swaps[o] {
			wantW, wantH = 3, 2
		}
		if got.Dx() != wantW || got.Dy() != wantH {
			t.Fatalf("orientation %d: size = %dx%d, want %dx%d", o, got.Dx(), got.Dy(), wantW, wantH)
		}
	}
}

// TestApplyPixelRemap verifies the exact per-pixel mapping for a representative
// transform from each family: a mirror (2), a 180° rotation (3), a clockwise
// quarter-turn (6) and a counter-clockwise one (8). For every source pixel we
// assert it lands where the EXIF transform says it should, which also proves
// the corner placement the spec asks for.
func TestApplyPixelRemap(t *testing.T) {
	const w, h = 2, 3
	src := makeImage(w, h)

	cases := []struct {
		orientation int
		// remap returns the expected destination coord of source (x, y).
		remap func(x, y int) (int, int)
	}{
		{2, func(x, y int) (int, int) { return w - 1 - x, y }},         // mirror horizontal
		{3, func(x, y int) (int, int) { return w - 1 - x, h - 1 - y }}, // rotate 180°
		{6, func(x, y int) (int, int) { return h - 1 - y, x }},         // rotate 90° CW
		{8, func(x, y int) (int, int) { return y, w - 1 - x }},         // rotate 90° CCW
	}
	for _, c := range cases {
		dst := Apply(src, c.orientation)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				dx, dy := c.remap(x, y)
				if got, want := dst.At(dx, dy), src.At(x, y); got != want {
					t.Fatalf("orientation %d: src(%d,%d) expected at dst(%d,%d): got %v want %v",
						c.orientation, x, y, dx, dy, got, want)
				}
			}
		}
	}
}

// TestApplyFlipCorner is the spec's explicit "a flip maps a known corner pixel"
// check: under orientation 2 (mirror horizontal) the top-left pixel must move
// to the top-right.
func TestApplyFlipCorner(t *testing.T) {
	src := makeImage(4, 1)
	topLeft := src.At(0, 0)
	dst := Apply(src, 2)
	if got := dst.At(3, 0); got != topLeft {
		t.Fatalf("mirror horizontal: top-left pixel = %v at (3,0), want %v", got, topLeft)
	}
}
