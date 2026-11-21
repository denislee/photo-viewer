package ui

import (
	"context"
	"errors"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gioui.org/layout"
	"gioui.org/op"
	"gioui.org/unit"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// newDuplicatesTestIndex opens a throwaway on-disk index and closes it when the
// test ends.
func newDuplicatesTestIndex(t *testing.T) *cache.Index {
	t.Helper()
	idx, err := cache.Load(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

// TestPruneMissingBatchesRemovals covers U3-05: every vanished row is
// collected across all groups and dropped with exactly one batched
// RemoveEntries transaction, and groups left with a single survivor (or none)
// are dropped from the result.
func TestPruneMissingBatchesRemovals(t *testing.T) {
	idx := newDuplicatesTestIndex(t)
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)

	// Three groups of four real files each, all reconciled into the index.
	var groups []cache.DuplicateGroup
	var results []scan.Result
	for gi := range 3 {
		g := cache.DuplicateGroup{Hash: fmt.Sprintf("hash-%d", gi)}
		for ei := range 4 {
			p := filepath.Join(dir, fmt.Sprintf("g%d-e%d.jpg", gi, ei))
			if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
				t.Fatalf("write %s: %v", p, err)
			}
			g.Entries = append(g.Entries, cache.Entry{
				Path:    p,
				Type:    scan.TypePhoto,
				Size:    1,
				ModTime: now,
			})
			results = append(results, scan.Result{
				Path:    p,
				Type:    scan.TypePhoto,
				Size:    1,
				ModTime: now,
			})
		}
		groups = append(groups, g)
	}
	idx.ReconcileBatch(results)

	// Vanished files: 2 from group 0, 2 from group 1, 3 from group 2 (which
	// therefore drops to a single survivor and must disappear from the result).
	var deleted []string
	for gi, n := range []int{2, 2, 3} {
		for ei := range n {
			p := groups[gi].Entries[ei].Path
			if err := os.Remove(p); err != nil {
				t.Fatalf("remove %s: %v", p, err)
			}
			deleted = append(deleted, p)
		}
	}

	d := NewDuplicatesView(idx, nil, nil, nil)
	calls := 0
	var got []string
	d.removeEntries = func(paths []string) error {
		calls++
		got = append(got, paths...)
		return idx.RemoveEntries(paths)
	}

	out := d.pruneMissing(groups)

	if calls != 1 {
		t.Errorf("RemoveEntries batches = %d, want 1", calls)
	}
	if len(got) != 7 {
		t.Errorf("RemoveEntries paths = %d, want 7 (%v)", len(got), got)
	}
	if len(out) != 2 {
		t.Fatalf("groups = %d, want 2", len(out))
	}
	for i, wantHash := range []string{"hash-0", "hash-1"} {
		if out[i].Hash != wantHash {
			t.Errorf("group %d hash = %q, want %q", i, out[i].Hash, wantHash)
		}
		if len(out[i].Entries) != 2 {
			t.Errorf("group %d entries = %d, want 2", i, len(out[i].Entries))
		}
	}

	for _, p := range deleted {
		if _, ok := idx.GetEntry(p); ok {
			t.Errorf("deleted path still indexed: %s", p)
		}
	}
	for gi, g := range groups {
		for _, e := range g.Entries {
			_, err := os.Stat(e.Path)
			if os.IsNotExist(err) {
				continue // deleted above, already asserted gone from the index
			}
			if _, ok := idx.GetEntry(e.Path); !ok {
				t.Errorf("surviving path missing from index: group %d %s", gi, e.Path)
			}
		}
	}
}

// TestDuplicatesClickOnLastVisibleRowSelects covers U4-13: the click sweep only
// drains the window the list laid out last frame, and the last row in that
// window still selects when clicked.
func TestDuplicatesClickOnLastVisibleRowSelects(t *testing.T) {
	idx := newDuplicatesTestIndex(t)
	d := NewDuplicatesView(idx, nil, nil, nil)

	// 60 synthetic groups of 2 entries — paths need not exist, Layout never
	// touches the store or thumbs cache (both nil from NewDuplicatesView).
	d.mu.Lock()
	d.groups = make([]cache.DuplicateGroup, 0, 60)
	for gi := range 60 {
		d.groups = append(d.groups, cache.DuplicateGroup{
			Hash: fmt.Sprintf("h%d", gi),
			Entries: []cache.Entry{
				{
					Path:    fmt.Sprintf("/synthetic/%d-a.jpg", gi),
					Type:    scan.TypePhoto,
					Size:    int64(gi + 1),
					ModTime: time.Unix(1700000000+int64(gi), 0),
				},
				{
					Path:    fmt.Sprintf("/synthetic/%d-b.jpg", gi),
					Type:    scan.TypePhoto,
					Size:    int64(gi + 1),
					ModTime: time.Unix(1700000100+int64(gi), 0),
				},
			},
		})
	}
	d.selected = -1
	d.mu.Unlock()

	th := NewTheme()
	var ops op.Ops
	gtx := layout.Context{
		Ops:         &ops,
		Now:         time.Now(),
		Metric:      unit.Metric{PxPerDp: 1, PxPerSp: 1},
		Constraints: layout.Exact(image.Pt(1000, 700)),
	}

	d.Layout(gtx, th)

	if d.groupList.Position.Count <= 0 {
		t.Fatalf("list laid out no rows: %+v", d.groupList.Position)
	}
	last := d.groupList.Position.First + d.groupList.Position.Count - 1
	if last >= 59 {
		t.Fatalf("list was not clipped: last visible row %d of 59", last)
	}

	d.groupClicks[last].Click()
	ops.Reset()
	d.Layout(gtx, th)

	d.mu.Lock()
	selected := d.selected
	d.mu.Unlock()
	if selected != last {
		t.Fatalf("selected = %d, want %d (last visible row)", selected, last)
	}
}

// TestDuplicatesStaleRowCancelSparesNewPass covers U6-14: a pass's process-bar
// Cancel must cancel that pass, not whatever pass d.cancel holds by the time
// the (still visible) old row is clicked.
func TestDuplicatesStaleRowCancelSparesNewPass(t *testing.T) {
	d := NewDuplicatesView(newDuplicatesTestIndex(t), nil, nil, func() {})
	var reg *ProcessRegistry
	var first atomic.Pointer[Process]
	reg = NewProcessRegistry(func() {
		for _, s := range reg.Snapshot() {
			if s.Kind == ProcDuplicates && first.Load() == nil {
				first.CompareAndSwap(nil, reg.Get(s.ID))
			}
		}
	})
	d.SetProcessRegistry(reg)

	d.startHash()
	deadline := time.Now().Add(5 * time.Second)
	for first.Load() == nil || reg.Count() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("first pass never registered or never finished")
		}
		time.Sleep(2 * time.Millisecond)
	}

	// A newer pass now owns d.cancel.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	d.mu.Lock()
	d.cancel = cancel2
	d.mu.Unlock()

	first.Load().Cancel()
	if ctx2.Err() != nil {
		t.Fatal("the old pass's Cancel cancelled the newer pass")
	}
}

// TestDupStatusForErr: a pass cancelled through its own context (Cancel or a
// superseding Rescan) reads as "Cancelled.", while a real failure keeps the
// "Error: " prefix. Wrapped context errors count as cancellation too.
func TestDupStatusForErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"cancelled", context.Canceled, "Cancelled."},
		{"wrapped cancelled", fmt.Errorf("hash pass: %w", context.Canceled), "Cancelled."},
		{"other error", errors.New("disk full"), "Error: disk full"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dupStatusForErr(tc.err); got != tc.want {
				t.Fatalf("dupStatusForErr(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
