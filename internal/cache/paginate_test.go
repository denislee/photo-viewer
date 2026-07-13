package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// loadEmpty opens a fresh SQLite index in a temp dir, returns it plus a
// cleanup function. Tests that need data call seed afterwards.
func loadEmpty(t *testing.T) (*Index, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pv-paginate-")
	if err != nil {
		t.Fatal(err)
	}
	idx, err := Load(filepath.Join(dir, "index.db"))
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	return idx, func() {
		idx.Close()
		os.RemoveAll(dir)
	}
}

// seed inserts n synthetic photo entries under root using ReconcileBatch.
// Paths sort lexicographically so positional assertions are stable.
func seed(t *testing.T, idx *Index, root string, n int) []Entry {
	t.Helper()
	results := make([]scan.Result, 0, n)
	for i := range n {
		results = append(results, scan.Result{
			Path:    filepath.Join(root, padded(i)+".jpg"),
			Type:    scan.TypePhoto,
			Size:    int64(1000 + i),
			ModTime: time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC),
		})
	}
	return idx.ReconcileBatch(results)
}

func padded(i int) string {
	out := make([]byte, 6)
	for k := 5; k >= 0; k-- {
		out[k] = byte('0' + i%10)
		i /= 10
	}
	return string(out)
}

func TestListPagePagination(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	root := "/lib"
	seed(t, idx, root, 250)

	v := View{Kind: "dir", Dir: root}
	if got := idx.CountView(v); got != 250 {
		t.Fatalf("CountView = %d, want 250", got)
	}

	first := idx.ListPage(v, 0, 100)
	if len(first) != 100 {
		t.Fatalf("first page len = %d, want 100", len(first))
	}
	if filepath.Base(first[0].Path) != "000000.jpg" {
		t.Errorf("first[0] = %s, want 000000.jpg", first[0].Path)
	}
	if filepath.Base(first[99].Path) != "000099.jpg" {
		t.Errorf("first[99] = %s, want 000099.jpg", first[99].Path)
	}

	second := idx.ListPage(v, 100, 100)
	if filepath.Base(second[0].Path) != "000100.jpg" {
		t.Errorf("second[0] = %s, want 000100.jpg", second[0].Path)
	}

	last := idx.ListPage(v, 200, 100)
	if len(last) != 50 {
		t.Fatalf("last page len = %d, want 50 (250 - 200)", len(last))
	}
}

func TestNeighbors(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	root := "/lib"
	seed(t, idx, root, 10)

	v := View{Kind: "all"}
	target := filepath.Join(root, "000005.jpg")
	prev, next, pos, total := idx.Neighbors(v, target)
	if pos != 6 {
		t.Errorf("pos = %d, want 6 (1-based)", pos)
	}
	if total != 10 {
		t.Errorf("total = %d, want 10", total)
	}
	if prev == nil || filepath.Base(prev.Path) != "000004.jpg" {
		t.Errorf("prev = %v, want 000004.jpg", prev)
	}
	if next == nil || filepath.Base(next.Path) != "000006.jpg" {
		t.Errorf("next = %v, want 000006.jpg", next)
	}

	// First item: prev should be nil.
	prev, _, _, _ = idx.Neighbors(v, filepath.Join(root, "000000.jpg"))
	if prev != nil {
		t.Errorf("first item prev = %v, want nil", prev)
	}
	// Last item: next should be nil.
	_, next, _, _ = idx.Neighbors(v, filepath.Join(root, "000009.jpg"))
	if next != nil {
		t.Errorf("last item next = %v, want nil", next)
	}
}

// entryPath returns e.Path or "<nil>" so equivalence failures print a stable
// label instead of a pointer address.
func entryPath(e *Entry) string {
	if e == nil {
		return "<nil>"
	}
	return e.Path
}

