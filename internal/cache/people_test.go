package cache

import (
	"errors"
	"testing"
)

// vec builds the 128-d embedding used throughout these tests: all zeros
// except index axis. vec(0,1) and vec(1,1) are 2 apart (different faces);
// vec(0,1) and vec(0,0.9) are 0.01 apart (the same face seen twice).
func vec(axis int, v float32) []float32 {
	out := make([]float32, 128)
	out[axis] = v
	return out
}

// jitterEmb copies emb with delta added on axis: the small per-detection drift
// a re-detection of the same face produces.
func jitterEmb(emb []float32, axis int, delta float32) []float32 {
	out := append([]float32(nil), emb...)
	out[axis] += delta
	return out
}

// writeOneFace rewrites path with a single face in a fresh cluster and returns
// the new face id and cluster id.
func writeOneFace(t *testing.T, idx *Index, path string, emb []float32, box [4]float64) (faceID, clusterID int64) {
	t.Helper()
	res, err := idx.WriteFacesForPath(path, 1, []FaceOp{{
		Face:               Face{Path: path, ThumbMtime: 1, Embedding: emb, NBox: box, Quality: 0.5},
		NewClusterCentroid: emb,
	}})
	if err != nil {
		t.Fatalf("WriteFacesForPath(%q): %v", path, err)
	}
	if len(res) != 1 {
		t.Fatalf("WriteFacesForPath(%q) returned %d results, want 1", path, len(res))
	}
	return res[0].FaceID, res[0].ClusterID
}

// clusterOf writes one face per path into a single fresh cluster and returns
// the cluster id plus the face ids in path order.
func clusterOf(t *testing.T, idx *Index, paths []string, embs [][]float32, box [4]float64) (int64, []int64) {
	t.Helper()
	if len(paths) == 0 || len(paths) != len(embs) {
		t.Fatalf("clusterOf: %d paths, %d embeddings", len(paths), len(embs))
	}
	res, err := idx.WriteFacesForPath(paths[0], 1, []FaceOp{{
		Face:               Face{Path: paths[0], ThumbMtime: 1, Embedding: embs[0], NBox: box, Quality: 0.5},
		NewClusterCentroid: embs[0],
	}})
	if err != nil {
		t.Fatalf("WriteFacesForPath(%q): %v", paths[0], err)
	}
	if len(res) != 1 {
		t.Fatalf("WriteFacesForPath(%q) returned %d results, want 1", paths[0], len(res))
	}
	cluster := res[0].ClusterID
	faces := []int64{res[0].FaceID}
	for k := 1; k < len(paths); k++ {
		res, err := idx.WriteFacesForPath(paths[k], 1, []FaceOp{{
			Face:              Face{Path: paths[k], ThumbMtime: 1, Embedding: embs[k], NBox: box, Quality: 0.5},
			ExistingClusterID: cluster,
			UpdatedCentroid:   embs[k],
		}})
		if err != nil {
			t.Fatalf("WriteFacesForPath(%q): %v", paths[k], err)
		}
		if len(res) != 1 {
			t.Fatalf("WriteFacesForPath(%q) returned %d results, want 1", paths[k], len(res))
		}
		faces = append(faces, res[0].FaceID)
	}
	return cluster, faces
}

// wantIdentity fails unless faceID currently resolves to personID (0 = nobody)
// with the given assign state.
func wantIdentity(t *testing.T, idx *Index, faceID, personID int64, assign string) {
	t.Helper()
	f, ok := idx.Face(faceID)
	if !ok {
		t.Fatalf("face %d not found", faceID)
	}
	if personID == 0 {
		if f.PersonID.Valid {
			t.Errorf("face %d person = %d, want unmatched", faceID, f.PersonID.Int64)
		}
	} else if !f.PersonID.Valid || f.PersonID.Int64 != personID {
		t.Errorf("face %d person = %v, want %d", faceID, f.PersonID, personID)
	}
	if f.Assign != assign {
		t.Errorf("face %d assign = %q, want %q", faceID, f.Assign, assign)
	}
}

func mustCreatePerson(t *testing.T, idx *Index, name string) int64 {
	t.Helper()
	id, err := idx.CreatePerson(name)
	if err != nil {
		t.Fatalf("CreatePerson(%q): %v", name, err)
	}
	return id
}

func mustAssignCluster(t *testing.T, idx *Index, clusterID, personID int64) *Undo {
	t.Helper()
	u, err := idx.AssignClusterToPerson(clusterID, personID)
	if err != nil {
		t.Fatalf("AssignClusterToPerson(%d, %d): %v", clusterID, personID, err)
	}
	return u
}

