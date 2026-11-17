package cache

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// newAfterIndex builds a self-contained index for the keyset-pagination tests.
// It deliberately mirrors the shape of a real library: photos + RAW in one
// directory, videos in another, and a dir view whose Dir is itself an entry
// path (the /lib/c row), which exercises ListPageAfter's exact-row stitch.
func newAfterIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := Load(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	var results []scan.Result
	i := 0
	add := func(path string, typ scan.MediaType) {
		results = append(results, scan.Result{
			Path:    path,
			Type:    typ,
			Size:    int64(1000 + i),
			ModTime: time.Date(2023+i%2, 3, 1, 0, 0, i, 0, time.UTC),
		})
		i++
	}

	// 60 photos + 20 RAW under /lib/a.
	for n := 0; n < 60; n++ {
		add(fmt.Sprintf("/lib/a/%03d.jpg", n), scan.TypePhoto)
	}
	for n := 0; n < 20; n++ {
		add(fmt.Sprintf("/lib/a/r%03d.cr2", n), scan.TypeRAW)
	}
	// 15 videos under /lib/b.
	for n := 0; n < 15; n++ {
		add(fmt.Sprintf("/lib/b/%03d.mp4", n), scan.TypeVideo)
	}
	// A file whose path is itself a directory name, plus 5 photos below it.
	add("/lib/c", scan.TypePhoto)
	for n := 0; n < 5; n++ {
		add(fmt.Sprintf("/lib/c/%03d.jpg", n), scan.TypePhoto)
	}

	idx.ReconcileBatch(results)

	// Every 4th /lib/a photo is a favorite.
	for n := 0; n < 60; n += 4 {
		if err := idx.SetFavorite(fmt.Sprintf("/lib/a/%03d.jpg", n), true); err != nil {
			t.Fatalf("SetFavorite: %v", err)
		}
	}
	return idx
}

// afterViews are the views both comparison tests walk.
func afterViews() []View {
	return []View{
		{Kind: "all", Filter: "All", ShowRAW: true},
		{Kind: "all", Filter: "Photos", ShowRAW: false},
		{Kind: "dir", Dir: "/lib/a", Filter: "All", ShowRAW: true},
		{Kind: "dir", Dir: "/lib/c", Filter: "All", ShowRAW: true},
		{Kind: "year", Year: 2024, Filter: "All", ShowRAW: true},
		{Kind: "favorites", Filter: "All", ShowRAW: true},
		{Kind: "all", Filter: "Videos", ShowRAW: true},
	}
}

// viewPaths projects entries onto their paths, in order.
func viewPaths(entries []Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Path
	}
	return out
}

// TestListPageAfterMatchesOffset proves the keyset walk reproduces the OFFSET
// walk exactly for every view and page size: concatenating ListPageAfter pages
// must equal both ListPage(v, 0, 0) and the sequence of ListPage offsets.
func TestListPageAfterMatchesOffset(t *testing.T) {
	idx := newAfterIndex(t)

	for _, v := range afterViews() {
		for _, limit := range []int{1, 3, 7} {
			want := viewPaths(idx.ListPage(v, 0, 0))
			if len(want) == 0 {
				t.Fatalf("view %+v is empty — fixture cannot test pagination", v)
			}

			var got []string
			after := ""
			for iter := 0; iter < 1000; iter++ {
				page := idx.ListPageAfter(v, after, limit)
				got = append(got, viewPaths(page)...)
				if len(page) == 0 || len(page) < limit {
					break
				}
				after = page[len(page)-1].Path
			}
			if !slices.Equal(got, want) {
				t.Errorf("view %+v limit %d: keyset walk = %d paths, want %d (differ at first mismatch)",
					v, limit, len(got), len(want))
				if n := firstDiff(got, want); n >= 0 {
					t.Errorf("  first difference at index %d: got %q want %q", n, got[n], want[n])
				}
			}

			var offset []string
			off := 0
			for iter := 0; iter < 1000; iter++ {
				page := idx.ListPage(v, off, limit)
				offset = append(offset, viewPaths(page)...)
				if len(page) == 0 || len(page) < limit {
					break
				}
				off += len(page)
			}
			if !slices.Equal(offset, want) {
				t.Errorf("view %+v limit %d: offset walk = %d paths, want %d (differ at first mismatch)",
					v, limit, len(offset), len(want))
				if n := firstDiff(offset, want); n >= 0 {
					t.Errorf("  first difference at index %d: got %q want %q", n, offset[n], want[n])
				}
			}

			// The dir view whose Dir is a real entry path must start with it.
			if v.Kind == "dir" && v.Dir == "/lib/c" && want[0] != "/lib/c" {
				t.Errorf("view %+v: first path = %q, want /lib/c (exact-path row)", v, want[0])
			}
		}
	}
}

// firstDiff returns the index of the first unequal element, or -1.
func firstDiff(a, b []string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return min(len(a), len(b))
	}
	return -1
}

// TestListPageAfterExactRowAfterCursor pins the dir-view exact-path stitch on
// the keyset path: a cursor before it yields it first, consuming one of the
// limit slots, and a limit of 1 returns it alone.
func TestListPageAfterExactRowAfterCursor(t *testing.T) {
	idx := newAfterIndex(t)
	v := View{Kind: "dir", Dir: "/lib/c", Filter: "All", ShowRAW: true}

	got := viewPaths(idx.ListPageAfter(v, "/lib/b", 2))
	want := []string{"/lib/c", "/lib/c/000.jpg"}
	if !slices.Equal(got, want) {
		t.Errorf("ListPageAfter(dir /lib/c, /lib/b, 2) = %v, want %v", got, want)
	}

	got = viewPaths(idx.ListPageAfter(v, "/lib/b", 1))
	want = []string{"/lib/c"}
	if !slices.Equal(got, want) {
		t.Errorf("ListPageAfter(dir /lib/c, /lib/b, 1) = %v, want %v", got, want)
	}
}

// TestPlanListPageAfter checks the keyset statement's query plan: it must seek
// (not scan) and must not sort through a temp b-tree, for every view shape.
func TestPlanListPageAfter(t *testing.T) {
	idx := newAfterIndex(t)

	for _, v := range afterViews() {
		q, args := listPageAfterQuery(v, "/lib/a/030.jpg", 201)

		rows, err := idx.db.Query("EXPLAIN QUERY PLAN "+q, args...)
		if err != nil {
			t.Fatalf("view %+v: EXPLAIN QUERY PLAN: %v", v, err)
		}
		var details []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatalf("view %+v: scan plan row: %v", v, err)
			}
			details = append(details, detail)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("view %+v: plan rows: %v", v, err)
		}
		rows.Close()

		plan := strings.Join(details, "\n")
		t.Logf("view %+v plan:\n%s", v, plan)

		if strings.Contains(plan, "TEMP B-TREE") {
			t.Errorf("view %+v: plan uses a temp b-tree:\n%s", v, plan)
		}
		for _, line := range details {
			if strings.TrimSpace(line) == "SCAN entries" {
				t.Errorf("view %+v: plan full-scans entries:\n%s", v, plan)
			}
		}
	}
}
