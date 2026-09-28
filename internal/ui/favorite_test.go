package ui

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestPatchFavoriteAdjustsEntryAndCount exercises the U-07 in-memory patch in
// isolation (no index needed — patchFavorite only touches the Controller's own
// state). It asserts the matching entry's Favorite flag flips, the neighbours
// are untouched, the cached FavoritesView count moves by exactly ±1, and — the
// concurrency-critical bit — the swap is a fresh backing array so a goroutine
// still holding the previous Snapshot slice header never observes a mutated
// element (mirrors patchLocalDeletion's clone-swap contract).
func TestPatchFavoriteAdjustsEntryAndCount(t *testing.T) {
	c := &Controller{}
	invalidated := 0
	c.SetInvalidate(func() { invalidated++ })

	c.mu.Lock()
	c.entries = []cache.Entry{
		{Path: "/lib/a.jpg"},
		{Path: "/lib/b.jpg"},
		{Path: "/lib/c.jpg"},
	}
	// Seed a FavoritesView count that deliberately does NOT match any DB-derived
	// value, so a stray full refresh (which would recompute it) is distinguishable
	// from the pure ±1 patch under test.
	c.dirCounts = map[string]int{"/lib": 3, FavoritesView: 7}
	c.dirCountsVer = 1
	oldEntries := c.entries
	c.mu.Unlock()
	verBefore := c.dirCountsVer

	// Favorite the middle entry.
	c.patchFavorite("/lib/b.jpg", true)

	_, _, entries, _ := c.Snapshot()
	if !entries[1].Favorite {
		t.Fatalf("entry b.jpg Favorite = false after patch, want true")
	}
	if entries[0].Favorite || entries[2].Favorite {
		t.Fatalf("neighbours flipped: a=%v c=%v", entries[0].Favorite, entries[2].Favorite)
	}
	if got := c.DirCounts()[FavoritesView]; got != 8 {
		t.Fatalf("FavoritesView count = %d after favoriting, want 8 (7+1)", got)
	}
	if c.dirCountsVer == verBefore {
		t.Fatalf("dirCountsVer not bumped by patch")
	}
	// Clone-swap: the previous backing array a lock-free reader may still hold
	// must be untouched, and the published slice must be a new allocation.
	if oldEntries[1].Favorite {
		t.Fatalf("previous backing array was mutated in place")
	}
	if &oldEntries[0] == &entries[0] {
		t.Fatalf("patch reused the old backing array instead of cloning")
	}
	if invalidated != 1 {
		t.Fatalf("invalidate called %d times, want 1", invalidated)
	}

	// Un-favorite it again: count returns to 7.
	c.patchFavorite("/lib/b.jpg", false)
	_, _, entries, _ = c.Snapshot()
	if entries[1].Favorite {
		t.Fatalf("entry b.jpg still Favorite after un-favoriting")
	}
	if got := c.DirCounts()[FavoritesView]; got != 7 {
		t.Fatalf("FavoritesView count = %d after un-favoriting, want 7", got)
	}
}

