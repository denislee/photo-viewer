package ui

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// syncBuffer is a mutex-guarded sink for the package logger. The refresh that
// performDeletion schedules on error runs on its own goroutine and may log
// while the test reads the captured output, so the buffer must be safe for
// concurrent access under -race.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitRefreshSettled blocks until the scheduleRefresh coalescer goroutine has
// exited, then returns the directory it last targeted. It bounds the wait so a
// stuck goroutine fails the test instead of hanging it.
func waitRefreshSettled(t *testing.T, c *Controller) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.refreshMu.Lock()
		running := c.refreshRunning
		dir := c.refreshDir
		c.refreshMu.Unlock()
		if !running {
			return dir
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh goroutine did not settle")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestPerformDeletionSurfacesIndexError is the U-04 guard. When the batched
// index DELETE fails after the files have already been renamed into the trash,
// the rows survive and the next refresh would list phantom entries with broken
// cells. The fix logs the failure and schedules a refresh against the active
// directory so the following paint resurfaces the index's truth. Closing the
// index makes RemoveEntries error deterministically; MoveToTrash still succeeds
// (it is a filesystem op), so we reach the failing DELETE.
func TestPerformDeletionSurfacesIndexError(t *testing.T) {
	dir := t.TempDir()
	idx, err := cache.Load(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	cacheDir := filepath.Join(dir, "cache")
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		t.Fatalf("NewThumbStore: %v", err)
	}
	root := filepath.Join(dir, "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewController(root, idx, store, cacheDir, cache.IndexPath(root, cacheDir))
	if c.trashDir == "" {
		t.Fatal("controller has no trash dir; the trash branch is untested")
	}

	// The directory a resurfacing refresh must target.
	const sentinel = "/active/dir/sentinel"
	c.mu.Lock()
	c.currentDir = sentinel
	c.mu.Unlock()

	// Capture the package logger, then close the index so RemoveEntries fails.
	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })
	idx.Close()

	victim := filepath.Join(root, "photo.jpg")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.performDeletion([]string{victim}, false)

	// The file still left the library even though the row could not be dropped.
	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("victim still at original path (stat err = %v, want IsNotExist)", err)
	}

	if got := waitRefreshSettled(t, c); got != sentinel {
		t.Errorf("scheduled refresh dir = %q, want %q", got, sentinel)
	}
	if out := logBuf.String(); !strings.Contains(out, "index cleanup failed") {
		t.Errorf("index cleanup failure not logged; got %q", out)
	}
}

// TestPerformDeletionSurfacesIndexErrorNoTrash covers the second call site: the
// straight-unlink branch taken when no trash dir is configured. Same failure
// (closed index → RemoveEntries errors) must log and schedule the refresh.
func TestPerformDeletionSurfacesIndexErrorNoTrash(t *testing.T) {
	dir := t.TempDir()
	idx, err := cache.Load(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	cacheDir := filepath.Join(dir, "cache")
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		t.Fatalf("NewThumbStore: %v", err)
	}
	root := filepath.Join(dir, "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewController(root, idx, store, cacheDir, cache.IndexPath(root, cacheDir))
	// Force the no-trash branch.
	c.trashDir = ""

	const sentinel = "/active/dir/notrash"
	c.mu.Lock()
	c.currentDir = sentinel
	c.mu.Unlock()

	var logBuf syncBuffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })
	idx.Close()

	victim := filepath.Join(root, "photo.jpg")
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	c.performDeletion([]string{victim}, false)

	if _, err := os.Stat(victim); !os.IsNotExist(err) {
		t.Errorf("victim still at original path (stat err = %v, want IsNotExist)", err)
	}
	if got := waitRefreshSettled(t, c); got != sentinel {
		t.Errorf("scheduled refresh dir = %q, want %q", got, sentinel)
	}
	if out := logBuf.String(); !strings.Contains(out, "index cleanup failed") {
		t.Errorf("index cleanup failure not logged; got %q", out)
	}
}

// TestDeleteOffGridPathRecounts covers U6-B3: a Duplicates delete usually
// removes files outside the current grid, which the local count patch cannot
// see, so the deletion must request a sidebar recount or the counts stay high.
func TestDeleteOffGridPathRecounts(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	keep, victim := filepath.Join(a, "keep.jpg"), filepath.Join(b, "dup.jpg")
	for _, p := range []string{keep, victim} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestController(t, root, filepath.Join(tmp, "cache"))
	c.index.ReconcileBatch([]scan.Result{
		{Path: keep, Type: scan.TypePhoto, Size: 1, ModTime: time.Unix(1000, 0)},
		{Path: victim, Type: scan.TypePhoto, Size: 1, ModTime: time.Unix(1000, 0)},
	})
	c.mu.Lock()
	c.currentDir = a
	c.mu.Unlock()
	c.refreshFromIndex(a, true)
	if counts, _ := c.DirCountsWithVersion(); counts[root] != 2 {
		t.Fatalf("baseline root count = %d, want 2", counts[root])
	}

	if err := c.DeletePaths([]string{victim}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		counts, _ := c.DirCountsWithVersion()
		if counts[root] == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("root count = %d after deleting an off-grid file, want 1", counts[root])
		}
		time.Sleep(5 * time.Millisecond)
	}
	waitRefreshSettled(t, c)
}
