package cache

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestDirKeysetMatchesOffset is the behavioural half of C5-01: clamping the
// keyset cursor against a dir view's own [lower, upper) range must not change
// any result. It covers the three cursor consumers —
//
//   - ListPageAfter's `path > cursor` page,
//   - NeighborsWithTotal's prev (`path < cursor`) and next (`path > cursor`),
//   - NeighborsWithTotal's pos (`path <= cursor`),
//
// against a brute-force walk of ListPage(v, 0, 0) over a ~300-entry tree nested
// several levels deep, with files outside the range on both sides (one sorting
// before Dir+"/", one sorting after the upper bound) and a row whose path is the
// directory itself (the exact-dir fold).
func TestDirKeysetMatchesOffset(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	// 300 photos across nested subdirectories, zero-padded so the expected
	// order is plain lexicographic. Every 4th lives one level deeper so the
	// range spans non-uniform depths.
	var results []scan.Result
	i := 0
	add := func(path string) {
		results = append(results, scan.Result{
			Path:    path,
			Type:    scan.TypePhoto,
			Size:    int64(1000 + i),
			ModTime: time.Date(2024, 1, 1, 0, 0, i%60, 0, time.UTC),
		})
		i++
	}
	for n := 0; n < 300; n++ {
		sub := fmt.Sprintf("d%02d", n%7)
		if n%4 == 0 {
			sub = filepath.Join(sub, "nested")
		}
		add(filepath.Join("/lib/gallery", sub, fmt.Sprintf("%04d.jpg", n)))
	}
	// Out-of-range siblings: "/lib/aaa.jpg" sorts before Dir+"/", "/lib2/zzz.jpg"
	// after the range's upper bound. Neither may leak into the view.
	add("/lib/aaa.jpg")
	add("/lib2/zzz.jpg")
	// A row whose path == the view's directory: the exact-dir fold.
	add("/lib/gallery")
	idx.ReconcileBatch(results)

	v := View{Kind: "dir", Dir: "/lib/gallery", Filter: "All", ShowRAW: true}
	full := idx.ListPage(v, 0, 0)
	if len(full) != 301 { // 300 children + the exact-dir row
		t.Fatalf("ListPage full = %d entries, want 301", len(full))
	}
	if full[0].Path != "/lib/gallery" {
		t.Fatalf("full[0] = %q, want the exact-dir row /lib/gallery", full[0].Path)
	}
	if got := idx.CountView(v); got != len(full) {
		t.Fatalf("CountView = %d, want %d", got, len(full))
	}
	want := viewPaths(full)

	// (1) Keyset walk, 7 at a time, must reproduce the offset view exactly.
	var got []string
	after := ""
	for iter := 0; iter < 1000; iter++ {
		page := idx.ListPageAfter(v, after, 7)
		got = append(got, viewPaths(page)...)
		if len(page) == 0 || len(page) < 7 {
			break
		}
		after = page[len(page)-1].Path
	}
	if !slices.Equal(got, want) {
		if n := firstDiff(got, want); n >= 0 {
			t.Fatalf("keyset walk = %d paths, want %d; first difference at %d: got %q want %q",
				len(got), len(want), n, got[n], want[n])
		}
		t.Fatalf("keyset walk = %d paths, want %d", len(got), len(want))
	}

	// (2) A page from an arbitrary cursor is the tail of the view, page-sized.
	_, upper := dirRange("/lib/gallery")
	probes := []string{
		full[0].Path,           // exact-dir row
		full[1].Path,           // first range row
		full[len(full)/3].Path, // mid, shallow
		full[len(full)/2].Path, // mid, possibly nested
		full[len(full)-1].Path, // last row
		"/lib/aaa.jpg",         // before the range (a row outside the view)
		upper,                  // exactly at the range's upper bound
		"/lib2/zzz.jpg",        // after the range
	}
	for _, probe := range probes {
		var wantTail []string
		for _, p := range want {
			if p > probe {
				wantTail = append(wantTail, p)
			}
			if len(wantTail) == 7 {
				break
			}
		}
		gotTail := viewPaths(idx.ListPageAfter(v, probe, 7))
		if !slices.Equal(gotTail, wantTail) {
			t.Errorf("ListPageAfter(probe %q) = %v, want %v", probe, gotTail, wantTail)
		}
	}

	// (3) NeighborsWithTotal must agree with a brute-force scan of the view:
	// prev = biggest path strictly below the probe, next = smallest strictly
	// above it, pos = number of view rows at or before it (which is the 1-based
	// index when the probe is in the view, and 0 before the range).
	brute := func(probe string) (prev, next string, pos int) {
		for _, e := range full {
			if e.Path < probe {
				prev = e.Path
				continue
			}
			if e.Path > probe && next == "" {
				next = e.Path
			}
		}
		for _, e := range full {
			if e.Path <= probe {
				pos++
			}
		}
		return prev, next, pos
	}
	for _, probe := range probes {
		wantPrev, wantNext, wantPos := brute(probe)
		prev, next, pos := idx.NeighborsWithTotal(v, probe)
		gotPrev, gotNext := entryPath(prev), entryPath(next)
		if gotPrev == "<nil>" {
			gotPrev = ""
		}
		if gotNext == "<nil>" {
			gotNext = ""
		}

		if gotPrev != wantPrev {
			t.Errorf("probe %q: prev = %q, want %q", probe, gotPrev, wantPrev)
		}
		if gotNext != wantNext {
			t.Errorf("probe %q: next = %q, want %q", probe, gotNext, wantNext)
		}
		if pos != wantPos {
			t.Errorf("probe %q: pos = %d, want %d", probe, pos, wantPos)
		}
	}

	// The probes that are members of the view must report their 1-based index.
	for n, e := range full {
		_, _, pos := idx.NeighborsWithTotal(v, e.Path)
		if pos != n+1 {
			t.Errorf("member %q at index %d: pos = %d, want %d", e.Path, n, pos, n+1)
		}
	}
}
