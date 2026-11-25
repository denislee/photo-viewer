package cache

import "testing"

// countRows runs a COUNT(*) query and returns the number.
func countRows(t *testing.T, idx *Index, query string, args ...any) int {
	t.Helper()
	var n int
	if err := idx.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestFaceTagSurvivesRedetectionBoxShift guards W4 durability: a confirmed
// name must survive a re-detection whose box and embedding drifted a little.
func TestFaceTagSurvivesRedetectionBoxShift(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	path := "/lib/000000.jpg"
	a := mustCreatePerson(t, idx, "A")

	box := [4]float64{0.2, 0.2, 0.2, 0.2}
	emb := vec(0, 1)
	face, _ := writeOneFace(t, idx, path, emb, box)
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	// Re-detection: the same face, box shifted by 0.02 and embedding jittered
	// by 0.01 on one axis, landing in a brand-new cluster.
	redetected, _ := writeOneFace(t, idx, path, jitterEmb(emb, 0, 0.01), [4]float64{0.22, 0.22, 0.2, 0.2})
	if redetected == face {
		t.Fatalf("re-detection reused face id %d, want a fresh row", face)
	}
	wantIdentity(t, idx, redetected, a, AssignUser)
}

// TestFaceTagSurvivesRedetectionWithoutNBox guards the fallback: rows whose
// normalised box is unknown are matched by embedding alone.
func TestFaceTagSurvivesRedetectionWithoutNBox(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	path := "/lib/000000.jpg"
	a := mustCreatePerson(t, idx, "A")

	emb := vec(0, 1)
	face, _ := writeOneFace(t, idx, path, emb, [4]float64{0.2, 0.2, 0.2, 0.2})
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	// The same embedding, but NBox is unknown: IoU is 0, so only the
	// embedding can re-attach the tag.
	redetected, _ := writeOneFace(t, idx, path, emb, NoNBox)
	wantIdentity(t, idx, redetected, a, AssignUser)
}

// TestFaceTagDormantOnDifferentFace guards both halves of the matcher: a
// different face in the same photo must not steal the tag (it stays dormant),
// and the tag re-applies when the original face comes back.
func TestFaceTagDormantOnDifferentFace(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	path := "/lib/000000.jpg"
	a := mustCreatePerson(t, idx, "A")

	emb := vec(0, 1)
	face, _ := writeOneFace(t, idx, path, emb, [4]float64{0.2, 0.2, 0.2, 0.2})
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	// A completely different face: zero IoU with the tagged box, embedding
	// vec(1,1) is 2 away from the tag (and far beyond TagMaxSqDistance).
	other, _ := writeOneFace(t, idx, path, vec(1, 1), [4]float64{0.7, 0.7, 0.1, 0.1})
	wantIdentity(t, idx, other, 0, AssignCluster)
	if n := countRows(t, idx, "SELECT COUNT(*) FROM face_tags WHERE path = ?", path); n != 1 {
		t.Errorf("face_tags for %q = %d, want the dormant tag kept (1)", path, n)
	}

	// The original face returns on the next detector run: the tag re-applies.
	back, _ := writeOneFace(t, idx, path, emb, [4]float64{0.2, 0.2, 0.2, 0.2})
	wantIdentity(t, idx, back, a, AssignUser)
}

// TestClearKeepsPeopleAndFaceTags guards that a rebuild (Clear) wipes only the
// machine layer: names and tags survive and re-attach to re-detected faces.
func TestClearKeepsPeopleAndFaceTags(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	path := "/lib/000000.jpg"
	a := mustCreatePerson(t, idx, "A")

	box := [4]float64{0.2, 0.2, 0.2, 0.2}
	emb := vec(0, 1)
	face, _ := writeOneFace(t, idx, path, emb, box)
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	peopleBefore := countRows(t, idx, "SELECT COUNT(*) FROM people")
	tagsBefore := countRows(t, idx, "SELECT COUNT(*) FROM face_tags")

	if err := idx.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	if got := countRows(t, idx, "SELECT COUNT(*) FROM people"); got != peopleBefore {
		t.Errorf("people after Clear = %d, want %d", got, peopleBefore)
	}
	if got := countRows(t, idx, "SELECT COUNT(*) FROM face_tags"); got != tagsBefore {
		t.Errorf("face_tags after Clear = %d, want %d", got, tagsBefore)
	}
	if got := countRows(t, idx, "SELECT COUNT(*) FROM face_tag_links"); got != 0 {
		t.Errorf("face_tag_links after Clear = %d, want 0", got)
	}

	// The rescan brings the entry back; the next detection run must get the
	// name back from the surviving tag.
	seed(t, idx, "/lib", 1)
	redetected, _ := writeOneFace(t, idx, path, emb, box)
	wantIdentity(t, idx, redetected, a, AssignUser)
}

// TestMovePathCarriesFaceTags guards that an organize move re-keys the tag and
// the name still applies to faces written under the new path.
func TestMovePathCarriesFaceTags(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	oldPath, newPath := "/lib/000000.jpg", "/lib/moved.jpg"
	a := mustCreatePerson(t, idx, "A")

	box := [4]float64{0.2, 0.2, 0.2, 0.2}
	emb := vec(0, 1)
	face, _ := writeOneFace(t, idx, oldPath, emb, box)
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	if err := idx.MovePath(oldPath, newPath); err != nil {
		t.Fatalf("MovePath: %v", err)
	}
	var tagPath string
	if err := idx.db.QueryRow("SELECT path FROM face_tags").Scan(&tagPath); err != nil {
		t.Fatalf("select face_tags.path: %v", err)
	}
	if tagPath != newPath {
		t.Errorf("face_tags.path = %q after MovePath, want %q", tagPath, newPath)
	}

	redetected, _ := writeOneFace(t, idx, newPath, emb, box)
	wantIdentity(t, idx, redetected, a, AssignUser)
}

// TestMoveFacesCarriesFaceTags guards the same carry-over for MoveFaces when
// there is no entries row to move (the ApplyMove fallback path).
func TestMoveFacesCarriesFaceTags(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	oldPath, newPath := "/lib/ghost.jpg", "/lib/ghost-moved.jpg"
	if _, ok := idx.GetEntry(oldPath); ok {
		t.Fatalf("test setup: %q unexpectedly has an entries row", oldPath)
	}
	a := mustCreatePerson(t, idx, "A")

	box := [4]float64{0.2, 0.2, 0.2, 0.2}
	emb := vec(0, 1)
	face, _ := writeOneFace(t, idx, oldPath, emb, box)
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	if err := idx.MoveFaces(oldPath, newPath); err != nil {
		t.Fatalf("MoveFaces: %v", err)
	}
	var tagPath string
	if err := idx.db.QueryRow("SELECT path FROM face_tags").Scan(&tagPath); err != nil {
		t.Fatalf("select face_tags.path: %v", err)
	}
	if tagPath != newPath {
		t.Errorf("face_tags.path = %q after MoveFaces, want %q", tagPath, newPath)
	}

	redetected, _ := writeOneFace(t, idx, newPath, emb, box)
	wantIdentity(t, idx, redetected, a, AssignUser)
}

// TestRemoveEntryKeepsFaceTagsAndSweep guards the trash round-trip: deleting the
// entry must not drop the name, the sweep spares trashed paths, and a sweep
// without a keep set reaps the orphan.
func TestRemoveEntryKeepsFaceTagsAndSweep(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	path := "/lib/000000.jpg"
	a := mustCreatePerson(t, idx, "A")

	face, _ := writeOneFace(t, idx, path, vec(0, 1), [4]float64{0.2, 0.2, 0.2, 0.2})
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	if err := idx.RemoveEntry(path); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if n := countRows(t, idx, "SELECT COUNT(*) FROM face_tags WHERE path = ?", path); n != 1 {
		t.Fatalf("face_tags for the deleted entry = %d, want 1 (kept for a restore)", n)
	}

	// A trashed file's path is in the keep set: the sweep spares it.
	n, err := idx.SweepOrphanTags(map[string]bool{path: true})
	if err != nil {
		t.Fatalf("SweepOrphanTags(keep): %v", err)
	}
	if n != 0 {
		t.Errorf("SweepOrphanTags(keep) removed %d tags, want 0", n)
	}
	if got := countRows(t, idx, "SELECT COUNT(*) FROM face_tags WHERE path = ?", path); got != 1 {
		t.Errorf("face_tags after kept sweep = %d, want 1", got)
	}

	// No keep set: the orphan is reaped.
	n, err = idx.SweepOrphanTags(nil)
	if err != nil {
		t.Fatalf("SweepOrphanTags(nil): %v", err)
	}
	if n != 1 {
		t.Errorf("SweepOrphanTags(nil) removed %d tags, want 1", n)
	}
	if got := countRows(t, idx, "SELECT COUNT(*) FROM face_tags WHERE path = ?", path); got != 0 {
		t.Errorf("face_tags after orphan sweep = %d, want 0", got)
	}
}
