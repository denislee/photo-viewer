package cache

import (
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// writeTestJPEG writes a small solid-colour JPEG at path and returns it.
func writeTestJPEG(t *testing.T, path string, c color.Color) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := range 16 {
		for x := range 16 {
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(f, img, nil); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// countTmp returns the number of leftover "*.tmp" files under the store's
// thumbs directory (recursively). A correct generation renames its temp into
// place and leaves none; a failure removes its temp and leaves none.
func countTmp(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Ext(p) == ".tmp" {
			n++
		}
		return nil
	})
	return n
}

// TestPathGeneratesAndReuses covers the happy path: a photo entry generates a
// thumbnail via the in-process Go decoder (no external tools needed), the file
// lands on disk, Has reports it fresh, a second call reuses it, and no .tmp
// file is left behind.
func TestPathGeneratesAndReuses(t *testing.T) {
	cacheDir := t.TempDir()
	store, err := NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "photo.jpg")
	writeTestJPEG(t, src, color.RGBA{200, 100, 50, 255})
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	e := Entry{Path: src, Type: scan.TypePhoto, ThumbID: ThumbIDFor(src), ModTime: info.ModTime()}

	got, err := store.Path(e)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("thumb not on disk: %v", err)
	}
	if !store.Has(e) {
		t.Fatal("Has = false after generation")
	}
	if n := countTmp(t, filepath.Join(cacheDir, "thumbs")); n != 0 {
		t.Fatalf("%d leftover .tmp files after success", n)
	}

	// Second call returns the same path without regenerating.
	got2, err := store.Path(e)
	if err != nil || got2 != got {
		t.Fatalf("reuse: got (%q,%v), want (%q,nil)", got2, err, got)
	}
}

// TestPathRegeneratesStale verifies a thumb older than its source is treated as
// stale and regenerated (the stale-remove now happens inside the singleflight).
func TestPathRegeneratesStale(t *testing.T) {
	cacheDir := t.TempDir()
	store, err := NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "photo.jpg")
	writeTestJPEG(t, src, color.RGBA{10, 20, 30, 255})

	// First generation with an old source mtime.
	oldMod := time.Now().Add(-2 * time.Hour)
	e := Entry{Path: src, Type: scan.TypePhoto, ThumbID: ThumbIDFor(src), ModTime: oldMod}
	dst, err := store.Path(e)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	// Force the thumb's mtime to match the old source so it is unambiguously
	// stale once we bump the source's mtime.
	if err := os.Chtimes(dst, oldMod, oldMod); err != nil {
		t.Fatal(err)
	}

	// Source now appears newer than the cached thumb → must regenerate. Use a
	// time safely in the past: file mtimes come from the kernel's coarse clock,
	// so a thumb written right after time.Now() can carry an mtime a few µs
	// *before* it and read as stale.
	newMod := time.Now().Add(-time.Hour)
	e2 := Entry{Path: src, Type: scan.TypePhoto, ThumbID: ThumbIDFor(src), ModTime: newMod}
	if store.Has(e2) {
		t.Fatal("Has = true for a stale thumb")
	}
	if _, err := store.Path(e2); err != nil {
		t.Fatalf("regen Path: %v", err)
	}
	if !store.Has(e2) {
		t.Fatal("thumb still stale after regeneration")
	}
	if n := countTmp(t, filepath.Join(cacheDir, "thumbs")); n != 0 {
		t.Fatalf("%d leftover .tmp files after regen", n)
	}
}

// TestPathFailureLeavesNoTmp verifies a generation failure (an un-decodable
// "photo") surfaces an error and removes its temp file rather than leaking it.
func TestPathFailureLeavesNoTmp(t *testing.T) {
	cacheDir := t.TempDir()
	store, err := NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "broken.jpg")
	if err := os.WriteFile(src, []byte("not a real jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(src)
	e := Entry{Path: src, Type: scan.TypePhoto, ThumbID: ThumbIDFor(src), ModTime: info.ModTime()}

	if _, err := store.Path(e); err == nil {
		t.Fatal("Path succeeded on an un-decodable source, want error")
	}
	if n := countTmp(t, filepath.Join(cacheDir, "thumbs")); n != 0 {
		t.Fatalf("%d leftover .tmp files after failure", n)
	}
}

// TestSweepStaleTmp seeds a shard directory with a crash-orphaned "*.tmp" older
// than the reaper threshold, an in-flight "*.tmp" written moments ago, and a
// real thumbnail, then sweeps. Only the stale temp is removed: the fresh temp
// (a generation still in flight) and the finished .jpg both survive. This is
// the startup reclaim NewThumbStore kicks off in the background (C-07); the
// helper is driven synchronously here so the assertions don't race the
// goroutine.
func TestSweepStaleTmp(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Place a file under a shard dir with a specific age relative to now.
	place := func(rel string, age time.Duration) string {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := now.Add(-age)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return p
	}

	oldTmp := place("aa/abcd-123.tmp", 2*time.Hour)   // crash-orphaned, stale
	freshTmp := place("aa/abcd-456.tmp", time.Minute) // in-flight, keep
	realThumb := place("aa/abcd.jpg", 3*time.Hour)    // finished thumb, never touched

	sweepStaleTmp(root, staleTmpMaxAge, now)

	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	if exists(oldTmp) {
		t.Error("stale .tmp not swept")
	}
	if !exists(freshTmp) {
		t.Error("fresh .tmp wrongly swept")
	}
	if !exists(realThumb) {
		t.Error("finished .jpg wrongly swept")
	}
}

// TestSweepStaleTmpMissingRoot verifies the sweep is a no-op (no panic) when the
// store root doesn't exist yet — newStore MkdirAll's it first, but the helper
// must tolerate a missing tree regardless.
func TestSweepStaleTmpMissingRoot(t *testing.T) {
	sweepStaleTmp(filepath.Join(t.TempDir(), "does-not-exist"), staleTmpMaxAge, time.Now())
}

// TestZeroByteThumbIsNotFresh covers G4: without the fsync a crash can leave a
// zero-byte thumbnail behind the rename; it must count as missing and be
// regenerated rather than served forever.
func TestZeroByteThumbIsNotFresh(t *testing.T) {
	store, err := NewThumbStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "photo.jpg")
	writeTestJPEG(t, src, color.RGBA{10, 20, 30, 255})
	info, _ := os.Stat(src)
	e := Entry{Path: src, Type: scan.TypePhoto, ThumbID: ThumbIDFor(src), ModTime: info.ModTime()}
	p, err := store.Path(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 0); err != nil {
		t.Fatal(err)
	}
	if store.Has(e) {
		t.Fatal("zero-byte thumbnail reported fresh")
	}
	if _, err := store.Path(e); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(p); err != nil || fi.Size() == 0 {
		t.Fatalf("thumbnail not regenerated: %v, %v", fi, err)
	}
}

