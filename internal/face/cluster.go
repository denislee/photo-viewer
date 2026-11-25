package face

import (
	"math"

	"github.com/dns/photo-viewer/internal/cache"
)

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

// NearestCluster returns the index of the cluster whose centroid is closest
// to v under squared-Euclidean distance, or -1 when none is within
// MaxClusterSqDistance (the caller then starts a new cluster). See
// MaxClusterSqDistance for why the metric is squared-Euclidean, not cosine.
func NearestCluster(v []float32, clusters []cache.Cluster) int {
	if len(v) == 0 {
		return -1
	}
	best, bestDist := -1, float64(MaxClusterSqDistance)
	for i := range clusters {
		if d := SquaredEuclidean(v, clusters[i].Centroid); d < bestDist {
			best, bestDist = i, d
		}
	}
	return best
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

// People-aware assignment (plan §3.5, items 1–3).

// MinClusterQuality is the helper quality score below which a face is stored
// but not clustered: tiny or blurry crops give noisy embeddings, and one
// noisy early face is enough to drag a running-mean centroid toward someone
// else ("chaining"). A zero quality means "not reported" (older helper) and
// is clustered as before. Deliberately low until the W1 eval set says more.
const MinClusterQuality = 0.05

// ExemplarMargin is how much closer (squared distance) a face must be to
// person P's nearest confirmed face than to any other person's before the
// exemplar step sends it to P.
const ExemplarMargin = 0.04

// Exemplar is a confirmed face of a person, used as an anchor.
type Exemplar = cache.Exemplar

// Decision is where one detection goes.
type Decision struct {
	// Cluster is the index into the clusters slice to join, or -1.
	Cluster int
	// New is true when a new cluster should be created (Cluster == -1).
	// Cluster == -1 && !New means "store unclustered".
	New bool
	// Person names the new cluster (New only), 0 for unnamed.
	Person int64
}

// Assigner holds the knobs and anchors for one assignment pass.
type Assigner struct {
	// MaxSq is the squared same-person threshold (strictness); 0 means
	// MaxClusterSqDistance.
	MaxSq     float64
	Exemplars []Exemplar
}

func (a *Assigner) maxSq() float64 {
	if a == nil || a.MaxSq <= 0 {
		return MaxClusterSqDistance
	}
	return a.MaxSq
}

// Decide places one detection with embedding v and helper quality q, given
// what the user's tags already say about it (hint) and the current clusters.
// Rules, in order:
//
//  1. ignored, or quality under MinClusterQuality → stored unclustered;
//  2. confirmed as P → P's nearest cluster, whatever the distance (or a new
//     cluster named P);
//  3. nearest confirmed exemplar is P within the threshold and beats every
//     other person by ExemplarMargin → P's nearest cluster (or a new one);
//  4. nearest centroid within the threshold, skipping clusters of people the
//     face rejected;
//  5. otherwise a new unnamed cluster.
func (a *Assigner) Decide(v []float32, q float64, hint cache.TagHint, clusters []cache.Cluster) Decision {
	if len(v) == 0 || hint.Ignored || (q > 0 && q < MinClusterQuality) {
		return Decision{Cluster: -1}
	}
	toPerson := func(p int64) Decision {
		best, bestD := -1, math.Inf(1)
		for k := range clusters {
			if clusters[k].PersonID.Valid && clusters[k].PersonID.Int64 == p {
				if d := SquaredEuclidean(v, clusters[k].Centroid); d < bestD {
					best, bestD = k, d
				}
			}
		}
		if best >= 0 {
			return Decision{Cluster: best}
		}
		return Decision{Cluster: -1, New: true, Person: p}
	}
	if hint.Confirmed > 0 {
		return toPerson(hint.Confirmed)
	}
	maxSq := a.maxSq()
	if a != nil && len(a.Exemplars) > 0 {
		nearest := map[int64]float64{}
		for _, e := range a.Exemplars {
			d := SquaredEuclidean(v, e.Embedding)
			if cur, ok := nearest[e.PersonID]; !ok || d < cur {
				nearest[e.PersonID] = d
			}
		}
		bestP, bestD, otherD := int64(0), math.Inf(1), math.Inf(1)
		for p, d := range nearest {
			if rejected(hint, p) {
				continue
			}
			switch {
			case d < bestD || (d == bestD && p < bestP):
				otherD = math.Min(otherD, bestD)
				bestP, bestD = p, d
			case d < otherD:
				otherD = d
			}
		}
		// A rejected person still counts as "another person" for the margin.
		for p, d := range nearest {
			if p != bestP && rejected(hint, p) {
				otherD = math.Min(otherD, d)
			}
		}
		if bestP > 0 && bestD < maxSq && bestD+ExemplarMargin <= otherD {
			return toPerson(bestP)
		}
	}
	best, bestD := -1, maxSq
	for k := range clusters {
		if clusters[k].PersonID.Valid && rejected(hint, clusters[k].PersonID.Int64) {
			continue
		}
		if d := SquaredEuclidean(v, clusters[k].Centroid); d < bestD {
			best, bestD = k, d
		}
	}
	if best >= 0 {
		return Decision{Cluster: best}
	}
	return Decision{Cluster: -1, New: true}
}

func rejected(h cache.TagHint, p int64) bool {
	for _, r := range h.Rejected {
		if r == p {
			return true
		}
	}
	return false
}
