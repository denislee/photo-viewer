package face

import (
	"math"
	"testing"
)

// unit128 builds a 128-d embedding whose first component is x and the rest 0.
// Distances between two such vectors are just |x1-x2| along one axis, which
// makes the squared-Euclidean thresholds easy to reason about.
func axisVec(x float32) []float32 {
	v := make([]float32, 128)
	v[0] = x
	return v
}

func TestSquaredEuclidean(t *testing.T) {
	if got := SquaredEuclidean([]float32{0, 0, 0}, []float32{0, 0, 0}); got != 0 {
		t.Fatalf("identical vectors: got %v, want 0", got)
	}
	// (3,4) has L2 norm 5, so squared distance from origin is 25.
	if got := SquaredEuclidean([]float32{0, 0}, []float32{3, 4}); got != 25 {
		t.Fatalf("3-4-5: got %v, want 25", got)
	}
	// Mismatched / empty dims must never win a nearest search.
	if got := SquaredEuclidean(nil, []float32{1}); !math.IsInf(got, 1) {
		t.Fatalf("empty a: got %v, want +Inf", got)
	}
	if got := SquaredEuclidean([]float32{1, 2}, []float32{1}); !math.IsInf(got, 1) {
		t.Fatalf("dim mismatch: got %v, want +Inf", got)
	}
}

func TestNearestClusterMergesNearIdentical(t *testing.T) {
	// A candidate centroid and a query 0.1 away on one axis: squared distance
	// 0.01, well under the 0.36 threshold, so they must merge (index 0).
	cands := []CentroidCandidate{{ID: 7, Centroid: axisVec(0), Count: 1}}
	idx, dist := NearestCluster(axisVec(0.1), cands)
	if idx != 0 {
		t.Fatalf("near-identical vector should merge into cluster 0, got idx=%d", idx)
	}
	if math.Abs(dist-0.01) > 1e-6 {
		t.Fatalf("distance: got %v, want ~0.01", dist)
	}
}

func TestNearestClusterSeparatesDifferent(t *testing.T) {
	// Query 0.7 away on one axis: squared distance 0.49 > 0.36, so it must NOT
	// merge — this is exactly the pair the old cosine metric wrongly chained.
	cands := []CentroidCandidate{{ID: 7, Centroid: axisVec(0), Count: 1}}
	idx, _ := NearestCluster(axisVec(0.7), cands)
	if idx != -1 {
		t.Fatalf("clearly-different vector should start a new cluster, got idx=%d", idx)
	}
}

func TestNearestClusterThresholdBoundary(t *testing.T) {
	// 0.6 apart on one axis => squared distance exactly 0.36 == threshold. The
	// comparison is strictly-less, so the boundary is treated as a new cluster,
	// matching dlib's "distance < tolerance" convention.
	cands := []CentroidCandidate{{ID: 1, Centroid: axisVec(0), Count: 1}}
	if idx, _ := NearestCluster(axisVec(0.6), cands); idx != -1 {
		t.Fatalf("distance == threshold must not merge, got idx=%d", idx)
	}
	// Just inside the threshold (0.59² = 0.3481 < 0.36) must merge.
	if idx, _ := NearestCluster(axisVec(0.59), cands); idx != 0 {
		t.Fatalf("distance just under threshold must merge, got idx=%d", idx)
	}
}

// TestGreedyClusteringKeepsThreePeopleApart drives the same greedy assignment
// the pipeline uses (nearest cluster within threshold, else new cluster) over
// three well-separated groups and asserts exactly three clusters form. Under
// the old cosine/0.6 metric these groups would have chained together.
func TestGreedyClusteringKeepsThreePeopleApart(t *testing.T) {
	// Three identities placed 5 units apart on axis 0; small within-group jitter.
	groups := [][]float32{
		{0.0, 0.05, -0.05},
		{5.0, 5.05, 4.95},
		{10.0, 10.05, 9.95},
	}

	var cands []CentroidCandidate
	assign := func(v []float32) {
		idx, _ := NearestCluster(v, cands)
		if idx >= 0 {
			cands[idx].Centroid = UpdatedCentroid(cands[idx].Centroid, v, cands[idx].Count)
			cands[idx].Count++
			return
		}
		cands = append(cands, CentroidCandidate{Centroid: append([]float32(nil), v...), Count: 1})
	}

	for _, g := range groups {
		for _, x := range g {
			assign(axisVec(x))
		}
	}

	if len(cands) != 3 {
		t.Fatalf("expected 3 clusters for 3 distinct identities, got %d", len(cands))
	}
	for i, c := range cands {
		if c.Count != 3 {
			t.Fatalf("cluster %d: expected 3 faces, got %d", i, c.Count)
		}
	}
}
