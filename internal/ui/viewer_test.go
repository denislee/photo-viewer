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
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gioui.org/op/paint"

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
	writeJPEGWithOrientationTB(t, path, w, h, orientation)
}

func writeJPEGWithOrientationTB(t testing.TB, path string, w, h int, orientation byte) {
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
// decodeOriginalSize runs decodeOriginal for a large window, the old
// fixed-cap behaviour these orientation tests were written against.
func decodeOriginalSize(ctx context.Context, e cache.Entry) (paint.ImageOp, image.Point, bool) {
	d, ok := decodeOriginal(ctx, e, image.Pt(4096, 4096), nil)
	return d.op, d.sz, ok
}

func TestDecodeOriginalAppliesOrientation(t *testing.T) {
	dir := t.TempDir()

	upright := filepath.Join(dir, "upright.jpg")
	writeJPEGWithOrientation(t, upright, 40, 20, 1)
	_, size, ok := decodeOriginalSize(context.Background(), cache.Entry{Path: upright, Type: scan.TypePhoto})
	if !ok {
		t.Fatal("decodeOriginal failed on upright fixture")
	}
	if size.X != 40 || size.Y != 20 {
		t.Fatalf("upright size = %dx%d, want 40x20", size.X, size.Y)
	}

	rotated := filepath.Join(dir, "rotated.jpg")
	writeJPEGWithOrientation(t, rotated, 40, 20, 6)
	_, size, ok = decodeOriginalSize(context.Background(), cache.Entry{Path: rotated, Type: scan.TypePhoto})
	if !ok {
		t.Fatal("decodeOriginal failed on rotated fixture")
	}
	// Orientation 6 transposes the axes: stored 40x20 must display as 20x40.
	if size.X != 20 || size.Y != 40 {
		t.Fatalf("rotated size = %dx%d, want 20x40 (orientation not applied)", size.X, size.Y)
	}
}

// TestDecodeOriginalOrientsRAWPreview is the S-21 regression guard: a portrait
// RAW whose embedded preview is stored landscape must display transposed, like
// its grid thumbnail. The JPEG-bodied ".nef" fixture takes LoadRAWPreview's
// whole-file fast path, so no exiftool is needed.
func TestDecodeOriginalOrientsRAWPreview(t *testing.T) {
	raw := filepath.Join(t.TempDir(), "portrait.nef")
	writeJPEGWithOrientation(t, raw, 40, 20, 6)
	_, size, ok := decodeOriginalSize(context.Background(), cache.Entry{Path: raw, Type: scan.TypeRAW})
	if !ok {
		t.Fatal("decodeOriginal failed on RAW fixture")
	}
	if size.X != 20 || size.Y != 40 {
		t.Fatalf("RAW size = %dx%d, want 20x40 (orientation not applied)", size.X, size.Y)
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
		inflight := len(v.inflight)
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

// cacheHas reports whether path is in the recent cache without promoting it.
func (v *Viewer) cacheHas(path string) bool {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	for _, r := range v.recent {
		if r.path == path {
			return true
		}
	}
	return false
}

func prefetchingLen(v *Viewer) int {
	v.recentMu.Lock()
	defer v.recentMu.Unlock()
	return len(v.inflight)
}

// TestPrefetchVideoBoundedConcurrency is the G-10 guard: neighbour-video
// prefetch must respect the prefetchSem bound instead of running one 16 MB
// read per neighbour at once. Each entry is a FIFO whose reader blocks in
// os.Open until a writer appears, so a goroutine that acquires a slot stays
// parked holding it. Requests past the bound queue (V6: they used to be
// dropped) rather than reading, so exactly viewerPrefetchSlots slots are held
// while every request stays registered.
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
		deadline := time.Now().Add(5 * time.Second)
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

	deadline := time.Now().Add(2 * time.Second)
	for len(v.prefetchSem) < viewerPrefetchSlots && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// Confirm the bound is stable and nothing was dropped.
	time.Sleep(30 * time.Millisecond)
	if got := len(v.prefetchSem); got != viewerPrefetchSlots {
		t.Fatalf("held prefetch slots = %d, want %d", got, viewerPrefetchSlots)
	}
	if got := prefetchingLen(&v); got != n {
		t.Fatalf("registered video prefetches = %d, want all %d (queued, not dropped)", got, n)
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

// TestConfirmDeletePrefetchesNeighbours is the G-15 regression: after a
// delete the viewer lands on a new entry whose neighbours were never
// prefetched, so the next step fell back to the blurry thumbnail.
// ConfirmDelete now prefetches around the new position like Show (+1, −1, +2).
func TestConfirmDeletePrefetchesNeighbours(t *testing.T) {
	dir := t.TempDir()
	var entries []cache.Entry
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg"} {
		p := filepath.Join(dir, name)
		writeJPEGWithOrientation(t, p, 8, 8, 1)
		entries = append(entries, cache.Entry{Path: p, Type: scan.TypePhoto})
	}
	v := &Viewer{}
	waitCached := func(path string) bool {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
			if v.cacheHas(path) {
				return true
			}
			time.Sleep(2 * time.Millisecond)
		}
		return false
	}

	v.Show(entries, 1) // on b: prefetches c, a and d
	if !waitCached(entries[0].Path) || !waitCached(entries[2].Path) || !waitCached(entries[3].Path) {
		t.Fatal("Show did not prefetch its neighbours")
	}
	if v.cacheHas(entries[4].Path) {
		t.Fatal("e prefetched before the delete")
	}

	v.Confirming = true
	v.ConfirmDelete(func(string) {}) // now on c: +2 is e
	if v.Index != 1 || v.entries[v.Index].Path != entries[2].Path {
		t.Fatalf("after delete on %d (%s), want c", v.Index, v.entries[v.Index].Path)
	}
	if !waitCached(entries[4].Path) {
		t.Error("ConfirmDelete did not prefetch the new neighbours")
	}
}

// TestKickDecodeAdoptsInflightPrefetch is the V4 guard: a main load for a path
// that is already being prefetched waits for that prefetch instead of starting
// a second decode. The path doesn't exist, so an independent decode would fail
// and never invalidate; only the adopted prefetch's result can.
func TestKickDecodeAdoptsInflightPrefetch(t *testing.T) {
	var v Viewer
	v.init()
	var invalidated atomic.Int32
	v.SetInvalidate(func() { invalidated.Add(1) })
	e := cache.Entry{Path: "/nonexistent/v4.jpg", Type: scan.TypePhoto}

	release := make(chan struct{})
	job := v.startPrefetch(e, -1, 0, func(context.Context) {
		<-release
		img := image.NewRGBA(image.Rect(0, 0, 4, 3))
		v.cachePut(e, decodedImage{op: paint.NewImageOp(img), sz: img.Rect.Size(), native: img.Rect.Size(), full: true})
	})
	if job == nil {
		t.Fatal("startPrefetch returned nil")
	}
	for deadline := time.Now().Add(2 * time.Second); !job.started.Load() && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	v.kickDecode(e, image.Pt(100, 100), 0)
	if !job.adopted.Load() {
		t.Fatal("main load did not adopt the in-flight prefetch")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for invalidated.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if invalidated.Load() == 0 {
		t.Fatal("main load never picked up the prefetch result")
	}
	if r, ok := v.cacheGet(e); !ok || r.sz != image.Pt(4, 3) {
		t.Fatalf("cache = %+v, %v; want the prefetched 4x3 image", r.sz, ok)
	}
}

// TestRecentSurvivesCloseAndTracksMtime covers V7: closing the viewer keeps
// decoded images for an instant reopen, but a source whose mtime changed is a
// miss rather than a stale hit.
func TestRecentSurvivesCloseAndTracksMtime(t *testing.T) {
	var v Viewer
	v.init()
	mod := time.Unix(1700000000, 0)
	e := cache.Entry{Path: "/x/a.jpg", Type: scan.TypePhoto, ModTime: mod}
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	v.cachePut(e, decodedImage{op: paint.NewImageOp(img), sz: image.Pt(2, 2)})
	v.Open = true
	v.Close()
	if _, ok := v.cacheGet(e); !ok {
		t.Fatal("Close dropped the recent cache")
	}
	changed := e
	changed.ModTime = mod.Add(time.Second)
	if _, ok := v.cacheGet(changed); ok {
		t.Fatal("cache served an image decoded from an older version of the file")
	}
	if v.cacheHas(e.Path) {
		t.Fatal("stale entry was not dropped")
	}
}

// TestDecodeOriginalFitsWindow covers V2: the decode is sized for the image
// area (within 2× per axis), not the 4096 px cap, and remembers the native
// size so a bigger window can trigger a sharper re-decode.
func TestDecodeOriginalFitsWindow(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.jpg")
	writeJPEGWithOrientation(t, p, 1600, 1200, 6) // upright 1200x1600
	e := cache.Entry{Path: p, Type: scan.TypePhoto}
	target := image.Pt(300, 200) // fit: 150x200
	d, ok := decodeOriginal(context.Background(), e, target, nil)
	if !ok {
		t.Fatal("decode failed")
	}
	if d.sz.X < 150 || d.sz.Y < 200 || d.sz.X > 300 || d.sz.Y > 400 {
		t.Fatalf("decoded %v for a 150x200 fit", d.sz)
	}
	if d.sz.Y <= d.sz.X {
		t.Fatalf("decoded %v is not portrait (orientation lost)", d.sz)
	}
	r := recentImage{sz: d.sz, native: d.native, full: d.full}
	if r.needsUpgrade(target) {
		t.Fatal("needsUpgrade for the target it was decoded for")
	}
	if !r.needsUpgrade(image.Pt(1200, 1600)) {
		t.Fatalf("no upgrade for a larger window (native %v, full %v)", d.native, d.full)
	}
}

func TestFitInside(t *testing.T) {
	cases := []struct{ sz, box, want image.Point }{
		{image.Pt(4032, 3024), image.Pt(1920, 1080), image.Pt(1440, 1080)},
		{image.Pt(3024, 4032), image.Pt(1920, 1080), image.Pt(810, 1080)},
		{image.Pt(100, 50), image.Pt(1920, 1080), image.Pt(100, 50)},
		{image.Pt(4000, 1000), image.Pt(1000, 1000), image.Pt(1000, 250)},
	}
	for _, c := range cases {
		if got := fitInside(c.sz, c.box); got != c.want {
			t.Errorf("fitInside(%v, %v) = %v, want %v", c.sz, c.box, got, c.want)
		}
	}
}

// BenchmarkDecodeOriginal measures opening a 12 MP photo in a 1440p window,
// landscape and portrait (EXIF orientation 6).
func BenchmarkDecodeOriginal(b *testing.B) {
	dir := b.TempDir()
	for _, o := range []byte{1, 6} {
		p := filepath.Join(dir, fmt.Sprintf("o%d.jpg", o))
		writeJPEGWithOrientationTB(b, p, 4032, 3024, o)
		e := cache.Entry{Path: p, Type: scan.TypePhoto}
		b.Run(fmt.Sprintf("orientation-%d", o), func(b *testing.B) {
			for b.Loop() {
				if _, ok := decodeOriginal(context.Background(), e, image.Pt(2560, 1440), nil); !ok {
					b.Fatal("decode failed")
				}
			}
		})
	}
}