// TestTierFollowsForgetAndRename covers the G5 tier bookkeeping: a tier's
// renditions are removed and renamed along with the base store's, and Discard
// touches only the store it is called on.
func TestTierFollowsForgetAndRename(t *testing.T) {
	cacheDir := t.TempDir()
	store, err := NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	large, err := store.Tier(LargeThumbDir, LargeThumbSize)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := store.Tier(LargeThumbDir, LargeThumbSize); again != large {
		t.Fatal("Tier created a second store for the same subdir")
	}
	src := filepath.Join(t.TempDir(), "photo.jpg")
	writeTestJPEG(t, src, color.RGBA{10, 20, 30, 255})
	info, _ := os.Stat(src)
	e := Entry{Path: src, Type: scan.TypePhoto, ThumbID: ThumbIDFor(src), ModTime: info.ModTime()}
	for _, s := range []*ThumbStore{store, large} {
		if _, err := s.Path(e); err != nil {
			t.Fatal(err)
		}
	}
	moved := e
	moved.ThumbID = ThumbIDFor(src + ".moved")
	if err := store.Rename(e.ThumbID, moved.ThumbID); err != nil {
		t.Fatal(err)
	}
	if !large.Has(moved) || large.Has(e) {
		t.Fatal("tier rendition did not follow Rename")
	}
	large.Discard(moved.ThumbID)
	if !store.Has(moved) {
		t.Fatal("Discard on the tier removed the base thumbnail")
	}
	if _, err := large.Path(moved); err != nil {
		t.Fatal(err)
	}
	store.Forget(moved.ThumbID)
	if store.Has(moved) || large.Has(moved) {
		t.Fatal("Forget left a rendition behind")
	}
}

// TestPathRecreatesShardAfterRemoveAll verifies Path recreates a shard
// directory that vanished under it (e.g. the cache was wiped mid-session):
// CreateTemp reports the missing dir, Path MkdirAll's it and retries, so the
// generation succeeds instead of failing on a missing directory.
func TestPathRecreatesShardAfterRemoveAll(t *testing.T) {
	cacheDir := t.TempDir()
	store, err := NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "photo.jpg")
	writeTestJPEG(t, src, color.RGBA{10, 20, 30, 255})
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	e := Entry{Path: src, Type: scan.TypePhoto, ThumbID: ThumbIDFor(src), ModTime: info.ModTime()}
	if _, err := store.Path(e); err != nil {
		t.Fatalf("Path: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(cacheDir, "thumbs")); err != nil {
		t.Fatal(err)
	}

	got, err := store.Path(e)
	if err != nil {
		t.Fatalf("Path after RemoveAll: %v", err)
	}
	fi, err := os.Stat(got)
	if err != nil {
		t.Fatalf("thumb not recreated: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("recreated thumb is empty")
	}
}

// TestTierSizeMismatch covers the size guard: an existing subdir is returned
// unchanged for a repeat request at the same size, and a request at a different
// size is an error rather than silently reusing a store that renders another
// size.
func TestTierSizeMismatch(t *testing.T) {
	store, err := NewThumbStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Tier("x", 512)
	if err != nil {
		t.Fatalf("Tier: %v", err)
	}
	again, err := store.Tier("x", 512)
	if err != nil {
		t.Fatalf("Tier repeat: %v", err)
	}
	if again != first {
		t.Fatal("Tier created a second store for the same subdir and size")
	}
	if _, err := store.Tier("x", 300); err == nil {
		t.Fatal("Tier with a different size returned nil error")
	}
}

// TestTierSharesSemaphores verifies a tier reuses its parent's generation
// semaphores so every size together stays within one CPU / external-tool bound.
func TestTierSharesSemaphores(t *testing.T) {
	store, err := NewThumbStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t512, err := store.Tier("x", 512)
	if err != nil {
		t.Fatalf("Tier: %v", err)
	}
	if t512.cpuSem != store.cpuSem {
		t.Fatal("tier does not share the parent cpuSem")
	}
	if t512.extSem != store.extSem {
		t.Fatal("tier does not share the parent extSem")
	}
}
