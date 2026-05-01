package cache_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestPipeline walks a fixture directory, builds an index, generates
// thumbnails, and verifies that a second pass reuses cached entries.
func TestPipeline(t *testing.T) {
	root := os.Getenv("PV_TEST_ROOT")
	if root == "" {
		t.Skip("set PV_TEST_ROOT to a sample photo directory to run this test")
	}
	cacheDir, err := os.MkdirTemp("", "pv-cache-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(cacheDir)

	dbPath := filepath.Join(cacheDir, "index.db")
	idx, err := cache.Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store, err := cache.NewThumbStore(cacheDir)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	var entries []cache.Entry
	for r := range scan.Walk(ctx, root) {
		entries = append(entries, idx.ReconcileBatch([]scan.Result{r})[0])
	}
	if len(entries) == 0 {
		t.Fatalf("no media found under %s", root)
	}

	for _, e := range entries {
		path, err := store.Path(e)
		if err != nil {
			t.Logf("thumb %s: %v", e.Path, err)
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("thumb stat %s: %v", path, err)
			continue
		}
		if info.Size() == 0 {
			t.Errorf("thumb is empty: %s", path)
		}
	}

	if err := idx.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("index.db missing: %v", err)
	}

	// Second pass: reload index, walk again, and ensure each result is fresh.
	idx2, err := cache.Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	regen := 0
	for r := range scan.Walk(ctx, root) {
		before := idx2.All()
		entry := idx2.ReconcileBatch([]scan.Result{r})[0]
		_ = entry
		after := idx2.All()
		if len(after) != len(before) {
			regen++
		}
	}
	if regen != 0 {
		t.Errorf("expected zero new entries on second pass, got %d", regen)
	}
}
