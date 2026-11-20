package ui

import (
	"maps"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// seedRefreshIndex inserts entries describing a small library: files directly
// in dir, two nested child trees (up to three levels deep), a mix of photo,
// video and RAW types, plus a row whose path is dir itself (the exact-path row
// ListPage folds in when a *file* path is passed as the dir).
func seedRefreshIndex(t *testing.T, idx *cache.Index, dir string) {
	t.Helper()
	paths := []string{
		dir,
		filepath.Join(dir, "a.jpg"),
		filepath.Join(dir, "b.mp4"),
		filepath.Join(dir, "c.cr2"),
		filepath.Join(dir, "sub1", "d.jpg"),
		filepath.Join(dir, "sub1", "e.cr2"),
		filepath.Join(dir, "sub1", "deep", "f.mp4"),
		filepath.Join(dir, "sub1", "deep", "deeper", "g.jpg"),
		filepath.Join(dir, "sub2", "h.png"),
		filepath.Join(dir, "sub2", "i.mov"),
	}
	results := make([]scan.Result, 0, len(paths))
	for i, p := range paths {
		typ := scan.TypePhoto
		switch filepath.Ext(p) {
		case ".mp4", ".mov":
			typ = scan.TypeVideo
		case ".cr2":
			typ = scan.TypeRAW
		}
		results = append(results, scan.Result{
			Path:    p,
			Type:    typ,
			Size:    int64(i + 1),
			ModTime: time.Unix(1000, 0),
		})
	}
	idx.ReconcileBatch(results)
}

// TestChildCountsFromEntriesMatchesSQL is the U5-02 guard: deriving the tree's
// total and per-child counts from the loaded (already media-filtered) dir-view
// slice must agree exactly with the SQL aggregates it replaces, for every
// filter × showRAW combination.
func TestChildCountsFromEntriesMatchesSQL(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTestController(t, root, filepath.Join(tmp, "cache"))
	seedRefreshIndex(t, c.index, root)

	for _, filter := range []string{"All", "Photos", "Videos"} {
		for _, showRAW := range []bool{true, false} {
			entries := c.index.ListPage(cache.View{
				Kind: "dir", Dir: root, Filter: filter, ShowRAW: showRAW,
			}, 0, 0)

			got := childCountsFromEntries(root, entries)
			want := c.index.CountChildDirsFiltered(root, filter, showRAW)
			if !maps.Equal(got, want) {
				t.Errorf("filter=%q showRAW=%v: childCountsFromEntries = %v, CountChildDirsFiltered = %v",
					filter, showRAW, got, want)
			}
			if n, want := len(entries), c.index.CountDirFiltered(root, filter, showRAW); n != want {
				t.Errorf("filter=%q showRAW=%v: len(ListPage) = %d, CountDirFiltered = %d",
					filter, showRAW, n, want)
			}

			// Sanity: the children really are represented, so the comparison
			// above can't pass by both sides being empty.
			if filter == "All" && showRAW {
				if want[filepath.Join(root, "sub1")] != 4 || want[filepath.Join(root, "sub2")] != 2 {
					t.Fatalf("unexpected unfiltered child counts: %v", want)
				}
			}
		}
	}
}

// TestPreviewDirSkipsCounts is the U5-01 guard: a refresh that only changes the
// previewed directory must not recompute (or republish) the sidebar counts,
// while a refresh that carries a recount request must.
func TestPreviewDirSkipsCounts(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	sub := filepath.Join(root, "2024-01-01")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTestController(t, root, filepath.Join(tmp, "cache"))
	c.index.ReconcileBatch([]scan.Result{
		{Path: filepath.Join(root, "a.jpg"), Type: scan.TypePhoto, Size: 1, ModTime: time.Unix(1000, 0)},
		{Path: filepath.Join(sub, "b.jpg"), Type: scan.TypePhoto, Size: 1, ModTime: time.Unix(1000, 0)},
	})

	c.refreshFromIndex(root, true)
	counts, before := c.DirCountsWithVersion()
	if before == 0 || counts == nil {
		t.Fatal("initial refresh published no counts")
	}

	// Previewing a subdir leaves treeDir (and so the counts key) untouched.
	c.mu.Lock()
	c.currentDir = sub
	c.mu.Unlock()
	c.refreshFromIndex(sub, false)

	if _, after := c.DirCountsWithVersion(); after != before {
		t.Fatalf("preview refresh bumped dirCountsVer %d -> %d, want unchanged", before, after)
	}
	if _, _, entries, _ := c.Snapshot(); len(entries) != 1 || entries[0].Path != filepath.Join(sub, "b.jpg") {
		t.Fatalf("preview did not publish the subdir entries: %+v", entries)
	}

	// An explicit recount means data changed: the counts are recomputed.
	c.refreshFromIndex(sub, true)
	if _, after := c.DirCountsWithVersion(); after <= before {
		t.Fatalf("recount refresh left dirCountsVer at %d, want > %d", after, before)
	}
}

// TestScheduleRefreshRecountMerges is the U5-01 coalescer guard: a recount
// request interleaved with plain refreshes must never be swallowed by the
// coalescing, however the calls race.
func TestScheduleRefreshRecountMerges(t *testing.T) {
	tmp := t.TempDir()
	root := filepath.Join(tmp, "lib")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	c := newTestController(t, root, filepath.Join(tmp, "cache"))
	c.index.ReconcileBatch([]scan.Result{
		{Path: filepath.Join(root, "a.jpg"), Type: scan.TypePhoto, Size: 1, ModTime: time.Unix(1000, 0)},
	})
	c.refreshFromIndex(root, true) // establish a valid counts baseline
	_, before := c.DirCountsWithVersion()

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			c.scheduleRefresh(root)
		}()
		go func() {
			defer wg.Done()
			c.scheduleRefreshRecount(root)
		}()
	}
	wg.Wait()
	waitRefreshSettled(t, c)

	if _, after := c.DirCountsWithVersion(); after <= before {
		t.Fatalf("dirCountsVer = %d after concurrent refreshes, want > %d (recount lost)", after, before)
	}
}
