package ui

import (
	"fmt"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// viewerTestEntries builds n entries the viewer treats as opaque: scan.TypeUnknown
// matches neither the photo/RAW/HEIC decode path nor the video prefetch path, so
// Show/ConfirmDelete start no background work and these tests stay deterministic.
func viewerTestEntries(n int) []cache.Entry {
	entries := make([]cache.Entry, n)
	for i := range entries {
		entries[i] = cache.Entry{
			Path: fmt.Sprintf("/nonexistent/viewer-entries-%d.bin", i),
			Type: scan.TypeUnknown,
		}
	}
	return entries
}

func entryPaths(entries []cache.Entry) []string {
	paths := make([]string, len(entries))
	for i, e := range entries {
		paths[i] = e.Path
	}
	return paths
}

func assertPaths(t *testing.T, what string, got []cache.Entry, want []string) {
	t.Helper()
	gotPaths := entryPaths(got)
	if len(gotPaths) != len(want) {
		t.Fatalf("%s has %d entries (%v), want %d (%v)", what, len(gotPaths), gotPaths, len(want), want)
	}
	for i := range want {
		if gotPaths[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q (full: %v vs %v)", what, i, gotPaths[i], want[i], gotPaths, want)
		}
	}
}

// TestViewerShowSharesEntries is the G4-03 guard: Show stores the caller's
// slice instead of copying it, so entering the viewer on a huge library costs
// no allocation, and entriesOwned is false until the viewer first writes.
func TestViewerShowSharesEntries(t *testing.T) {
	t.Run("shares caller slice", func(t *testing.T) {
		entries := viewerTestEntries(3)
		var v Viewer
		v.Show(entries, 0)
		if &v.entries[0] != &entries[0] {
			t.Fatalf("Show copied the entries: &v.entries[0] = %p, &entries[0] = %p", &v.entries[0], &entries[0])
		}
		if v.entriesOwned {
			t.Fatal("Show marked the entries as owned, want the caller's slice shared")
		}
	})

	t.Run("no allocation at 10000 entries", func(t *testing.T) {
		entries := viewerTestEntries(10000)
		var v Viewer
		v.Show(entries, 0)
		if n := testing.AllocsPerRun(50, func() { v.Show(entries, 0) }); n > 4 {
			t.Fatalf("Show allocs = %v per run over %d shared entries, want <= 4 (the slice must not be copied)", n, len(entries))
		}
		if v.entriesOwned {
			t.Fatal("Show left entriesOwned true after repeated shows of a shared slice")
		}
	})
}

// TestViewerConfirmDeleteKeepsCallerSlice covers G4-03 for the delete path: the
// viewer must drop the entry from its own list without shifting the caller's
// slice, which the grid keeps reading.
func TestViewerConfirmDeleteKeepsCallerSlice(t *testing.T) {
	entries := viewerTestEntries(3)
	wantCaller := entryPaths(entries)
	wantViewer := []string{entries[0].Path, entries[2].Path}

	var v Viewer
	v.Show(entries, 1)
	v.RequestDelete()
	v.ConfirmDelete(func(string) {})

	assertPaths(t, "caller entries", entries, wantCaller)
	assertPaths(t, "viewer entries", v.entries, wantViewer)
	if !v.entriesOwned {
		t.Fatal("ConfirmDelete did not mark the viewer entries owned, want a private copy")
	}
}

// TestViewerFavoriteKeepsCallerSlice covers G4-03 for the write paths: both the
// 'F' toggle (mutableEntries) and SetFavorite must mutate only the viewer's
// private copy, and SetFavorite with nothing to change must not copy at all.
func TestViewerFavoriteKeepsCallerSlice(t *testing.T) {
	t.Run("mutableEntries clones on first write", func(t *testing.T) {
		entries := viewerTestEntries(3)
		var v Viewer
		v.Show(entries, 0)

		e := &v.mutableEntries()[1]
		e.Favorite = true

		if entries[1].Favorite {
			t.Fatal("favorite write leaked into the caller's entries")
		}
		if !v.entries[1].Favorite {
			t.Fatal("favorite not set on the viewer's entries")
		}
		if !v.entriesOwned {
			t.Fatal("mutableEntries did not mark the viewer entries owned")
		}
	})

	t.Run("SetFavorite clones on first write", func(t *testing.T) {
		entries := viewerTestEntries(3)
		wantCaller := entryPaths(entries)
		var v Viewer
		v.Show(entries, 0)

		v.SetFavorite(entries[2].Path, true)

		assertPaths(t, "caller entries", entries, wantCaller)
		if entries[2].Favorite {
			t.Fatal("SetFavorite leaked into the caller's entries")
		}
		if !v.entries[2].Favorite {
			t.Fatal("SetFavorite did not set the viewer's copy")
		}
	})

	t.Run("SetFavorite no-op leaves entries shared", func(t *testing.T) {
		entries := viewerTestEntries(3)
		var v Viewer
		v.Show(entries, 0)

		// entries[2].Favorite is already false, so nothing is written.
		v.SetFavorite(entries[2].Path, false)

		if v.entriesOwned {
			t.Fatal("SetFavorite with an unchanged value copied the entries")
		}
		if &v.entries[0] != &entries[0] {
			t.Fatal("SetFavorite with an unchanged value stopped sharing the caller's slice")
		}
	})
}

// TestGridIndexFor covers the fast path (a correct hint costs one comparison),
// the fallback scan (the list changed under the viewer), out-of-range hints and
// missing paths.
func TestGridIndexFor(t *testing.T) {
	entries := viewerTestEntries(4)
	cases := []struct {
		name    string
		entries []cache.Entry
		hint    int
		path    string
		want    int
	}{
		{"hint correct", entries, 2, entries[2].Path, 2},
		{"hint wrong, path present", entries, 2, entries[0].Path, 0},
		{"hint -1 falls back to scan", entries, -1, entries[3].Path, 3},
		{"hint len falls back to scan", entries, len(entries), entries[1].Path, 1},
		{"path absent", entries, 0, "/nonexistent/missing.bin", -1},
		{"empty slice", nil, 0, entries[0].Path, -1},
		{"empty slice with -1 hint", nil, -1, "/nonexistent/missing.bin", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gridIndexFor(tc.entries, tc.hint, tc.path); got != tc.want {
				t.Fatalf("gridIndexFor(%d entries, hint %d, %q) = %d, want %d",
					len(tc.entries), tc.hint, tc.path, got, tc.want)
			}
		})
	}
}
