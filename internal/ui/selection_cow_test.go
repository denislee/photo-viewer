package ui

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
)

// TestSnapshotSelectedCopyOnWrite covers G4-02: a snapshot is the published
// map, so it must never change under a reader. Every mutation publishes a
// new map instead.
func TestSnapshotSelectedCopyOnWrite(t *testing.T) {
	dir := t.TempDir()
	c := newTestController(t, filepath.Join(dir, "lib"), filepath.Join(dir, "cache"))

	c.Select("/lib/a.jpg")
	snap := c.SnapshotSelected()
	c.ToggleSelection("/lib/b.jpg")
	c.ToggleSelection("/lib/a.jpg")
	c.Select("/lib/c.jpg")
	if !reflect.DeepEqual(snap, map[string]bool{"/lib/a.jpg": true}) {
		t.Fatalf("snapshot changed under the reader: %v", snap)
	}
	want := map[string]bool{"/lib/b.jpg": true, "/lib/c.jpg": true}
	if got := c.SnapshotSelected(); !reflect.DeepEqual(got, want) {
		t.Fatalf("selection = %v, want %v", got, want)
	}
	snap = c.SnapshotSelected()
	c.ClearSelection()
	if len(snap) != 2 || len(c.SnapshotSelected()) != 0 {
		t.Fatalf("ClearSelection: old snapshot %v, new %v", snap, c.SnapshotSelected())
	}
}

// TestSnapshotSelectedAllocFree: the per-frame read costs no copy.
func TestSnapshotSelectedAllocFree(t *testing.T) {
	dir := t.TempDir()
	c := newTestController(t, filepath.Join(dir, "lib"), filepath.Join(dir, "cache"))
	for i := range 5000 {
		c.Select(fmt.Sprintf("/lib/%d.jpg", i))
	}
	c.SetSelectionMode(true)
	if n := testing.AllocsPerRun(100, func() { _ = c.SnapshotSelected() }); n != 0 {
		t.Fatalf("SnapshotSelected allocs = %v, want 0", n)
	}
	if n := testing.AllocsPerRun(100, func() { _, _ = c.SelectionState() }); n != 0 {
		t.Fatalf("SelectionState allocs = %v, want 0", n)
	}
}

// TestSelectionConcurrentToggleAndRead is meaningful under -race: readers
// iterate published snapshots while writers toggle.
func TestSelectionConcurrentToggleAndRead(t *testing.T) {
	dir := t.TempDir()
	c := newTestController(t, filepath.Join(dir, "lib"), filepath.Join(dir, "cache"))
	var wg sync.WaitGroup
	for w := range 2 {
		wg.Go(func() {
			for i := range 500 {
				c.ToggleSelection(fmt.Sprintf("/lib/%d-%d.jpg", w, i%50))
				if i%100 == 0 {
					c.ClearSelection()
				}
			}
		})
	}
	for range 2 {
		wg.Go(func() {
			for range 500 {
				n := 0
				for p, on := range c.SnapshotSelected() {
					if on && p != "" {
						n++
					}
				}
				_ = n
			}
		})
	}
	wg.Wait()
}

// TestSelectedEntriesKeepsOrder covers G4-08: SelectedEntries filters entries
// against one snapshot and keeps their order.
func TestSelectedEntriesKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	c := newTestController(t, filepath.Join(dir, "lib"), filepath.Join(dir, "cache"))
	entries := []cache.Entry{{Path: "/a"}, {Path: "/b"}, {Path: "/c"}, {Path: "/d"}}
	paths := func(es []cache.Entry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Path)
		}
		return out
	}
	cases := []struct {
		name string
		sel  []string
		want []string
	}{
		{"none", nil, nil},
		{"one", []string{"/c"}, []string{"/c"}},
		{"order follows entries", []string{"/d", "/a", "/c"}, []string{"/a", "/c", "/d"}},
		{"stale path ignored", []string{"/zz", "/b"}, []string{"/b"}},
		{"all", []string{"/b", "/a", "/d", "/c"}, []string{"/a", "/b", "/c", "/d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c.ClearSelection()
			for _, p := range tc.sel {
				c.Select(p)
			}
			if got := paths(c.SelectedEntries(entries)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SelectedEntries = %v, want %v", got, tc.want)
			}
		})
	}
}
