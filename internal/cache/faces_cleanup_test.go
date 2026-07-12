package cache

import "testing"

// writeFace inserts one face row (with a fresh cluster) for path, so the
// delete/move cleanup paths have something to act on.
func writeFace(t *testing.T, idx *Index, path string) {
	t.Helper()
	if _, err := idx.WriteFacesForPath(path, []FaceOp{{
		Face:               Face{Path: path, ThumbMtime: 1, BBox: [4]int{1, 2, 3, 4}, Embedding: []float32{0.1, 0.2}},
		NewClusterCentroid: []float32{0.1, 0.2},
	}}); err != nil {
		t.Fatalf("WriteFacesForPath(%s): %v", path, err)
	}
}

// TestRemoveEntryDeletesFaces verifies RemoveEntry drops the path's face rows in
// the same call — without this they orphan forever (C-01).
func TestRemoveEntryDeletesFaces(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	root := "/lib"
	seed(t, idx, root, 1)
	path := root + "/000000.jpg"
	writeFace(t, idx, path)

	if got := len(idx.LoadFaceFreshness()); got != 1 {
		t.Fatalf("before RemoveEntry: face rows = %d, want 1", got)
	}
	if err := idx.RemoveEntry(path); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if got := idx.Count(); got != 0 {
		t.Errorf("after RemoveEntry: Count = %d, want 0", got)
	}
	if got := len(idx.LoadFaceFreshness()); got != 0 {
		t.Errorf("after RemoveEntry: face rows = %d, want 0 (faces must not orphan)", got)
	}
}

// TestRemoveEntriesDeletesFaces verifies the batch delete cleans up faces for
// every removed path.
func TestRemoveEntriesDeletesFaces(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	root := "/lib"
	seed(t, idx, root, 2)
	p0, p1 := root+"/000000.jpg", root+"/000001.jpg"
	writeFace(t, idx, p0)
	writeFace(t, idx, p1)

	if got := len(idx.LoadFaceFreshness()); got != 2 {
		t.Fatalf("before RemoveEntries: face rows = %d, want 2", got)
	}
	if err := idx.RemoveEntries([]string{p0, p1}); err != nil {
		t.Fatalf("RemoveEntries: %v", err)
	}
	if got := len(idx.LoadFaceFreshness()); got != 0 {
		t.Errorf("after RemoveEntries: face rows = %d, want 0", got)
	}
}

// TestMoveFacesRelocatesRows verifies MoveFaces re-keys the face rows to the new
// path (preserving thumb_mtime) rather than dropping them, so a moved file
// doesn't have to be re-detected from scratch (C-01).
func TestMoveFacesRelocatesRows(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	root := "/lib"
	seed(t, idx, root, 1)
	oldPath := root + "/000000.jpg"
	newPath := root + "/2024-01-01/000000.jpg"
	writeFace(t, idx, oldPath)

	if err := idx.MoveFaces(oldPath, newPath); err != nil {
		t.Fatalf("MoveFaces: %v", err)
	}

	fresh := idx.LoadFaceFreshness()
	if _, ok := fresh[oldPath]; ok {
		t.Errorf("face row still keyed to oldPath %s after MoveFaces", oldPath)
	}
	if mtime, ok := fresh[newPath]; !ok {
		t.Errorf("no face row under newPath %s after MoveFaces", newPath)
	} else if mtime != 1 {
		t.Errorf("relocated face thumb_mtime = %d, want 1 (preserved)", mtime)
	}
}
