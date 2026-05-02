package face

import "math"

// MaxClusterDistance is the cosine distance above which a new face is treated
// as a new identity rather than added to an existing cluster. 0.6 is the
// canonical threshold for the dlib 128-d face embedding used by the helper.
const MaxClusterDistance = 0.6

// CosineDistance returns 1 - cos(a, b). Returns 1 (max distance) if either
// vector has zero norm or if the dimensions disagree.
func CosineDistance(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 1
	}
	var dot, na, nb float64
	for i := range a {
		ai, bi := float64(a[i]), float64(b[i])
		dot += ai * bi
		na += ai * ai
		nb += bi * bi
	}
	if na == 0 || nb == 0 {
		return 1
	}
	return 1 - dot/(math.Sqrt(na)*math.Sqrt(nb))
}

// CentroidCandidate is the slice of (id, vector) pairs the cluster step
// searches against. The caller fills it from the index.
type CentroidCandidate struct {
	ID       int64
	Centroid []float32
	Count    int // running face count, used to update the centroid mean
}

// NearestCluster picks the cluster whose centroid is closest to v. Returns
// (-1, distance) if no candidates are within MaxClusterDistance.
func NearestCluster(v []float32, candidates []CentroidCandidate) (int, float64) {
	bestIdx, bestDist := -1, MaxClusterDistance
	for i, c := range candidates {
		d := CosineDistance(v, c.Centroid)
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
