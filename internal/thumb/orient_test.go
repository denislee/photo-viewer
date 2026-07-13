package thumb

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// exifAPP1 builds a minimal EXIF APP1 segment carrying a single Orientation
// tag (big-endian TIFF, one-entry IFD0 with the SHORT value in the high bytes).
// It mirrors the fixture machinery in internal/ui/viewer_test.go; goexif reads
// only the Orientation tag from it, which is all imgorient.ReadOrientation
// consults.
func exifAPP1(orientation byte) []byte {
	payload := []byte{
		'E', 'x', 'i', 'f', 0, 0, // EXIF identifier
		'M', 'M', // big-endian
		0x00, 0x2A, // TIFF magic 42
		0x00, 0x00, 0x00, 0x08, // offset to IFD0
		0x00, 0x01, // one directory entry
		0x01, 0x12, // tag = Orientation
		0x00, 0x03, // type = SHORT
		0x00, 0x00, 0x00, 0x01, // count = 1
		0x00, orientation, 0x00, 0x00, // value (SHORT in high bytes)
		0x00, 0x00, 0x00, 0x00, // next-IFD offset = 0
	}
	segLen := len(payload) + 2 // +2 for the length field itself
	seg := []byte{0xFF, 0xE1, byte(segLen >> 8), byte(segLen)}
	return append(seg, payload...)
}

// writeJPEGWithOrientation writes a w×h JPEG to path with the given EXIF
// Orientation spliced in right after the SOI marker. Kept well under
// imageFfmpegThreshold so Image() takes the in-process decode + writeThumb
// path deterministically (no ffmpeg dependency).
func writeJPEGWithOrientation(t *testing.T, path string, w, h int, orientation byte) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	raw := buf.Bytes()
	if len(raw) < 2 || raw[0] != 0xFF || raw[1] != 0xD8 {
		t.Fatalf("unexpected jpeg header %x", raw[:2])
	}
	out := make([]byte, 0, len(raw)+40)
	out = append(out, raw[:2]...) // SOI
	out = append(out, exifAPP1(orientation)...)
	out = append(out, raw[2:]...) // rest of the JPEG
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatalf("write jpeg: %v", err)
	}
}

// thumbSize decodes the JPEG at path and returns its dimensions.
func thumbSize(t *testing.T, path string) (int, int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open thumb: %v", err)
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		t.Fatalf("decode thumb config: %v", err)
	}
	return cfg.Width, cfg.Height
}

// TestImageAppliesOrientationAfterScale is S-04's end-to-end guard for the
// scale-then-orient reordering: a landscape-stored photo with Orientation=6
// (rotate 90° CW) must thumbnail to a *portrait* (transposed) image, while an
// upright control stays landscape. A regression that dropped the orientation,
// or fed imgfit.Within the wrong axes, would flip one of these.
func TestImageAppliesOrientationAfterScale(t *testing.T) {
	dir := t.TempDir()

	// Upright control: 80×40 landscape stays landscape after thumbnailing.
	upright := filepath.Join(dir, "upright.jpg")
	writeJPEGWithOrientation(t, upright, 80, 40, 1)
	dstU := filepath.Join(dir, "upright-thumb.jpg")
	if err := Image(context.Background(), upright, dstU, 64); err != nil {
		t.Fatalf("Image(upright): %v", err)
	}
	if w, h := thumbSize(t, dstU); w <= h {
		t.Fatalf("upright thumbnail = %dx%d, want landscape (w>h)", w, h)
	}

	// Orientation 6 transposes the axes: the same 80×40 source must come out
	// portrait (h>w) once the orientation is baked in after the downscale.
	rotated := filepath.Join(dir, "rotated.jpg")
	writeJPEGWithOrientation(t, rotated, 80, 40, 6)
	dstR := filepath.Join(dir, "rotated-thumb.jpg")
	if err := Image(context.Background(), rotated, dstR, 64); err != nil {
		t.Fatalf("Image(rotated): %v", err)
	}
	if w, h := thumbSize(t, dstR); h <= w {
		t.Fatalf("rotated thumbnail = %dx%d, want portrait (h>w); orientation not applied", w, h)
	}
}
