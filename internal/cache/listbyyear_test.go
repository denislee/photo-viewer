package cache

import (
	"sort"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// seedDated inserts one entry per (relative path, type) pair under root using
// ReconcileBatch, so the `year` column is backfilled from each YYYY-MM-DD parent
// dir exactly as it is on a real ingest. mtime is deliberately set to a year
// (2020) that matches none of the folder years, proving the year column — not
// the mtime fallback — drives ListByYear here.
func seedDated(t *testing.T, idx *Index, root string, files map[string]scan.MediaType) {
	t.Helper()
	results := make([]scan.Result, 0, len(files))
	for rel, typ := range files {
		results = append(results, scan.Result{
			Path:    root + "/" + rel,
			Type:    typ,
			Size:    1000,
			ModTime: time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC),
		})
	}
	idx.ReconcileBatch(results)
}

// pathSet returns the sorted absolute paths of entries — the "result set" the
// year-preview union cares about (final display order is re-sorted by the UI).
func pathSet(entries []Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Path)
	}
	sort.Strings(out)
	return out
}

// unionListDir replicates the pre-U-08 year-preview loop: one recursive ListDir
// per bucketed date dir, unioned in order, then the media filter applied in Go
// (mirroring refreshFromIndex). This is the authoritative "old behavior" the new
// single query must reproduce.
func unionListDir(idx *Index, dirs []string, filter string, showRAW bool) []Entry {
	var entries []Entry
	for _, d := range dirs {
		entries = append(entries, idx.ListDir(d)...)
	}
	var out []Entry
	for _, e := range entries {
		if !showRAW && e.Type == scan.TypeRAW {
			continue
		}
		switch filter {
		case "Photos":
			if e.Type == scan.TypeVideo {
				continue
			}
		case "Videos":
			if e.Type != scan.TypeVideo {
				continue
			}
		}
		out = append(out, e)
	}
	return out
}

// TestListByYearMatchesUnionLoop proves the U-08 single query reproduces the old
// per-date-dir ListDir union for the active year and treeDir, across every media
// filter, while dir-scoping excludes a different year and a different treeDir.
func TestListByYearMatchesUnionLoop(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	// treeDir A: two 2023 date dirs (the year-2023 bucket) + one 2022 dir.
	seedDated(t, idx, "/libA", map[string]scan.MediaType{
		"2023-01-10/a0.jpg": scan.TypePhoto,
		"2023-01-10/a1.jpg": scan.TypePhoto,
		"2023-06-20/b0.jpg": scan.TypePhoto,
		"2023-06-20/b1.mp4": scan.TypeVideo,
		"2023-06-20/b2.cr2": scan.TypeRAW,
		"2022-03-03/c0.jpg": scan.TypePhoto, // different year — must be excluded
	})
	// treeDir B: a 2023 date dir under a different anchor — must be excluded by
	// dir-scoping even though its year matches.
	seedDated(t, idx, "/libB", map[string]scan.MediaType{
		"2023-09-09/d0.jpg": scan.TypePhoto,
	})

	const treeDir = "/libA"
	const year = 2023
	// The sidebar buckets these two immediate YYYY-MM-DD children of treeDir
	// under the 2023 header; PreviewYear hands them to the old loop.
	yearPreviewDirs := []string{"/libA/2023-01-10", "/libA/2023-06-20"}

	cases := []struct {
		filter  string
		showRAW bool
	}{
		{"All", true},
		{"All", false},
		{"Photos", true},
		{"Photos", false},
		{"Videos", true},
		{"Videos", false},
	}
	for _, tc := range cases {
		want := pathSet(unionListDir(idx, yearPreviewDirs, tc.filter, tc.showRAW))
		got := pathSet(idx.ListByYear(year, tc.filter, tc.showRAW, treeDir))
		if !equalStrings(want, got) {
			t.Errorf("filter=%q showRAW=%v: ListByYear set = %v, want union-loop set = %v",
				tc.filter, tc.showRAW, got, want)
		}
	}

	// Different year is excluded: the 2022 dir never appears in the 2023 result.
	for _, p := range pathSet(idx.ListByYear(year, "All", true, treeDir)) {
		if p == "/libA/2022-03-03/c0.jpg" {
			t.Errorf("2022 entry leaked into year 2023 result: %s", p)
		}
	}

	// Dir-scoping excludes the other treeDir: /libB entries stay out of the
	// treeDir=/libA result even though their year matches. The library-global
	// form (dir == "") would wrongly include them — that superset is exactly the
	// bug the dir-scoped variant fixes.
	scoped := pathSet(idx.ListByYear(year, "All", true, treeDir))
	for _, p := range scoped {
		if p == "/libB/2023-09-09/d0.jpg" {
			t.Errorf("dir-scoped result leaked another treeDir: %s", p)
		}
	}
	global := pathSet(idx.ListByYear(year, "All", true, ""))
	foundGlobal := false
	for _, p := range global {
		if p == "/libB/2023-09-09/d0.jpg" {
			foundGlobal = true
		}
	}
	if !foundGlobal {
		t.Errorf("sanity: library-global ListByYear should include /libB/2023-09-09/d0.jpg; got %v", global)
	}
}

// TestPlanListByYearScoped proves the dir-scoped year query still streams off
// idx_entries_yearexpr — the year seek plus the trailing-path range are one
// ordered index scan, no full table scan and no temp b-tree for ORDER BY.
func TestPlanListByYearScoped(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 200)

	lower, upper := dirRange("/lib")
	q := "SELECT path FROM entries WHERE " + yearExpr + " = ? AND path >= ? AND path < ? ORDER BY path"
	plan := explainPlan(t, idx, q, 2024, lower, upper)

	mustContain(t, "list-by-year-scoped", plan, "idx_entries_yearexpr")
	mustNotContain(t, "list-by-year-scoped", plan, "SCAN entries")
	mustNotContain(t, "list-by-year-scoped", plan, "TEMP B-TREE")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
