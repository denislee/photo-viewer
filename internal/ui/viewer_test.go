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
	"syscall"
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

// mkfifo creates a named pipe under dir. Reading it blocks in os.Open until a
// writer opens the other end, so a prefetch goroutine that reaches the read
// stays parked — letting these tests observe how many run concurrently.
func mkfifo(t *testing.T, dir string, i int) string {
	t.Helper()
	p := filepath.Join(dir, fmt.Sprintf("clip-%d.mp4", i))
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	return p
}

func prefetchingLen(v *Viewer) int {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	return len(v.prefetching)
}

// TestPrefetchVideoBoundedConcurrency is the G-10 guard: neighbour-video
// prefetch must respect the 2-slot prefetchSem instead of spawning one 16 MB
// read per neighbour. Each entry is a FIFO whose reader blocks in os.Open
// until a writer appears, so a goroutine that acquires a slot stays parked
// (its prefetching entry lingers) while goroutines that can't acquire return
// immediately and clear theirs. At steady state exactly two entries remain —
// the semaphore's capacity. Without the bound all five would block.
func TestPrefetchVideoBoundedConcurrency(t *testing.T) {
	dir := t.TempDir()
	const n = 5
	var v Viewer
	v.curIdx.Store(2) // keep every idx below within the ±2 stale window
	fifos := make([]string, n)
	for i := range fifos {
		fifos[i] = mkfifo(t, dir, i)
		v.prefetchVideo(cache.Entry{Path: fifos[i], Type: scan.TypeVideo}, i)
	}

	// Unblock parked readers on the way out so no goroutine leaks past the test.
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for prefetchingLen(&v) > 0 && time.Now().Before(deadline) {
			for _, p := range fifos {
				if w, err := os.OpenFile(p, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					_, _ = w.Write([]byte{0})
					_ = w.Close()
				}
			}
			time.Sleep(2 * time.Millisecond)
		}
	})

	// Wait for the three losers to clear; the two slot-holders stay parked.
	deadline := time.Now().Add(2 * time.Second)
	for prefetchingLen(&v) > 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := prefetchingLen(&v); got != 2 {
		t.Fatalf("parked video prefetches = %d, want 2 (prefetchSem cap)", got)
	}
	// Confirm the bound is stable rather than still draining toward 0.
	time.Sleep(30 * time.Millisecond)
	if got := prefetchingLen(&v); got != 2 {
		t.Fatalf("parked video prefetches = %d after settle, want a stable 2", got)
	}
}

// TestPrefetchVideoStaleAborts is the other half of G-10: a prefetch whose
// target index has fallen outside the ±2 window must abort before touching the
// file. The entry is a FIFO with no writer, so an os.Open would block forever —
// the goroutine can only clear its prefetching entry if the stale check
// short-circuits ahead of the read.
func TestPrefetchVideoStaleAborts(t *testing.T) {
	dir := t.TempDir()
	fifo := mkfifo(t, dir, 0)
	var v Viewer
	v.curIdx.Store(100) // far from idx 0 → outside ±2
	v.prefetchVideo(cache.Entry{Path: fifo, Type: scan.TypeVideo}, 0)

	deadline := time.Now().Add(2 * time.Second)
	for prefetchingLen(&v) > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := prefetchingLen(&v); got != 0 {
		t.Fatalf("prefetching not drained (%d) — stale prefetch blocked on os.Open instead of aborting", got)
	}
}
