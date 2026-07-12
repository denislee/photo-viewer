package cache

import "testing"

// TestSetDurationMsPersists is the W-06 write-back guard. SetDurationMs records
// a probed video duration on the row so the HLS handler stops re-forking
// ffprobe for duration-less videos. A subsequent read must observe the value, a
// non-positive ms must be ignored (0 is the "duration unknown" sentinel and a
// bogus probe must never cement it), setting one row must not touch its
// neighbours, and a missing path must be a harmless no-op.
func TestSetDurationMsPersists(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	root := "/lib"
	entries := seed(t, idx, root, 3)
	path := entries[0].Path

	// Seeded entries carry duration_ms = 0 (the reconcile default).
	if e, ok := idx.GetEntryByThumbID(ThumbIDFor(path)); !ok || e.DurationMs != 0 {
		t.Fatalf("seeded DurationMs = %d (ok=%v), want 0", e.DurationMs, ok)
	}

	if err := idx.SetDurationMs(path, 12345); err != nil {
		t.Fatalf("SetDurationMs: %v", err)
	}
	e, ok := idx.GetEntryByThumbID(ThumbIDFor(path))
	if !ok {
		t.Fatal("entry vanished after SetDurationMs")
	}
	if e.DurationMs != 12345 {
		t.Errorf("DurationMs = %d after SetDurationMs, want 12345", e.DurationMs)
	}

	// Non-positive values are ignored so the stored duration survives intact.
	if err := idx.SetDurationMs(path, 0); err != nil {
		t.Fatalf("SetDurationMs(0): %v", err)
	}
	if err := idx.SetDurationMs(path, -5); err != nil {
		t.Fatalf("SetDurationMs(-5): %v", err)
	}
	if e, _ := idx.GetEntryByThumbID(ThumbIDFor(path)); e.DurationMs != 12345 {
		t.Errorf("DurationMs = %d after ignored non-positive writes, want 12345", e.DurationMs)
	}

	// Setting one row must not touch its neighbours.
	for _, n := range entries[1:] {
		if e, _ := idx.GetEntryByThumbID(ThumbIDFor(n.Path)); e.DurationMs != 0 {
			t.Errorf("neighbour %s DurationMs = %d, want 0", n.Path, e.DurationMs)
		}
	}

	// A path with no matching row updates nothing and reports no error.
	if err := idx.SetDurationMs("/lib/does-not-exist.mp4", 5000); err != nil {
		t.Fatalf("SetDurationMs on missing path err = %v, want nil", err)
	}
}
