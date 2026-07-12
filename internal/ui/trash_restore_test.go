package ui

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
)

// seedTrashFixture writes n media files under root, moves each into trashDir via
// cache.MoveToTrash (the exact path the delete flow uses, sidecar and all), and
// returns the resulting trash paths alongside their original locations. Because
// the move empties the source, a later restore puts each file back at its exact
// original path (uniqueRestorePath never has to disambiguate).
func seedTrashFixture(t *testing.T, root, trashDir string, n int) (trashPaths, origPaths []string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	trashPaths = make([]string, 0, n)
	origPaths = make([]string, 0, n)
	for i := range n {
		orig := filepath.Join(root, fmt.Sprintf("photo-%03d.jpg", i))
		if err := os.WriteFile(orig, fmt.Appendf(nil, "content-%d", i), 0o644); err != nil {
			t.Fatal(err)
		}
		dst, err := cache.MoveToTrash(orig, trashDir)
		if err != nil {
			t.Fatalf("MoveToTrash(%s): %v", orig, err)
		}
		trashPaths = append(trashPaths, dst)
		origPaths = append(origPaths, orig)
	}
	return trashPaths, origPaths
}

// TestCollectRestoresBatchesAllResults is the U-02 batching guard. collectRestores
// is the filesystem half of the restore path: it moves every trashed file back and
// returns the scan.Results in ONE slice, which restoreBatch hands to a single
// ReconcileBatch. Asserting the slice carries all N rows proves the batch isn't
// fragmented into N single-row transactions (the frame-freezing behaviour U-02
// removed). It also confirms every file physically lands at its original path.
func TestCollectRestoresBatchesAllResults(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	trashDir := filepath.Join(dir, ".photo-viewer-trash")
	const n = 200
	trashPaths, origPaths := seedTrashFixture(t, root, trashDir, n)

	// store nil: RestoreFromTrash skips the thumb rename when no store is wired.
	results, restored := collectRestores(trashPaths, nil)

	if restored != n {
		t.Fatalf("restored = %d, want %d", restored, n)
	}
	if len(results) != n {
		t.Fatalf("collectRestores returned %d results, want %d in a single batch", len(results), n)
	}

	byPath := make(map[string]bool, len(results))
	for _, r := range results {
		byPath[r.Path] = true
	}
	for _, orig := range origPaths {
		if !byPath[orig] {
			t.Errorf("batch missing restored path %s", orig)
		}
		if _, err := os.Stat(orig); err != nil {
			t.Errorf("restored file not on disk at %s: %v", orig, err)
		}
	}

	// Every file (and its sidecar) left the trash dir.
	if remaining, _ := os.ReadDir(trashDir); len(remaining) != 0 {
		t.Errorf("trash dir not empty after restore: %d entries remain", len(remaining))
	}
}

// TestRestoreBatchReconcilesOnce drives restoreBatch through a real Controller +
// index and asserts the end state is exactly what one batched write produces: all
// N rows land in the index, every file is back at its original path, and the
// cached trash count drops by N to zero via a single bumpTrashCount. The pre-fix
// code looped this per item on the frame goroutine — N transactions, N clone-swaps,
// N refreshes.
func TestRestoreBatchReconcilesOnce(t *testing.T) {
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
	c := NewController(root, idx, store, cacheDir)
	trashDir := c.TrashDir()
	if trashDir == "" {
		t.Fatal("controller has no trash dir")
	}

	const n = 64
	trashPaths, origPaths := seedTrashFixture(t, root, trashDir, n)

	// Seed the cached trash count from disk while the items are still trashed,
	// so the -n adjustment lands on a real value (=n) rather than a zero floor.
	if got := c.cachedTrashCount(); got != n {
		t.Fatalf("cachedTrashCount before restore = %d, want %d", got, n)
	}

	c.restoreBatch(trashPaths)

	if got := idx.Count(); got != n {
		t.Fatalf("index has %d rows after restore, want %d", got, n)
	}
	for _, orig := range origPaths {
		if _, ok := idx.GetEntry(orig); !ok {
			t.Errorf("no index row for restored %s", orig)
		}
		if _, err := os.Stat(orig); err != nil {
			t.Errorf("restored file not on disk at %s: %v", orig, err)
		}
	}
	if got := c.cachedTrashCount(); got != 0 {
		t.Errorf("cached trash count = %d after restoring all, want 0", got)
	}
}
