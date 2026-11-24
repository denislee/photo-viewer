package cache

import (
	"math"
	"testing"
)

// seedTwoFaceCluster writes /lib/a.jpg's face a (new cluster) and /lib/b.jpg's
// face b (joins it with the running mean), returning the cluster id.
func seedTwoFaceCluster(t *testing.T, idx *Index, a, b []float32) int64 {
	t.Helper()
	r, err := idx.WriteFacesForPath("/lib/a.jpg", 1, []FaceOp{{
		Face: Face{Path: "/lib/a.jpg", ThumbMtime: 1, Embedding: a}, NewClusterCentroid: a,
	}})
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}
	id := r[0].ClusterID
	mean := make([]float32, len(a))
	for i := range a {
		mean[i] = (a[i] + b[i]) / 2
	}
	if _, err := idx.WriteFacesForPath("/lib/b.jpg", 1, []FaceOp{{
		Face: Face{Path: "/lib/b.jpg", ThumbMtime: 1, Embedding: b}, ExistingClusterID: id, UpdatedCentroid: mean,
	}}); err != nil {
		t.Fatalf("seed b: %v", err)
	}
	return id
}

func storedCentroid(t *testing.T, idx *Index, id int64) []float32 {
	t.Helper()
	var blob []byte
	if err := idx.db.QueryRow("SELECT centroid FROM face_clusters WHERE id = ?", id).Scan(&blob); err != nil {
		t.Fatalf("select centroid: %v", err)
	}
	return DecodeEmbedding(blob)
}

func assertCentroid(t *testing.T, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("centroid = %v, want %v", got, want)
	}
	for i := range want {
		if math.Abs(float64(got[i]-want[i])) > 1e-6 {
			t.Fatalf("centroid = %v, want %v", got, want)
		}
	}
}

// TestRemovingFacesCorrectsCentroid pins I-04 6c: deleting a path's faces
// (entry delete, batch delete, or a re-detection that rewrites them) takes
// them out of their cluster's running-mean centroid, so the remaining
// members alone define it.
func TestRemovingFacesCorrectsCentroid(t *testing.T) {
	a, b := []float32{1, 0}, []float32{0, 1}
	for name, remove := range map[string]func(*Index) error{
		"RemoveEntry":   func(i *Index) error { return i.RemoveEntry("/lib/a.jpg") },
		"RemoveEntries": func(i *Index) error { return i.RemoveEntries([]string{"/lib/a.jpg"}) },
		"WriteFacesForPath": func(i *Index) error {
			_, err := i.WriteFacesForPath("/lib/a.jpg", 2, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			idx, cleanup := loadEmpty(t)
			defer cleanup()
			id := seedTwoFaceCluster(t, idx, a, b)
			assertCentroid(t, storedCentroid(t, idx, id), []float32{0.5, 0.5})
			if err := remove(idx); err != nil {
				t.Fatalf("remove: %v", err)
			}
			assertCentroid(t, storedCentroid(t, idx, id), b)
		})
	}
}

// TestRemovingLastFaceKeepsCentroid: an emptied cluster keeps its centroid
// (the next face to join replaces it, since its count is 0).
func TestRemovingLastFaceKeepsCentroid(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	id := seedTwoFaceCluster(t, idx, []float32{1, 0}, []float32{0, 1})
	if err := idx.RemoveEntries([]string{"/lib/a.jpg", "/lib/b.jpg"}); err != nil {
		t.Fatal(err)
	}
	assertCentroid(t, storedCentroid(t, idx, id), []float32{0, 1})
}

func TestCentroidWithoutInvertsRunningMean(t *testing.T) {
	assertCentroid(t, CentroidWithout([]float32{0.5, 0.5}, []float32{1, 0}, 2), []float32{0, 1})
	assertCentroid(t, CentroidWithout([]float32{3, 3}, []float32{3, 3}, 1), []float32{3, 3})
}
