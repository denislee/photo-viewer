package face

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"testing"

	"github.com/dns/photo-viewer/internal/imgfit"
)

// writeSourceThumb writes a w×h JPEG gradient to a temp file and returns its
// path. The gradient keeps every crop non-degenerate (distinct pixels) so a
// decoded crop can't be mistaken for a blank image.
func writeSourceThumb(t *testing.T, w, h int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	dst := t.TempDir() + "/src.jpg"
	f, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create source thumb: %v", err)
	}
	if err := jpeg.Encode(f, img, &jpeg.Options{Quality: 90}); err != nil {
		f.Close()
		t.Fatalf("encode source thumb: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close source thumb: %v", err)
	}
	return dst
}

// decodeDims decodes path and returns its pixel dimensions.
func decodeDims(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open crop %s: %v", path, err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode crop %s: %v", path, err)
	}
	return cfg.Width, cfg.Height
}

// TestEnsureThumbsDecodesSourceOnce is the S-15 regression: N faces of one
// source thumb must produce N correct crops from a single source decode.
func TestEnsureThumbsDecodesSourceOnce(t *testing.T) {
	src := writeSourceThumb(t, 200, 160)
	cacheDir := t.TempDir()

	faces := []FaceCrop{
		{ID: 0x11, BBox: [4]int{0, 0, 60, 60}},     // square
		{ID: 0x22, BBox: [4]int{40, 20, 120, 60}},  // landscape
		{ID: 0x33, BBox: [4]int{80, 40, 40, 100}},  // portrait
		{ID: 0x44, BBox: [4]int{150, 100, 80, 80}}, // clipped to bounds
	}

	var decodes int
	decodeSourceHook = func(string) { decodes++ }
	defer func() { decodeSourceHook = nil }()

	paths, errs := EnsureThumbs(cacheDir, src, faces)

	if decodes != 1 {
		t.Fatalf("source decoded %d times, want exactly 1", decodes)
	}
	if len(paths) != len(faces) || len(errs) != len(faces) {
		t.Fatalf("returned %d paths / %d errs, want %d each", len(paths), len(errs), len(faces))
	}

	for i, f := range faces {
		if errs[i] != nil {
			t.Fatalf("face %d: unexpected error: %v", i, errs[i])
		}
		if paths[i] != ThumbPath(cacheDir, f.ID) {
			t.Fatalf("face %d: path = %q, want %q", i, paths[i], ThumbPath(cacheDir, f.ID))
		}
		info, err := os.Stat(paths[i])
		if err != nil || info.Size() == 0 {
			t.Fatalf("face %d: crop missing or empty: %v", i, err)
		}

		// The crop must have the same non-degenerate geometry the single-face
		// path would produce: imgfit.Within(rect, FaceThumbSize) after clipping
		// the bbox to the source bounds.
		rect := image.Rect(f.BBox[0], f.BBox[1], f.BBox[0]+f.BBox[2], f.BBox[1]+f.BBox[3]).
			Intersect(image.Rect(0, 0, 200, 160))
		wantW, wantH := imgfit.Within(rect.Dx(), rect.Dy(), FaceThumbSize)
		gotW, gotH := decodeDims(t, paths[i])
		if gotW != wantW || gotH != wantH {
			t.Errorf("face %d: crop dims = %dx%d, want %dx%d", i, gotW, gotH, wantW, wantH)
		}
		if gotW < 1 || gotH < 1 {
			t.Errorf("face %d: degenerate crop %dx%d", i, gotW, gotH)
		}
	}
}

// TestEnsureThumbsSkipsCachedWithoutDecoding verifies laziness survives
// batching: a second call over fully-cached crops must not decode the source.
func TestEnsureThumbsSkipsCachedWithoutDecoding(t *testing.T) {
	src := writeSourceThumb(t, 128, 128)
	cacheDir := t.TempDir()
	faces := []FaceCrop{
		{ID: 0xA1, BBox: [4]int{0, 0, 50, 50}},
		{ID: 0xA2, BBox: [4]int{60, 60, 50, 50}},
	}

	if _, errs := EnsureThumbs(cacheDir, src, faces); errs[0] != nil || errs[1] != nil {
		t.Fatalf("first pass errored: %v", errs)
	}

	var decodes int
	decodeSourceHook = func(string) { decodes++ }
	defer func() { decodeSourceHook = nil }()

	paths, errs := EnsureThumbs(cacheDir, src, faces)
	if decodes != 0 {
		t.Fatalf("cached second pass decoded %d times, want 0", decodes)
	}
	for i := range faces {
		if errs[i] != nil {
			t.Fatalf("face %d: unexpected error on cached pass: %v", i, errs[i])
		}
		if paths[i] != ThumbPath(cacheDir, faces[i].ID) {
			t.Fatalf("face %d: cached path = %q, want %q", i, paths[i], ThumbPath(cacheDir, faces[i].ID))
		}
	}
}

// TestEnsureThumbDelegatesToBatch checks the single-face wrapper still returns
// a usable crop (proving delegation preserves the old contract).
func TestEnsureThumbDelegatesToBatch(t *testing.T) {
	src := writeSourceThumb(t, 100, 100)
	cacheDir := t.TempDir()

	path, err := EnsureThumb(cacheDir, src, 0xDEAD, [4]int{10, 10, 40, 40})
	if err != nil {
		t.Fatalf("EnsureThumb: %v", err)
	}
	if path != ThumbPath(cacheDir, 0xDEAD) {
		t.Fatalf("path = %q, want %q", path, ThumbPath(cacheDir, 0xDEAD))
	}
	w, h := decodeDims(t, path)
	wantW, wantH := imgfit.Within(40, 40, FaceThumbSize)
	if w != wantW || h != wantH {
		t.Errorf("crop dims = %dx%d, want %dx%d", w, h, wantW, wantH)
	}
}

// TestEnsureThumbsReportsPerFaceErrors verifies a bad bbox fails only its own
// entry while its siblings still materialize from the shared decode.
func TestEnsureThumbsReportsPerFaceErrors(t *testing.T) {
	src := writeSourceThumb(t, 120, 120)
	cacheDir := t.TempDir()
	faces := []FaceCrop{
		{ID: 0xB1, BBox: [4]int{0, 0, 40, 40}},     // ok
		{ID: 0xB2, BBox: [4]int{0, 0, 0, 40}},      // zero width
		{ID: 0xB3, BBox: [4]int{500, 500, 40, 40}}, // no intersection
		{ID: 0xB4, BBox: [4]int{80, 80, 40, 40}},   // ok
	}

	var decodes int
	decodeSourceHook = func(string) { decodes++ }
	defer func() { decodeSourceHook = nil }()

	paths, errs := EnsureThumbs(cacheDir, src, faces)
	if decodes != 1 {
		t.Fatalf("source decoded %d times, want 1", decodes)
	}
	if errs[0] != nil || errs[3] != nil {
		t.Fatalf("valid faces errored: %v / %v", errs[0], errs[3])
	}
	if errs[1] == nil || errs[2] == nil {
		t.Fatalf("bad faces did not error: %v / %v", errs[1], errs[2])
	}
	if paths[1] != "" || paths[2] != "" {
		t.Fatalf("failed faces returned non-empty paths: %q / %q", paths[1], paths[2])
	}
	for _, i := range []int{0, 3} {
		if _, err := os.Stat(paths[i]); err != nil {
			t.Fatalf("valid face %d crop missing: %v", i, err)
		}
	}
}
