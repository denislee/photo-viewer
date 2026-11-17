package cache

import (
	"database/sql"
	"reflect"
	"testing"
)

// TestWriteFacesForPathClusterIDs pins the cluster-id contract of
// WriteFacesForPath: every result reports the cluster the face row actually
// carries (an existing cluster is written directly on the INSERT, a new one is
// created and the face pointed at it), the seeded cluster's centroid is the op's
// updated centroid, and a rewrite with zero ops clears the path's rows through
// the cached DELETE statement.
func TestWriteFacesForPathClusterIDs(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	r1, err := idx.WriteFacesForPath("/lib/a.jpg", []FaceOp{{
		Face:               Face{Path: "/lib/a.jpg", ThumbMtime: 1, BBox: [4]int{1, 2, 3, 4}, Embedding: []float32{0.1, 0.2}},
		NewClusterCentroid: []float32{0.1, 0.2},
	}})
	if err != nil {
		t.Fatalf("WriteFacesForPath(/lib/a.jpg): %v", err)
	}
	if len(r1) != 1 {
		t.Fatalf("results for /lib/a.jpg = %d, want 1", len(r1))
	}
	c1 := r1[0].ClusterID
	if c1 == 0 {
		t.Fatalf("new-cluster op reported ClusterID 0, want a real cluster id")
	}

	r2, err := idx.WriteFacesForPath("/lib/b.jpg", []FaceOp{
		{
			Face:              Face{Path: "/lib/b.jpg", ThumbMtime: 2, BBox: [4]int{5, 6, 7, 8}, Embedding: []float32{0.3, 0.4}},
			ExistingClusterID: c1,
			UpdatedCentroid:   []float32{0.3, 0.4},
		},
		{
			Face:               Face{Path: "/lib/b.jpg", ThumbMtime: 2, BBox: [4]int{9, 10, 11, 12}, Embedding: []float32{0.5, 0.6}},
			NewClusterCentroid: []float32{0.5, 0.6},
		},
	})
	if err != nil {
		t.Fatalf("WriteFacesForPath(/lib/b.jpg): %v", err)
	}
	if len(r2) != 2 {
		t.Fatalf("results for /lib/b.jpg = %d, want 2", len(r2))
	}
	if r2[0].ClusterID != c1 {
		t.Errorf("existing-cluster op reported ClusterID %d, want %d", r2[0].ClusterID, c1)
	}
	if r2[1].ClusterID == c1 || r2[1].ClusterID == 0 {
		t.Errorf("new-cluster op reported ClusterID %d, want a fresh non-zero id distinct from %d", r2[1].ClusterID, c1)
	}

	for _, r := range append(append([]FaceOpResult{}, r1...), r2...) {
		var got sql.NullInt64
		if err := idx.db.QueryRow("SELECT cluster_id FROM faces WHERE id = ?", r.FaceID).Scan(&got); err != nil {
			t.Fatalf("select cluster_id for face %d: %v", r.FaceID, err)
		}
		if !got.Valid || got.Int64 != r.ClusterID {
			t.Errorf("face %d stored cluster_id = %v, reported %d", r.FaceID, got, r.ClusterID)
		}
	}

	var sampleFaceID sql.NullInt64
	if err := idx.db.QueryRow(
		"SELECT sample_face_id FROM face_clusters WHERE id = ?", r2[1].ClusterID,
	).Scan(&sampleFaceID); err != nil {
		t.Fatalf("select sample_face_id for cluster %d: %v", r2[1].ClusterID, err)
	}
	if !sampleFaceID.Valid || sampleFaceID.Int64 != r2[1].FaceID {
		t.Errorf("cluster %d sample_face_id = %v, want face %d", r2[1].ClusterID, sampleFaceID, r2[1].FaceID)
	}

	var blob []byte
	if err := idx.db.QueryRow("SELECT centroid FROM face_clusters WHERE id = ?", c1).Scan(&blob); err != nil {
		t.Fatalf("select centroid for cluster %d: %v", c1, err)
	}
	if got, want := DecodeEmbedding(blob), []float32{0.3, 0.4}; !reflect.DeepEqual(got, want) {
		t.Errorf("cluster %d centroid = %v, want %v", c1, got, want)
	}

	// Reusing the cached DELETE through tx.Stmt must clear the path's rows.
	if _, err := idx.WriteFacesForPath("/lib/b.jpg", nil); err != nil {
		t.Fatalf("WriteFacesForPath(/lib/b.jpg, nil): %v", err)
	}
	var n int
	if err := idx.db.QueryRow("SELECT COUNT(*) FROM faces WHERE path = '/lib/b.jpg'").Scan(&n); err != nil {
		t.Fatalf("count faces for /lib/b.jpg: %v", err)
	}
	if n != 0 {
		t.Errorf("faces for /lib/b.jpg after zero-op rewrite = %d, want 0", n)
	}
}
