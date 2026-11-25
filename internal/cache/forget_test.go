package cache

import "testing"

// TestForgetFacesWipesBiometrics guards "Forget all face data": every table
// holding embeddings or embedding-derived centroids is emptied (faces,
// face_clusters, person_anchors; face_tags only with alsoPeople), and
// DeletePerson drops that person's anchors.
func TestForgetFacesWipesBiometrics(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	seed(t, idx, "/lib", 2)
	a := mustCreatePerson(t, idx, "A")
	b := mustCreatePerson(t, idx, "B")
	fa, ca := writeOneFace(t, idx, "/lib/000000.jpg", vec(0, 1), [4]float64{0.1, 0.1, 0.1, 0.1})
	_, cb := writeOneFace(t, idx, "/lib/000001.jpg", vec(1, 1), [4]float64{0.1, 0.1, 0.1, 0.1})
	mustTagFaces(t, idx, []int64{fa}, VerdictConfirmed, a)
	if _, err := idx.AssignClusterToPerson(ca, a); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.AssignClusterToPerson(cb, b); err != nil {
		t.Fatal(err)
	}
	if err := idx.Clear(); err != nil { // saves anchors for A and B
		t.Fatal(err)
	}
	if n := countRows(t, idx, "SELECT COUNT(*) FROM person_anchors"); n != 2 {
		t.Fatalf("anchors after Clear = %d, want 2", n)
	}
	if _, err := idx.DeletePerson(b); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, idx, "SELECT COUNT(*) FROM person_anchors"); n != 1 {
		t.Errorf("anchors after DeletePerson(B) = %d, want 1", n)
	}
	if err := idx.ForgetFaces(false); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"faces", "face_clusters", "face_tag_links", "person_anchors"} {
		if n := countRows(t, idx, "SELECT COUNT(*) FROM "+q); n != 0 {
			t.Errorf("%s after ForgetFaces(false) = %d, want 0", q, n)
		}
	}
	if n := countRows(t, idx, "SELECT COUNT(*) FROM face_tags"); n != 1 {
		t.Errorf("face_tags after ForgetFaces(false) = %d, want 1 (kept)", n)
	}
	if err := idx.ForgetFaces(true); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"face_tags", "people"} {
		if n := countRows(t, idx, "SELECT COUNT(*) FROM "+q); n != 0 {
			t.Errorf("%s after ForgetFaces(true) = %d, want 0", q, n)
		}
	}
}
