package cache

import "testing"

// TestClear verifies the Rebuild primitive: Clear empties entries, faces, and
// clusters while leaving the same *Index handle usable. This is the core of
// I-01 — a rebuild must not delete/reopen the db (which orphaned the WAL and
// invalidated long-lived *Index captures); it resets the live handle instead.
func TestClear(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	root := "/lib"
	seed(t, idx, root, 5)

	// Add a face + cluster so Clear's faces-table wipe is exercised too.
	if _, err := idx.WriteFacesForPath(root+"/000000.jpg", []FaceOp{{
		Face:               Face{Path: root + "/000000.jpg", ThumbMtime: 1, BBox: [4]int{1, 2, 3, 4}, Embedding: []float32{0.1, 0.2}},
		NewClusterCentroid: []float32{0.1, 0.2},
	}}); err != nil {
		t.Fatalf("WriteFacesForPath: %v", err)
	}

	if got := idx.Count(); got != 5 {
		t.Fatalf("before Clear: Count = %d, want 5", got)
	}
	if got := len(idx.AllClusters()); got != 1 {
		t.Fatalf("before Clear: clusters = %d, want 1", got)
	}
	if got := len(idx.LoadFaceFreshness()); got != 1 {
		t.Fatalf("before Clear: face rows = %d, want 1", got)
	}

	if err := idx.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if got := idx.Count(); got != 0 {
		t.Errorf("after Clear: Count = %d, want 0", got)
	}
	if got := len(idx.AllClusters()); got != 0 {
		t.Errorf("after Clear: clusters = %d, want 0", got)
	}
	if got := len(idx.LoadFaceFreshness()); got != 0 {
		t.Errorf("after Clear: face rows = %d, want 0", got)
	}

	// The handle must remain live — the whole point of clearing in place is
	// that the same *Index keeps working (no reopen, no stale capture).
	seed(t, idx, root, 3)
	if got := idx.Count(); got != 3 {
		t.Fatalf("after re-seed on same handle: Count = %d, want 3", got)
	}
}
