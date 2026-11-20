package cache

import (
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// failMemoEntry writes data at path, pins the file's mtime to an exact second
// (so rewriting the file can restore the same mtime deterministically) and
// returns an Entry pointing at it.
func failMemoEntry(t *testing.T, path string, data []byte) Entry {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Add(-time.Minute).Truncate(time.Second)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return Entry{Path: path, Type: scan.TypePhoto, ThumbID: ThumbIDFor(path), ModTime: info.ModTime()}
}

// TestPathFailureMemo covers C5-06: once generation fails, Path must not retry
// the same broken source on every request. It returns the memoized error while
// the source mtime is unchanged, and Forget clears the memo so a fixed source
// is regenerated.
func TestPathFailureMemo(t *testing.T) {
	cacheDir := t.TempDir()
	store, err := NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "broken.jpg")
	e := failMemoEntry(t, src, []byte("not an image at all"))

	if _, err := store.Path(e); err == nil {
		t.Fatal("Path on garbage succeeded, want error")
	}

	// Replace the bytes with a valid JPEG but keep the same mtime: the memo is
	// keyed by thumb id + source mtime, so Path must still fail immediately
	// instead of regenerating.
	writeTestJPEG(t, src, color.RGBA{0, 128, 255, 255})
	if err := os.Chtimes(src, e.ModTime, e.ModTime); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = store.Path(e)
	if err == nil {
		t.Fatal("Path with unchanged mtime succeeded, want memoized error")
	}
	if !strings.Contains(err.Error(), "failed recently") {
		t.Fatalf("Path error = %v, want one mentioning %q", err, "failed recently")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("memoized Path took %v, want a fast failure", elapsed)
	}

	// Forget clears the memo (as well as the on-disk rendition), so the next
	// Path regenerates the now-valid file.
	store.Forget(e.ThumbID)
	got, err := store.Path(e)
	if err != nil {
		t.Fatalf("Path after Forget: %v", err)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("thumb not on disk after Forget: %v", err)
	}
}

// TestPathFailureMemoBypassedByNewerSource covers the other half of the memo
// key: a newer source mtime means the file changed, so Path retries instead of
// returning the old failure.
func TestPathFailureMemoBypassedByNewerSource(t *testing.T) {
	cacheDir := t.TempDir()
	store, err := NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "broken.jpg")
	e := failMemoEntry(t, src, []byte("still not an image"))

	if _, err := store.Path(e); err == nil {
		t.Fatal("Path on garbage succeeded, want error")
	}

	// Fix the file and bump its mtime past the memoized source mtime.
	writeTestJPEG(t, src, color.RGBA{255, 64, 0, 255})
	mod := e.ModTime.Add(2 * time.Second)
	if err := os.Chtimes(src, mod, mod); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	e.ModTime = info.ModTime()

	if _, err := store.Path(e); err != nil {
		t.Fatalf("Path with newer source mtime: %v", err)
	}
}