// assertNeighborsEquiv proves NeighborsWithTotal(v, probe, CountView(v)) returns
// the identical prev/next/pos/total tuple as the self-counting Neighbors(v,
// probe). This is the W-10 equivalence guarantee: threading the cached total
// into the bounded-position variant must not change any observable result.
func assertNeighborsEquiv(t *testing.T, idx *Index, label string, v View, probe string) {
	t.Helper()
	wantPrev, wantNext, wantPos, wantTotal := idx.Neighbors(v, probe)
	gotPrev, gotNext, gotPos, gotTotal := idx.NeighborsWithTotal(v, probe, idx.CountView(v))

	if entryPath(gotPrev) != entryPath(wantPrev) {
		t.Errorf("%s: prev = %s, want %s", label, entryPath(gotPrev), entryPath(wantPrev))
	}
	if entryPath(gotNext) != entryPath(wantNext) {
		t.Errorf("%s: next = %s, want %s", label, entryPath(gotNext), entryPath(wantNext))
	}
	if gotPos != wantPos {
		t.Errorf("%s: pos = %d, want %d", label, gotPos, wantPos)
	}
	if gotTotal != wantTotal {
		t.Errorf("%s: total = %d, want %d", label, gotTotal, wantTotal)
	}
}

// TestNeighborsWithTotalEquivalence exercises the W-10 split: NeighborsWithTotal
// fed CountView(v) must match Neighbors across every view kind (all, favorites,
// year, dir) and every probe position (first, middle, last), including the
// tricky dir-exact fold cases (file-as-dir and a synthetic exact-prepend row).
func TestNeighborsWithTotalEquivalence(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	root := "/lib"
	entries := seed(t, idx, root, 20) // all mtime year 2024

	// Favorite a scattered subset so the favorites view has interior + edge rows.
	for _, k := range []int{0, 3, 4, 9, 19} {
		if err := idx.SetFavorite(entries[k].Path, true); err != nil {
			t.Fatalf("SetFavorite: %v", err)
		}
	}
	// A synthetic row whose path == a subdirectory string, to drive the
	// dir-exact fold (can't occur on a real fs, but the index is path-keyed).
	seed(t, idx, "/lib/sub", 5)
	if _, err := idx.db.Exec(
		"INSERT INTO entries (path, type, size, mtime, thumb_id, favorite, duration_ms) VALUES (?,?,?,?,?,0,0)",
		"/lib/sub", 1, 1, 0, ThumbIDFor("/lib/sub"),
	); err != nil {
		t.Fatalf("insert exact-dir row: %v", err)
	}

	first, mid, last := entries[0].Path, entries[10].Path, entries[19].Path
	cases := []struct {
		label string
		v     View
		probe string
	}{
		{"all/first", View{Kind: "all"}, first},
		{"all/mid", View{Kind: "all"}, mid},
		{"all/last", View{Kind: "all"}, last},
		{"favorites/first", View{Kind: "favorites"}, entries[0].Path},
		{"favorites/mid", View{Kind: "favorites"}, entries[4].Path},
		{"favorites/last", View{Kind: "favorites"}, entries[19].Path},
		{"year/first", View{Kind: "year", Year: 2024}, first},
		{"year/mid", View{Kind: "year", Year: 2024}, mid},
		{"year/last", View{Kind: "year", Year: 2024}, last},
		{"dir/first", View{Kind: "dir", Dir: root}, first},
		{"dir/mid", View{Kind: "dir", Dir: root}, mid},
		{"dir/last", View{Kind: "dir", Dir: root}, last},
		// file-as-dir: probe is the file itself (sole member via dirExactEntry).
		{"dir/file-as-dir", View{Kind: "dir", Dir: entries[3].Path}, entries[3].Path},
		// exact-prepend: probe the exact-dir row and its first child.
		{"dir/exact-row", View{Kind: "dir", Dir: "/lib/sub"}, "/lib/sub"},
		{"dir/exact-firstchild", View{Kind: "dir", Dir: "/lib/sub"}, filepath.Join("/lib/sub", "000000.jpg")},
		// A probe outside the view (pos == 0) must agree too.
		{"all/absent", View{Kind: "all"}, filepath.Join(root, "999999.jpg")},
	}
	for _, c := range cases {
		assertNeighborsEquiv(t, idx, c.label, c.v, c.probe)
	}
}

func TestNeighborsDirView(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/a", 5)
	seed(t, idx, "/b", 5)

	// Neighbors restricted to /a must not cross into /b.
	v := View{Kind: "dir", Dir: "/a"}
	_, next, _, total := idx.Neighbors(v, filepath.Join("/a", "000004.jpg"))
	if total != 5 {
		t.Errorf("total in dir view = %d, want 5", total)
	}
	if next != nil {
		t.Errorf("next at end of /a = %v, want nil (must not cross into /b)", next)
	}
}
