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

// normalize returns v / ||v||. The result has length 1; cosine distance
// against another unit vector reduces to a dot product, dropping the two
// sqrts and divisions from the hot inner loop.
func normalize(v []float32) []float32 {
	if len(v) == 0 {
		return nil
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return nil
	}
	inv := 1 / math.Sqrt(sum)
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) * inv)
	}
	return out
}

// dotUnit returns the dot product of two equal-length vectors. Caller is
// responsible for normalization; this is the inner loop of nearest-cluster
// search so it deliberately avoids any branches or sqrt.
func dotUnit(a, b []float32) float64 {
	var d float64
	for i := range a {
		d += float64(a[i]) * float64(b[i])
	}
	return d
}

// CentroidCandidate is the slice of (id, vector) pairs the cluster step
// searches against. The caller fills it from the index. Norm is the cached
// unit-length form of Centroid — populated by NearestCluster on first use so
// repeated searches against the same in-memory cluster cache don't re-pay
// the normalization cost.
type CentroidCandidate struct {
	ID       int64
	Centroid []float32
	Count    int // running face count, used to update the centroid mean
	Norm     []float32
}

// NearestCluster picks the cluster whose centroid is closest to v. Returns
// (-1, distance) if no candidates are within MaxClusterDistance. The
// candidates slice is mutated in place: Norm is filled the first time a
// candidate is seen and reused on subsequent calls.
func NearestCluster(v []float32, candidates []CentroidCandidate) (int, float64) {
	vn := normalize(v)
	if vn == nil {
		return -1, 1
	}
	bestIdx, bestDist := -1, MaxClusterDistance
	for i := range candidates {
		c := &candidates[i]
		if c.Norm == nil || len(c.Norm) != len(c.Centroid) {
			c.Norm = normalize(c.Centroid)
			if c.Norm == nil {
				continue
			}
		}
		if len(c.Norm) != len(vn) {
			continue
		}
		d := 1 - dotUnit(vn, c.Norm)
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