func mustTagFaces(t *testing.T, idx *Index, faceIDs []int64, verdict Verdict, personID int64) *Undo {
	t.Helper()
	u, err := idx.TagFaces(faceIDs, verdict, personID)
	if err != nil {
		t.Fatalf("TagFaces(%v, %q, %d): %v", faceIDs, verdict, personID, err)
	}
	return u
}

// TestPeopleCreateAndRename guards the name-uniqueness contract of the People
// UI: names collide case-insensitively, blanks are rejected, and a person may
// still re-case their own name.
func TestPeopleCreateAndRename(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	first, err := idx.CreatePerson("Ana")
	if err != nil {
		t.Fatalf("CreatePerson(Ana): %v", err)
	}

	_, err = idx.CreatePerson("ana")
	var exists *ErrNameExists
	if !errors.As(err, &exists) {
		t.Fatalf("CreatePerson(ana) error = %v, want *ErrNameExists", err)
	}
	if exists.ID != first {
		t.Errorf("ErrNameExists.ID = %d, want the existing person %d", exists.ID, first)
	}

	if _, err := idx.CreatePerson("   "); !errors.Is(err, ErrEmptyName) {
		t.Errorf("CreatePerson(blank) error = %v, want ErrEmptyName", err)
	}

	second := mustCreatePerson(t, idx, "Bob")
	_, err = idx.RenamePerson(first, "bob")
	exists = nil
	if !errors.As(err, &exists) {
		t.Fatalf("RenamePerson(Ana -> bob) error = %v, want *ErrNameExists", err)
	}
	if exists.ID != second {
		t.Errorf("rename ErrNameExists.ID = %d, want the other person %d", exists.ID, second)
	}

	if _, err := idx.RenamePerson(first, "ANA"); err != nil {
		t.Fatalf("RenamePerson(Ana -> ANA): %v", err)
	}
	if got, ok := idx.PersonName(first); !ok || got != "ANA" {
		t.Errorf("PersonName = (%q, %v), want (\"ANA\", true)", got, ok)
	}
}

// TestAssignClusterToPerson guards that naming a group attributes all of its
// faces (without confirming them) and that the undo takes the attribution back.
func TestAssignClusterToPerson(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 2)
	paths := []string{"/lib/000000.jpg", "/lib/000001.jpg"}
	a := mustCreatePerson(t, idx, "A")

	cluster, faces := clusterOf(t, idx, paths, [][]float32{vec(0, 1), vec(0, 1)}, NoNBox)
	u := mustAssignCluster(t, idx, cluster, a)
	for _, id := range faces {
		wantIdentity(t, idx, id, a, AssignCluster)
	}

	people, err := idx.ListPeople()
	if err != nil {
		t.Fatalf("ListPeople: %v", err)
	}
	if len(people) != 1 {
		t.Fatalf("ListPeople returned %d people, want 1", len(people))
	}
	if people[0].ID != a {
		t.Errorf("ListPeople[0].ID = %d, want %d", people[0].ID, a)
	}
	if people[0].Photos != 2 || people[0].Faces != 2 {
		t.Errorf("person A = %d photos / %d faces, want 2 / 2", people[0].Photos, people[0].Faces)
	}

	if err := idx.ApplyUndo(u); err != nil {
		t.Fatalf("ApplyUndo: %v", err)
	}
	for _, id := range faces {
		wantIdentity(t, idx, id, 0, AssignCluster)
	}
}

// TestTagFacesConfirmedThenClusterReassign guards that a confirmed face stays
// with the confirmed person even when its cluster is later renamed.
func TestTagFacesConfirmedThenClusterReassign(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	b := mustCreatePerson(t, idx, "B")

	face, cluster := writeOneFace(t, idx, "/lib/a.jpg", vec(0, 1), NoNBox)
	mustAssignCluster(t, idx, cluster, b)
	wantIdentity(t, idx, face, b, AssignCluster)

	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)
	wantIdentity(t, idx, face, a, AssignUser)

	mustAssignCluster(t, idx, cluster, b)
	wantIdentity(t, idx, face, a, AssignUser)
}

// TestTagFacesRejectedKeepsClusterMate guards that rejecting one face for the
// cluster's person unmakes only that face, leaving its cluster mate named.
func TestTagFacesRejectedKeepsClusterMate(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	cluster, faces := clusterOf(t, idx, []string{"/lib/a.jpg", "/lib/b.jpg"},
		[][]float32{vec(0, 1), vec(0, 1)}, NoNBox)
	mustAssignCluster(t, idx, cluster, a)

	mustTagFaces(t, idx, []int64{faces[0]}, VerdictRejected, a)
	wantIdentity(t, idx, faces[0], 0, AssignCluster)
	wantIdentity(t, idx, faces[1], a, AssignCluster)
}

