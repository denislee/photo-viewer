package cache

import "testing"

// clusterByID returns the AllClusters entry with the given id.
func clusterByID(idx *Index, id int64) (Cluster, bool) {
	for _, c := range idx.AllClusters() {
		if c.ID == id {
			return c, true
		}
	}
	return Cluster{}, false
}

// TestWriteFacesUnclustered pins the "stored but not clustered" FaceOp: with
// neither ExistingClusterID nor NewClusterCentroid the face keeps a NULL
// cluster_id, no cluster row is created for it, and the result reports 0.
func TestWriteFacesUnclustered(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	path := "/lib/unclustered.jpg"
	res, err := idx.WriteFacesForPath(path, 1, []FaceOp{{
		Face: Face{
			Path: path, ThumbMtime: 1, Embedding: vec(0, 1),
			NBox: [4]float64{0.2, 0.2, 0.2, 0.2}, Quality: 0.01,
		},
	}})
	if err != nil {
		t.Fatalf("WriteFacesForPath(%q): %v", path, err)
	}
	if len(res) != 1 {
		t.Fatalf("WriteFacesForPath(%q) returned %d results, want 1", path, len(res))
	}
	if res[0].ClusterID != 0 {
		t.Errorf("ClusterID = %d, want 0 for an unclustered op", res[0].ClusterID)
	}
	if n := len(idx.AllClusters()); n != 0 {
		t.Errorf("AllClusters() = %d clusters, want 0", n)
	}

	f, ok := idx.Face(res[0].FaceID)
	if !ok {
		t.Fatalf("face %d not found", res[0].FaceID)
	}
	if f.ClusterID.Valid {
		t.Errorf("stored cluster_id = %d, want NULL", f.ClusterID.Int64)
	}
}

// TestNewClusterPersonID pins the named-at-creation op: the new cluster carries
// NewClusterPersonID, and the face written with it resolves to that person.
func TestNewClusterPersonID(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	a := mustCreatePerson(t, idx, "A")
	path := "/lib/named-cluster.jpg"
	emb := vec(0, 1)

	res, err := idx.WriteFacesForPath(path, 1, []FaceOp{{
		Face: Face{
			Path: path, ThumbMtime: 1, Embedding: emb,
			NBox: [4]float64{0.2, 0.2, 0.2, 0.2}, Quality: 0.5,
		},
		NewClusterCentroid: emb,
		NewClusterPersonID: a,
	}})
	if err != nil {
		t.Fatalf("WriteFacesForPath(%q): %v", path, err)
	}
	if len(res) != 1 {
		t.Fatalf("WriteFacesForPath(%q) returned %d results, want 1", path, len(res))
	}

	clusters := idx.AllClusters()
	if len(clusters) != 1 {
		t.Fatalf("AllClusters() = %d clusters, want 1", len(clusters))
	}
	if clusters[0].ID != res[0].ClusterID {
		t.Errorf("cluster id = %d, want the reported %d", clusters[0].ID, res[0].ClusterID)
	}
	if !clusters[0].PersonID.Valid || clusters[0].PersonID.Int64 != a {
		t.Errorf("cluster person_id = %v, want %d", clusters[0].PersonID, a)
	}

	f, ok := idx.Face(res[0].FaceID)
	if !ok {
		t.Fatalf("face %d not found", res[0].FaceID)
	}
	if !f.PersonID.Valid || f.PersonID.Int64 != a {
		t.Errorf("face person_id = %v, want %d", f.PersonID, a)
	}
}

