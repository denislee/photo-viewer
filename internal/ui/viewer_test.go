package ui

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// exifAPP1 builds a minimal EXIF APP1 segment carrying a single Orientation
// tag. goexif (and every real decoder) reads only that tag from it, which is
// all decodeOriginal/decodeDimensions consult. Big-endian ("MM") TIFF layout:
// a one-entry IFD0 whose SHORT value sits in the high 2 bytes of the value
// field.
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
// Orientation spliced in right after the SOI marker.
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

// TestDecodeOriginalAppliesOrientation is the regression guard for G-01: a
// portrait photo stored landscape with Orientation=6 (rotate 90° CW) must come
// out of decodeOriginal transposed, matching the grid thumbnail instead of
// rendering sideways.
func TestDecodeOriginalAppliesOrientation(t *testing.T) {
	dir := t.TempDir()

	upright := filepath.Join(dir, "upright.jpg")
	writeJPEGWithOrientation(t, upright, 40, 20, 1)
	op, size, ok := decodeOriginal(context.Background(), cache.Entry{Path: upright, Type: scan.TypePhoto})
	if !ok {
		t.Fatal("decodeOriginal failed on upright fixture")
	}
	if size.X != 40 || size.Y != 20 {
		t.Fatalf("upright size = %dx%d, want 40x20", size.X, size.Y)
	}
	_ = op

	rotated := filepath.Join(dir, "rotated.jpg")
	writeJPEGWithOrientation(t, rotated, 40, 20, 6)
	_, size, ok = decodeOriginal(context.Background(), cache.Entry{Path: rotated, Type: scan.TypePhoto})
	if !ok {
		t.Fatal("decodeOriginal failed on rotated fixture")
	}
	// Orientation 6 transposes the axes: stored 40x20 must display as 20x40.
	if size.X != 20 || size.Y != 40 {
		t.Fatalf("rotated size = %dx%d, want 20x40 (orientation not applied)", size.X, size.Y)
	}
}

// TestPrefetchIndexNoRace is the -race regression guard for G-03: background
// prefetch goroutines stale-check their target against curIdx (an atomic mirror
// of Index) while the UI goroutine writes Index through Show/Next/Prev. Before
// the fix the prefetch goroutine read Index directly under a mutex no writer
// held, so -race flagged it the moment a prefetch overlapped a keypress. The
// stub paths don't exist, so decodeOriginal fails harmlessly — only the
// index-read window is exercised. Meaningful only under `go test -race`.
func TestPrefetchIndexNoRace(t *testing.T) {
	const n = 64
	entries := make([]cache.Entry, n)
	for i := range entries {
		entries[i] = cache.Entry{Path: fmt.Sprintf("/nonexistent/g03-%d.jpg", i), Type: scan.TypePhoto}
	}

	var v Viewer
	v.Show(entries, 0) // spawns the first prefetch readers

	// Walk the whole range back and forth; every step writes Index on this
	// goroutine and spawns a prefetch reader of curIdx, overlapping the two.
	for range 4 {
		for range n - 1 {
			v.Next()
		}
		for range n - 1 {
			v.Prev()
		}
	}

	// Drain in-flight prefetch goroutines so their curIdx reads land before
	// the test returns (decode of a missing path returns promptly).
	deadline := time.Now().Add(2 * time.Second)
	for {
		v.recentMu.Lock()
		inflight := len(v.prefetching)
		v.recentMu.Unlock()
		if inflight == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDecodeDimensionsSwapsForTranspose covers the info-panel follow-up: the
// reported dimensions must match what the viewer draws for a transposed image.
func TestDecodeDimensionsSwapsForTranspose(t *testing.T) {
	dir := t.TempDir()

	upright := filepath.Join(dir, "upright.jpg")
	writeJPEGWithOrientation(t, upright, 40, 20, 1)
	if got := decodeDimensions(upright); got != "40 × 20" {
		t.Fatalf("upright dimensions = %q, want %q", got, "40 × 20")
	}

	rotated := filepath.Join(dir, "rotated.jpg")
	writeJPEGWithOrientation(t, rotated, 40, 20, 6)
	if got := decodeDimensions(rotated); got != "20 × 40" {
		t.Fatalf("rotated dimensions = %q, want %q", got, "20 × 40")
	}
}