// TestTagFacesIgnoredThenCleared guards that ignoring a face drops its identity
// and that clearing the decision lets the cluster's person apply again.
func TestTagFacesIgnoredThenCleared(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	face, cluster := writeOneFace(t, idx, "/lib/a.jpg", vec(0, 1), NoNBox)
	mustAssignCluster(t, idx, cluster, a)

	mustTagFaces(t, idx, []int64{face}, VerdictIgnored, 0)
	wantIdentity(t, idx, face, 0, AssignIgnored)

	mustTagFaces(t, idx, []int64{face}, VerdictNone, 0)
	wantIdentity(t, idx, face, a, AssignCluster)
}

// TestMergePeople guards that merging folds src's clusters into dst, removes
// src, and that the undo puts both the person and the attribution back.
func TestMergePeople(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	src := mustCreatePerson(t, idx, "Src")
	dst := mustCreatePerson(t, idx, "Dst")
	cluster, faces := clusterOf(t, idx, []string{"/lib/a.jpg", "/lib/b.jpg"},
		[][]float32{vec(0, 1), vec(0, 0.95)}, NoNBox)
	mustAssignCluster(t, idx, cluster, src)

	u, err := idx.MergePeople(src, dst)
	if err != nil {
		t.Fatalf("MergePeople(%d, %d): %v", src, dst, err)
	}
	for _, id := range faces {
		wantIdentity(t, idx, id, dst, AssignCluster)
	}
	if name, ok := idx.PersonName(src); ok {
		t.Errorf("PersonName(src) = %q after merge, want not found", name)
	}

	if err := idx.ApplyUndo(u); err != nil {
		t.Fatalf("ApplyUndo: %v", err)
	}
	if name, ok := idx.PersonName(src); !ok || name != "Src" {
		t.Errorf("PersonName(src) after undo = (%q, %v), want (\"Src\", true)", name, ok)
	}
	for _, id := range faces {
		wantIdentity(t, idx, id, src, AssignCluster)
	}
}

// TestTagFacesUndoRestoresIdentity guards that undoing a confirmation returns
// the face to the identity it had before (cluster attribution, not nobody).
func TestTagFacesUndoRestoresIdentity(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	b := mustCreatePerson(t, idx, "B")
	face, cluster := writeOneFace(t, idx, "/lib/a.jpg", vec(0, 1), NoNBox)
	mustAssignCluster(t, idx, cluster, a)

	u := mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, b)
	wantIdentity(t, idx, face, b, AssignUser)

	if err := idx.ApplyUndo(u); err != nil {
		t.Fatalf("ApplyUndo: %v", err)
	}
	wantIdentity(t, idx, face, a, AssignCluster)
}

// TestFacesOfPersonOutlierFirst guards the "wrong face first" ordering the
// review UI depends on: the face farthest from the person's centroid leads.
func TestFacesOfPersonOutlierFirst(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	embs := [][]float32{vec(0, 1), vec(0, 0.95), vec(0, 0.2)}
	paths := []string{"/lib/a.jpg", "/lib/b.jpg", "/lib/c.jpg"}
	var ids []int64
	for k := range paths {
		face, cluster := writeOneFace(t, idx, paths[k], embs[k], NoNBox)
		mustAssignCluster(t, idx, cluster, a)
		ids = append(ids, face)
	}

	fs, err := idx.FacesOfPerson(a, OrderOutlierFirst)
	if err != nil {
		t.Fatalf("FacesOfPerson: %v", err)
	}
	if len(fs) != 3 {
		t.Fatalf("FacesOfPerson returned %d faces, want 3", len(fs))
	}
	if fs[0].ID != ids[2] {
		t.Errorf("first face = %d (emb %.2f), want the vec(0,0.2) face %d", fs[0].ID, fs[0].Embedding[0], ids[2])
	}
	if fs[0].Dist <= fs[1].Dist {
		t.Errorf("first face dist %.4f <= second %.4f, want farthest first", fs[0].Dist, fs[1].Dist)
	}
}

