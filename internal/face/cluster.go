package face

import "math"

// MaxClusterSqDistance is the squared-Euclidean distance above which a new face
// is treated as a new identity rather than merged into an existing cluster.
//
// dlib's canonical same-person threshold for its 128-d face embedding is a
// *Euclidean* distance of 0.6 (this is what face_recognition.compare_faces uses
// as its default tolerance). We compare *squared* distances so the hot inner
// loop can skip a sqrt, hence 0.6² = 0.36. The important correctness point is
// the metric: the previous code applied 0.6 to *cosine* distance instead. For
// these near-unit dlib vectors a cosine distance of 0.6 corresponds to a
// Euclidean distance of ~1.1, so the greedy clusterer chained clearly-different
// people into one cluster and the running-mean centroid updates then
// accelerated the merge cascade. Comparing squared-Euclidean against 0.36
// restores dlib's calibrated semantics.
const MaxClusterSqDistance = 0.36

// SquaredEuclidean returns the squared L2 distance between a and b. It is the
// square (not the sqrt) because the clusterer only ever compares the result
// against a squared threshold, so taking the root would be wasted work in the
// nearest-cluster inner loop. Returns +Inf when the vectors are empty or their
// dimensions disagree, so such a pair can never win the nearest search.
func SquaredEuclidean(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return math.Inf(1)
	}
	var sum float64
	for i := range a {
		d := float64(a[i]) - float64(b[i])
		sum += d * d
	}
	return sum
}

// CentroidCandidate is one (id, centroid) pair the cluster step searches
// against. The caller fills it from the index. Count is the running face count,
// used to update the centroid mean when a face is added to the cluster.
type CentroidCandidate struct {
	ID       int64
	Centroid []float32
	Count    int
}

// NearestCluster picks the cluster whose centroid is closest to v under
// squared-Euclidean distance. Returns (-1, MaxClusterSqDistance) when no
// candidate is within MaxClusterSqDistance, which the caller reads as "start a
// new cluster". The distance is squared-Euclidean throughout — see
// MaxClusterSqDistance for why that (not cosine) is the correct metric.
func NearestCluster(v []float32, candidates []CentroidCandidate) (int, float64) {
	if len(v) == 0 {
		return -1, math.Inf(1)
	}
	bestIdx, bestDist := -1, float64(MaxClusterSqDistance)
	for i := range candidates {
		d := SquaredEuclidean(v, candidates[i].Centroid)
		if d < bestDist {
			bestIdx = i
			bestDist = d
		}
	}
	return bestIdx, bestDist
}

// UpdatedCentroid returns the running mean when adding `v` to a cluster of
// size `count` whose current centroid is `centroid`.
func UpdatedCentroid(centroid, v []float32, count int) []float32 {
	if count <= 0 || len(centroid) != len(v) {
		out := make([]float32, len(v))
		copy(out, v)
		return out
	}
	out := make([]float32, len(centroid))
	n := float32(count)
	for i := range centroid {
		out[i] = (centroid[i]*n + v[i]) / (n + 1)
	}
	return out
}
