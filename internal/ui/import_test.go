package ui

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
)

// TestImportCopyProgressInstrumentation locks the U-09 contract for the
// phase-3 Inbox-copy loop: it drives the progress bar with the exact helper
// sequence the loop uses — setProgress(0, len(files)) up front, then one
// bumpProgress per file (whether the copy succeeds or errors) — so the bar
// climbs to len(files) instead of holding the previous batch's stale terminal
// value. The loop itself lives inside runImport (goroutine + exiftool via
// processBatch), so it isn't hermetically unit-testable; this asserts the
// instrumentation invariant it depends on. No process is registered, so the
// helpers fall back to scheduleInvalidate → invalidate.
func TestImportCopyProgressInstrumentation(t *testing.T) {
	const nFiles = 7
	var invalidations int
	v := &ImportView{invalidate: func() { invalidations++ }}

	v.setProgress(0, int64(nFiles))
	if got := atomic.LoadInt64(&v.progressMax); got != nFiles {
		t.Fatalf("progressMax after setProgress = %d, want %d", got, nFiles)
	}
	if got := atomic.LoadInt64(&v.progressDone); got != 0 {
		t.Fatalf("progressDone after setProgress = %d, want 0", got)
	}

	// One bump per file, mirroring the loop's success and copy-error paths.
	for range nFiles {
		v.bumpProgress()
	}

	if got := atomic.LoadInt64(&v.progressDone); got != nFiles {
		t.Errorf("progressDone after %d bumps = %d, want %d", nFiles, got, nFiles)
	}
	if got := atomic.LoadInt64(&v.progressMax); got != nFiles {
		t.Errorf("progressMax drifted to %d, want %d", got, nFiles)
	}
	// Each helper wakes the frame loop (no registry wired ⇒ direct invalidate);
	// the bar can't visibly advance without at least one redraw per update.
	if invalidations < nFiles {
		t.Errorf("invalidate called %d times, want ≥ %d (bar would look frozen)", invalidations, nFiles)
	}
}

// errAfterReader yields up to fail good bytes, then fails — simulating a
// truncated read (yanked SD card, ZIP corruption) partway through a copy.
type errAfterReader struct {
	data []byte
	pos  int
	fail int
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if r.pos >= r.fail {
		return 0, errors.New("injected read failure")
	}
	n := copy(p, r.data[r.pos:min(r.fail, len(r.data))])
	r.pos += n
	return n, nil
}

// TestWriteFileDurableSuccess verifies the happy path publishes the full
// content atomically and leaves no .tmp sidecar behind.
func TestWriteFileDurableSuccess(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "IMG_0001.jpg")
	want := []byte("the full media file contents")

	if err := writeFileDurable(dst, bytes.NewReader(want)); err != nil {
		t.Fatalf("writeFileDurable: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("dst content = %q, want %q", got, want)
	}
	if _, err := os.Stat(dst + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp sidecar was left behind: %v", err)
	}
}

// TestWriteFileDurableNoPartialOnError is the crash-safety contract behind U-01:
// a write that fails midway must never leave a truncated file at the final name
// (which the next inbox walk would file into the library as valid media), and
// must clean up its .tmp too.
func TestWriteFileDurableNoPartialOnError(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "IMG_0002.jpg")

	r := &errAfterReader{data: []byte("partial-then-boom-more-bytes"), fail: 8}
	err := writeFileDurable(dst, r)
	if err == nil {
		t.Fatal("expected writeFileDurable to return the injected error, got nil")
	}
	if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
		t.Errorf("truncated file left at final name %s (stat err: %v)", dst, statErr)
	}
	if _, statErr := os.Stat(dst + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("partial .tmp left behind (stat err: %v)", statErr)
	}
}

// TestInboxHasFiles locks the U-10 cheap existence check: inboxHasFiles reports
// true when the inbox holds at least one importable media file (only DetectType
// != TypeUnknown counts), false for an empty inbox or one holding only
// non-media/hidden leftovers. It replaces the full recursive inboxFileCount at
// the Start-click decision sites, so it must agree with them on the ≥1-file
// question.
func TestInboxHasFiles(t *testing.T) {
	write := func(t *testing.T, path string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	t.Run("empty dir", func(t *testing.T) {
		if inboxHasFiles(t.TempDir()) {
			t.Error("inboxHasFiles on an empty dir = true, want false")
		}
	})

	t.Run("missing dir", func(t *testing.T) {
		if inboxHasFiles(filepath.Join(t.TempDir(), "nope")) {
			t.Error("inboxHasFiles on a missing dir = true, want false")
		}
	})

	t.Run("only non-media and hidden files", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".DS_Store"))
		write(t, filepath.Join(dir, "notes.txt"))
		write(t, filepath.Join(dir, "sub", "readme.md"))
		if inboxHasFiles(dir) {
			t.Error("inboxHasFiles with only non-media files = true, want false")
		}
	})

	t.Run("one media file at root", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "IMG_0001.jpg"))
		if !inboxHasFiles(dir) {
			t.Error("inboxHasFiles with a media file = false, want true")
		}
	})

	t.Run("media file nested among many non-media files", func(t *testing.T) {
		dir := t.TempDir()
		write(t, filepath.Join(dir, ".DS_Store"))
		for i := range 50 {
			write(t, filepath.Join(dir, "junk", "f"+strconv.Itoa(i)+".txt"))
		}
		write(t, filepath.Join(dir, "deep", "sub", "clip.mp4"))
		if !inboxHasFiles(dir) {
			t.Error("inboxHasFiles with a nested media file = false, want true")
		}
	})
}

// TestSyncDir confirms the U-13 helper opens and fsyncs a real directory and
// surfaces an error for a missing one (so a move never unlinks its source on a
// silently-failed dir sync). The durability itself is a crash-consistency
// property, verified by review against internal/export's moveFile.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir on a real dir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("syncDir on a missing dir: want error, got nil")
	}
}