// TestSuggestionsExcludeRejectedAndFarFaces guards the suggestion rule: only
// unattributed faces near a confirmed exemplar, and never one the user already
// rejected for that person.
func TestSuggestionsExcludeRejectedAndFarFaces(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")

	x, _ := writeOneFace(t, idx, "/lib/x.jpg", vec(0, 1), NoNBox)
	mustTagFaces(t, idx, []int64{x}, VerdictConfirmed, a)

	y, _ := writeOneFace(t, idx, "/lib/y.jpg", vec(0, 0.9), NoNBox)
	z, _ := writeOneFace(t, idx, "/lib/z.jpg", vec(0, 0.9), NoNBox)
	mustTagFaces(t, idx, []int64{z}, VerdictRejected, a)
	w, _ := writeOneFace(t, idx, "/lib/w.jpg", vec(1, 1), NoNBox)

	all, err := idx.Suggestions()
	if err != nil {
		t.Fatalf("Suggestions: %v", err)
	}
	got := map[int64]bool{}
	for _, f := range all[a] {
		got[f.ID] = true
	}
	if !got[y] {
		t.Errorf("suggestion for A is missing the near face %d", y)
	}
	if got[z] {
		t.Errorf("suggested face %d was already rejected for A", z)
	}
	if got[w] {
		t.Errorf("suggested face %d is too far from A's exemplar", w)
	}
}

// TestListGroupsMinFaces guards the unnamed-groups listing: only unnamed
// clusters with enough faces, never a named one.
func TestListGroupsMinFaces(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")

	big, _ := clusterOf(t, idx, []string{"/lib/a.jpg", "/lib/b.jpg"},
		[][]float32{vec(0, 1), vec(0, 1)}, NoNBox)
	if _, small := writeOneFace(t, idx, "/lib/c.jpg", vec(1, 1), NoNBox); small == big {
		t.Fatalf("small cluster reused the big cluster id %d", big)
	}
	_, namedCluster := writeOneFace(t, idx, "/lib/d.jpg", vec(2, 1), NoNBox)
	mustAssignCluster(t, idx, namedCluster, a)

	groups, err := idx.ListGroups(2)
	if err != nil {
		t.Fatalf("ListGroups(2): %v", err)
	}
	if len(groups) != 1 {
		t.Fatalf("ListGroups(2) returned %d groups, want 1 (cluster %d)", len(groups), big)
	}
	if groups[0].ClusterID != big || groups[0].Faces != 2 {
		t.Errorf("ListGroups(2)[0] = cluster %d with %d faces, want cluster %d with 2", groups[0].ClusterID, groups[0].Faces, big)
	}
	if n := idx.CountGroups(2); n != 1 {
		t.Errorf("CountGroups(2) = %d, want 1", n)
	}
}

// TestFacesInPhotoPersonName guards that the photo viewer gets the person's
// name alongside each face.
func TestFacesInPhotoPersonName(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	face, cluster := writeOneFace(t, idx, "/lib/pic.jpg", vec(0, 1), NoNBox)
	mustAssignCluster(t, idx, cluster, a)

	fs, err := idx.FacesInPhoto("/lib/pic.jpg")
	if err != nil {
		t.Fatalf("FacesInPhoto: %v", err)
	}
	if len(fs) != 1 {
		t.Fatalf("FacesInPhoto returned %d faces, want 1", len(fs))
	}
	if fs[0].ID != face {
		t.Errorf("FacesInPhoto face id = %d, want %d", fs[0].ID, face)
	}
	if fs[0].PersonName != "A" {
		t.Errorf("FacesInPhoto PersonName = %q, want \"A\"", fs[0].PersonName)
	}
}

// TestSetCoverInListPeople guards that a chosen avatar is the cover reported by
// the people overview.
func TestSetCoverInListPeople(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	f1, c1 := writeOneFace(t, idx, "/lib/a.jpg", vec(0, 1), [4]float64{0.1, 0.1, 0.1, 0.1})
	f2, c2 := writeOneFace(t, idx, "/lib/b.jpg", vec(0, 0.95), [4]float64{0.5, 0.5, 0.1, 0.1})
	mustAssignCluster(t, idx, c1, a)
	mustAssignCluster(t, idx, c2, a)

	if _, err := idx.SetCover(a, f2); err != nil {
		t.Fatalf("SetCover(%d, %d): %v", a, f2, err)
	}

	people, err := idx.ListPeople()
	if err != nil {
		t.Fatalf("ListPeople: %v", err)
	}
	if len(people) != 1 {
		t.Fatalf("ListPeople returned %d people, want 1", len(people))
	}
	if people[0].Cover.ID != f2 {
		t.Errorf("Cover.ID = %d, want the chosen face %d (other face %d)", people[0].Cover.ID, f2, f1)
	}
}
