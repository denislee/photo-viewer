package ui

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
)

// TestRebuildWipesCacheSubtrees verifies that Rebuild clears the display/
// rendition cache alongside thumbs/ and hls/ (W-03), so a rebuild that
// re-derives renditions from changed originals can't keep serving stale ones.
func TestRebuildWipesCacheSubtrees(t *testing.T) {
	dir := t.TempDir()
	idx, err := cache.Load(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	cacheDir := filepath.Join(dir, "cache")
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		t.Fatalf("NewThumbStore: %v", err)
	}
	root := filepath.Join(dir, "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := NewController(root, idx, store, cacheDir)

	// Seed a sentinel file in each cache subtree Rebuild is expected to wipe.
	subs := []string{"thumbs", "hls", "display"}
	for _, sub := range subs {
		d := filepath.Join(cacheDir, sub, "aa")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "sentinel.jpg"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := c.Rebuild(); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	// Rebuild removes the subtrees synchronously before spawning the rescan.
	for _, sub := range subs {
		p := filepath.Join(cacheDir, sub, "aa", "sentinel.jpg")
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s sentinel survived Rebuild (stat err = %v, want IsNotExist)", sub, err)
		}
	}

	// The rescan runs on an empty root, so it settles almost immediately; wait
	// for it before the temp dir is torn down.
	deadline := time.Now().Add(2 * time.Second)
	for c.IndexStatus().Active && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
}

// TestRebuildFailureSurfaced verifies that a failing Rebuild is made visible
// rather than silently swallowed (U-05): the error the toolbar handler receives
// is recorded in IndexStatus.LastError (the field the index-info modal shows)
// and accompanied by an invalidate. The failure is forced by closing the index
// so idx.Clear() errors before any rescan starts — deterministic and free of
// permission tricks (which the test user, if root, would bypass anyway).
func TestRebuildFailureSurfaced(t *testing.T) {
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
	c := NewController(root, idx, store, cacheDir)

	var invalidated atomic.Int32
	c.SetInvalidate(func() { invalidated.Add(1) })

	// Close the index so idx.Clear() inside Rebuild fails deterministically,
	// returning before any scanInto goroutine is spawned.
	if err := idx.Close(); err != nil {
		t.Fatalf("idx.Close: %v", err)
	}

	rebuildErr := c.Rebuild()
	if rebuildErr == nil {
		t.Fatal("Rebuild returned nil on a closed index; want an error")
	}
	// Mirror the toolbar handler (window.go OnRebuild): surface, don't drop.
	c.surfaceError("rebuild index", rebuildErr)

	if got := c.IndexStatus().LastError; got != rebuildErr.Error() {
		t.Errorf("IndexStatus().LastError = %q, want %q", got, rebuildErr.Error())
	}
	if invalidated.Load() == 0 {
		t.Error("surfaceError did not invalidate the UI")
	}
}