// TestPreMatchTags pins the hint PreMatchTags builds for detections that are
// about to be written: the tag's decision must land on the detection matching
// its box (or embedding), and only there.
func TestPreMatchTags(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	path := "/lib/000000.jpg"
	a := mustCreatePerson(t, idx, "A")
	b := mustCreatePerson(t, idx, "B")

	box := [4]float64{0.2, 0.2, 0.2, 0.2}
	emb := vec(0, 1)
	face, _ := writeOneFace(t, idx, path, emb, box)
	mustTagFaces(t, idx, []int64{face}, VerdictConfirmed, a)

	// Detection 0 is the tagged face, detection 1 a different face: no box
	// overlap and an embedding far past TagMaxSqDistance.
	boxes := [][4]float64{box, {0.7, 0.7, 0.1, 0.1}}
	embs := [][]float32{emb, vec(1, 1)}

	hints, err := idx.PreMatchTags(path, boxes, embs)
	if err != nil {
		t.Fatalf("PreMatchTags: %v", err)
	}
	if len(hints) != 2 {
		t.Fatalf("PreMatchTags returned %d hints, want 2", len(hints))
	}
	if hints[0].Confirmed != a {
		t.Errorf("hints[0].Confirmed = %d, want %d", hints[0].Confirmed, a)
	}
	if hints[0].Ignored || len(hints[0].Rejected) != 0 {
		t.Errorf("hints[0] = %+v, want only a confirmation", hints[0])
	}
	if hints[1].Confirmed != 0 || hints[1].Ignored || len(hints[1].Rejected) != 0 {
		t.Errorf("hints[1] = %+v, want a zero hint for the unmatched detection", hints[1])
	}

	// A rejection of B attaches to the same detection; the confirmation stays.
	mustTagFaces(t, idx, []int64{face}, VerdictRejected, b)
	hints, err = idx.PreMatchTags(path, boxes, embs)
	if err != nil {
		t.Fatalf("PreMatchTags after rejection: %v", err)
	}
	if hints[0].Confirmed != a {
		t.Errorf("hints[0].Confirmed = %d after a rejection of B, want %d", hints[0].Confirmed, a)
	}
	if !containsID(hints[0].Rejected, b) {
		t.Errorf("hints[0].Rejected = %v, want it to contain %d", hints[0].Rejected, b)
	}

	// "not a face" replaces the confirmed decision.
	mustTagFaces(t, idx, []int64{face}, VerdictIgnored, 0)
	hints, err = idx.PreMatchTags(path, boxes, embs)
	if err != nil {
		t.Fatalf("PreMatchTags after ignore: %v", err)
	}
	if !hints[0].Ignored {
		t.Errorf("hints[0].Ignored = false, want true after an ignore tag")
	}
	if hints[0].Confirmed != 0 {
		t.Errorf("hints[0].Confirmed = %d, want 0 on an ignored face", hints[0].Confirmed)
	}
}