// TestToggleFavoriteEndToEnd drives ToggleFavorite through a real Controller +
// index while the active view is an ordinary directory (not the favorites view).
// It confirms the flip persists to the DB (written in the background, G-14), the
// returned value is the new state, the grid entry's star flips in place, and the
// sidebar's cached favorites count moves by ±1 — all without the full
// refreshFromIndex the pre-U-07 code paid per keystroke.
func TestToggleFavoriteEndToEnd(t *testing.T) {
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
	paths := []string{
		filepath.Join(root, "a.jpg"),
		filepath.Join(root, "b.jpg"),
		filepath.Join(root, "c.jpg"),
	}
	results := make([]scan.Result, 0, len(paths))
	for _, p := range paths {
		results = append(results, scan.Result{
			Path:    p,
			Type:    scan.TypePhoto,
			Size:    10,
			ModTime: time.Unix(1000, 0),
		})
	}
	idx.ReconcileBatch(results)

	c := NewController(root, idx, store, cacheDir)
	// currentDir defaults to root (an ordinary dir), so ToggleFavorite takes the
	// patch-in-place branch rather than the favorites-view full-refresh branch.
	c.mu.Lock()
	c.entries = idx.ListDir(root)
	c.dirCounts = map[string]int{root: 3, FavoritesView: 0}
	c.dirCountsVer = 1
	c.mu.Unlock()

	got := c.ToggleFavorite(paths[1], false)
	if !got {
		t.Fatalf("ToggleFavorite returned %v, want true", got)
	}
	c.FlushFavorites(5 * time.Second)
	if !idx.IsFavorite(paths[1]) {
		t.Fatalf("flag not persisted to the index")
	}

	_, _, entries, _ := c.Snapshot()
	for _, e := range entries {
		want := e.Path == paths[1]
		if e.Favorite != want {
			t.Fatalf("entry %s Favorite = %v, want %v", e.Path, e.Favorite, want)
		}
	}
	if n := c.DirCounts()[FavoritesView]; n != 1 {
		t.Fatalf("FavoritesView count = %d after favoriting, want 1", n)
	}

	// Toggling back returns false and drops the count to 0.
	if got := c.ToggleFavorite(paths[1], true); got {
		t.Fatalf("second ToggleFavorite returned %v, want false", got)
	}
	c.FlushFavorites(5 * time.Second)
	if idx.IsFavorite(paths[1]) {
		t.Fatalf("flag still set after second toggle")
	}
	if n := c.DirCounts()[FavoritesView]; n != 0 {
		t.Fatalf("FavoritesView count = %d after un-favoriting, want 0", n)
	}
}

// TestToggleFavoriteOrderedAndReverted is the G-14 guard for the background
// write: rapid toggles land in order (the DB matches the last press), and a
// failed write undoes the optimistic flip in the grid and the favorites count,
// reports a revert for the viewer, and warns the user.
func TestToggleFavoriteOrderedAndReverted(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	c := newTestController(t, root, filepath.Join(dir, "cache"))
	path := filepath.Join(root, "a.jpg")
	c.index.ReconcileBatch([]scan.Result{{Path: path, Type: scan.TypePhoto, Size: 1, ModTime: time.Unix(1000, 0)}})
	c.mu.Lock()
	c.entries = c.index.ListDir(root)
	c.dirCounts = map[string]int{root: 1, FavoritesView: 0}
	c.mu.Unlock()

	var warned []string
	var warnMu sync.Mutex
	orig := favoriteWarnFn
	favoriteWarnFn = func(msg string) { warnMu.Lock(); warned = append(warned, msg); warnMu.Unlock() }
	defer func() { favoriteWarnFn = orig }()

	fav := false
	for range 7 { // odd number of presses: ends favorited
		fav = c.ToggleFavorite(path, fav)
	}
	c.FlushFavorites(5 * time.Second)
	if !fav || !c.index.IsFavorite(path) {
		t.Fatalf("after 7 presses: returned %v, DB %v; want both true", fav, c.index.IsFavorite(path))
	}

	// Make the next write fail: the row disappears underneath the grid.
	if err := c.index.RemoveEntry(path); err != nil {
		t.Fatal(err)
	}
	if got := c.ToggleFavorite(path, true); got {
		t.Fatalf("optimistic value = %v, want false", got)
	}
	c.FlushFavorites(5 * time.Second)

	_, _, entries, _ := c.Snapshot()
	if len(entries) != 1 || !entries[0].Favorite {
		t.Errorf("grid entry not reverted to favorite: %+v", entries)
	}
	if n := c.DirCounts()[FavoritesView]; n != 1 {
		t.Errorf("FavoritesView count = %d after revert, want 1", n)
	}
	if r := c.TakeFavoriteReverts(); len(r) != 1 || r[0] != (FavoriteRevert{Path: path, Favorite: true}) {
		t.Errorf("reverts = %+v, want one restoring true", r)
	}
	if r := c.TakeFavoriteReverts(); r != nil {
		t.Errorf("reverts not cleared: %+v", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		warnMu.Lock()
		n := len(warned)
		warnMu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("warnings = %d, want 1", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}