// TestReassignFace drives ReassignFace through every destination: an existing
// cluster (both running means stay exact), a fresh singleton, a fresh cluster
// named after a person, and out of clustering altogether. A cluster is deleted
// only when its last member leaves.
func TestReassignFace(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	box := [4]float64{0.2, 0.2, 0.2, 0.2}

	// c1 with two faces; its stored centroid is their running mean, vec(0,0.9).
	first, err := idx.WriteFacesForPath("/lib/c1-first.jpg", 1, []FaceOp{{
		Face: Face{
			Path: "/lib/c1-first.jpg", ThumbMtime: 1, Embedding: vec(0, 1),
			NBox: box, Quality: 0.5,
		},
		NewClusterCentroid: vec(0, 1),
	}})
	if err != nil {
		t.Fatalf("seed c1 first face: %v", err)
	}
	c1 := first[0].ClusterID
	second, err := idx.WriteFacesForPath("/lib/c1-second.jpg", 1, []FaceOp{{
		Face: Face{
			Path: "/lib/c1-second.jpg", ThumbMtime: 1, Embedding: vec(0, 0.8),
			NBox: box, Quality: 0.5,
		},
		ExistingClusterID: c1,
		UpdatedCentroid:   vec(0, 0.9),
	}})
	if err != nil {
		t.Fatalf("seed c1 second face: %v", err)
	}
	moving := second[0].FaceID
	assertCentroid(t, storedCentroid(t, idx, c1), vec(0, 0.9))

	// c2 with a single face of a different identity.
	original, c2 := writeOneFace(t, idx, "/lib/c2.jpg", vec(1, 1), box)

	// Move c1's second face into c2: c1 falls back to its remaining member,
	// c2 becomes the mean of its old member and the joiner.
	got, err := idx.ReassignFace(moving, c2, 0)
	if err != nil {
		t.Fatalf("ReassignFace(%d, %d, 0): %v", moving, c2, err)
	}
	if got != c2 {
		t.Errorf("ReassignFace returned cluster %d, want %d", got, c2)
	}
	if f, ok := idx.Face(moving); !ok {
		t.Fatalf("face %d not found", moving)
	} else if !f.ClusterID.Valid || f.ClusterID.Int64 != c2 {
		t.Errorf("face %d cluster_id = %v, want %d", moving, f.ClusterID, c2)
	}
	assertCentroid(t, storedCentroid(t, idx, c1), vec(0, 1))

	mean := append([]float32(nil), vec(1, 1)...)
	joiner := vec(0, 0.8)
	for i := range mean {
		mean[i] = (mean[i] + joiner[i]) / 2
	}
	assertCentroid(t, storedCentroid(t, idx, c2), mean)

	// Moving c2's original face out leaves c2 with one member: it must survive,
	// with exactly that member defining its centroid.
	solo, err := idx.ReassignFace(original, 0, 0)
	if err != nil {
		t.Fatalf("ReassignFace(%d, 0, 0): %v", original, err)
	}
	if solo == 0 || solo == c2 {
		t.Fatalf("ReassignFace(%d, 0, 0) returned cluster %d, want a fresh cluster id", original, solo)
	}
	if f, ok := idx.Face(original); !ok {
		t.Fatalf("face %d not found", original)
	} else if !f.ClusterID.Valid || f.ClusterID.Int64 != solo {
		t.Errorf("face %d cluster_id = %v, want %d", original, f.ClusterID, solo)
	}
	assertCentroid(t, storedCentroid(t, idx, c2), vec(0, 0.8))
	left, ok := clusterByID(idx, c2)
	if !ok {
		t.Fatalf("cluster %d was deleted while it still had one member", c2)
	}
	if left.Count != 1 {
		t.Errorf("cluster %d count = %d, want 1", c2, left.Count)
	}

	// Moving that last member into a fresh cluster named A empties c2, which
	// must be deleted; the new cluster is named and the face follows it.
	a := mustCreatePerson(t, idx, "A")
	named, err := idx.ReassignFace(moving, 0, a)
	if err != nil {
		t.Fatalf("ReassignFace(%d, 0, %d): %v", moving, a, err)
	}
	if named == 0 || named == solo {
		t.Fatalf("ReassignFace(%d, 0, %d) returned cluster %d, want a fresh cluster id", moving, a, named)
	}
	nc, ok := clusterByID(idx, named)
	if !ok {
		t.Fatalf("new cluster %d not found", named)
	}
	if !nc.PersonID.Valid || nc.PersonID.Int64 != a {
		t.Errorf("new cluster person_id = %v, want %d", nc.PersonID, a)
	}
	if _, ok := clusterByID(idx, c2); ok {
		t.Errorf("cluster %d still exists after its last member left", c2)
	}
	if f, ok := idx.Face(moving); !ok {
		t.Fatalf("face %d not found", moving)
	} else if !f.PersonID.Valid || f.PersonID.Int64 != a {
		t.Errorf("face %d person_id = %v, want %d", moving, f.PersonID, a)
	}

	// ReassignFace(id, -1, 0) takes the face out of clustering entirely.
	gone, err := idx.ReassignFace(moving, -1, 0)
	if err != nil {
		t.Fatalf("ReassignFace(%d, -1, 0): %v", moving, err)
	}
	if gone != 0 {
		t.Errorf("ReassignFace(%d, -1, 0) returned %d, want 0", moving, gone)
	}
	f, ok := idx.Face(moving)
	if !ok {
		t.Fatalf("face %d not found", moving)
	}
	if f.ClusterID.Valid {
		t.Errorf("face %d cluster_id = %d, want NULL", moving, f.ClusterID.Int64)
	}
	if _, ok := clusterByID(idx, named); ok {
		t.Errorf("cluster %d still exists after its last member left", named)
	}
}
