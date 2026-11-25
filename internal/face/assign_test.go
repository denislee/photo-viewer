package face

import (
	"database/sql"
	"math"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
)

// axisVal returns a 128-d vector with v on component axis and zeros elsewhere.
// Distances add across components, so a vector on another axis is far from
// every axisVec value even when its own value is small.
func axisVal(axis int, v float32) []float32 {
	out := make([]float32, 128)
	out[axis] = v
	return out
}

// axisAtSq places a vector on component 0 so its squared distance from
// axisVec(0) is d. It lets a case state the distance the rule is about (0.3,
// 0.10, …) instead of a raw component value.
func axisAtSq(d float64) []float32 {
	return axisVec(float32(math.Sqrt(d)))
}

// axisAtSqFrom is axisAtSq measured from axisVec(from): the squared distance
// between the returned vector and axisVec(from) is d.
func axisAtSqFrom(from float32, d float64) []float32 {
	return axisVec(from + float32(math.Sqrt(d)))
}

// personCluster builds a cluster around centroid; person > 0 gives it that
// identity, 0 leaves it unnamed (person_id NULL).
func personCluster(person int64, centroid []float32) cache.Cluster {
	c := cache.Cluster{Centroid: centroid, Count: 1}
	if person > 0 {
		c.PersonID = sql.NullInt64{Int64: person, Valid: true}
	}
	return c
}

// TestAssignerDecide pins every rule in Decide against clusters and exemplars
// placed at known squared distances: an axis-aligned vector pair at (0,a) and
// (0,b) is exactly (a-b)² apart, and a vector on another axis adds its own
// squared component on top.
func TestAssignerDecide(t *testing.T) {
	const (
		a int64 = 1
		b int64 = 2
		p int64 = 3
		q int64 = 4
	)

	origin := axisVec(0)
	// A face 0.1 from the origin: squared distance 0.01.
	near := axisVec(0.1)
	// Squared distance 2 from the origin, on a different axis, so it stays far
	// even from `near`.
	far := axisVal(1, float32(math.Sqrt(2)))

	// Fixture geometry, asserted so a drifting helper cannot quietly weaken a
	// case: every distance the rules below are calibrated against.
	for _, fx := range []struct {
		name string
		from []float32
		to   []float32
		sq   float64
	}{
		{"near→origin", near, origin, 0.01},
		{"origin→far", origin, far, 2},
		{"near→person B's cluster", near, axisAtSqFrom(0.1, 0.2), 0.2},
		{"near→person P's cluster", near, axisVec(1.1), 1.0},
		{"origin→P's exemplar", origin, axisAtSq(0.10), 0.10},
		{"origin→Q's exemplar", origin, axisAtSq(0.12), 0.12},
		{"origin→0.3 cluster", origin, axisAtSq(0.3), 0.3},
	} {
		if d := SquaredEuclidean(fx.from, fx.to); math.Abs(d-fx.sq) > 1e-4 {
			t.Fatalf("fixture %s: squared distance = %v, want %v", fx.name, d, fx.sq)
		}
	}

	tests := []struct {
		name     string
		v        []float32
		quality  float64
		hint     cache.TagHint
		clusters []cache.Cluster
		assigner Assigner
		want     Decision
	}{
		{
			name:     "ignored hint is stored unclustered",
			v:        near,
			quality:  0.9,
			hint:     cache.TagHint{Ignored: true},
			clusters: []cache.Cluster{personCluster(0, origin)},
			want:     Decision{Cluster: -1},
		},
		{
			// 0.01 is under MinClusterQuality (0.05): stored, but never
			// clustered, and not a "new cluster" either.
			name:     "quality under MinClusterQuality is stored unclustered",
			v:        near,
			quality:  0.01,
			clusters: []cache.Cluster{personCluster(0, origin)},
			want:     Decision{Cluster: -1},
		},
		{
			// Zero quality means "not reported" (older helper): cluster as before.
			name:     "quality zero clusters normally",
			v:        near,
			quality:  0,
			clusters: []cache.Cluster{personCluster(0, origin)},
			want:     Decision{Cluster: 0},
		},
		{
			// Person P's cluster is squared-distance 2 away, far past MaxSq:
			// a confirmation joins it regardless of distance.
			name:     "confirmed person joins their far cluster",
			v:        origin,
			quality:  0.9,
			hint:     cache.TagHint{Confirmed: p},
			clusters: []cache.Cluster{personCluster(p, far)},
			want:     Decision{Cluster: 0},
		},
		{
			// No cluster of P exists, and the nearby unnamed cluster must not
			// win over the confirmation.
			name:     "confirmed person with no cluster starts one",
			v:        origin,
			quality:  0.9,
			hint:     cache.TagHint{Confirmed: p},
			clusters: []cache.Cluster{personCluster(0, axisVec(0.05))},
			want:     Decision{Cluster: -1, New: true, Person: p},
		},
		{
			// A's centroid is squared-distance 0.01 away (nearest), B's 0.2, both
			// under MaxSq. The rejection must skip A's and join B's.
			name:    "rejected person's nearest cluster is skipped",
			v:       near,
			quality: 0.9,
			hint:    cache.TagHint{Rejected: []int64{a}},
			clusters: []cache.Cluster{
				personCluster(a, origin),
				personCluster(b, axisAtSqFrom(0.1, 0.2)),
			},
			want: Decision{Cluster: 1},
		},
		{
			name:     "rejected person's only cluster starts a new one",
			v:        near,
			quality:  0.9,
			hint:     cache.TagHint{Rejected: []int64{a}},
			clusters: []cache.Cluster{personCluster(a, origin)},
			want:     Decision{Cluster: -1, New: true},
		},
		{
			// The exemplar of P is 0.1 away (sq 0.01) while P's cluster centroid
			// is squared-distance 1.0 away (past MaxSq 0.36): the exemplar step
			// wins and sends the face to P's cluster anyway.
			name:     "confirmed exemplar pulls a face to the person's far cluster",
			v:        near,
			quality:  0.9,
			clusters: []cache.Cluster{personCluster(p, axisVec(1.1))},
			assigner: Assigner{Exemplars: []Exemplar{{PersonID: p, Embedding: origin}}},
			want:     Decision{Cluster: 0},
		},
		{
			// P's exemplar is 0.10 away and Q's 0.12 — inside ExemplarMargin
			// (0.04), so the exemplar step must not fire: the face falls back to
			// the nearest centroid, the unnamed cluster 0.01 away.
			name:     "exemplar inside the margin falls back to nearest centroid",
			v:        origin,
			quality:  0.9,
			clusters: []cache.Cluster{personCluster(0, axisVec(0.1))},
			assigner: Assigner{Exemplars: []Exemplar{
				{PersonID: p, Embedding: axisAtSq(0.10)},
				{PersonID: q, Embedding: axisAtSq(0.12)},
			}},
			want: Decision{Cluster: 0},
		},
		{
			// Squared distance 0.3 is inside the default 0.36…
			name:     "cluster at 0.3 is joined at the default threshold",
			v:        origin,
			quality:  0.9,
			clusters: []cache.Cluster{personCluster(0, axisAtSq(0.3))},
			want:     Decision{Cluster: 0},
		},
		{
			// …but outside a stricter MaxSq of 0.25.
			name:     "cluster at 0.3 is too far at MaxSq 0.25",
			v:        origin,
			quality:  0.9,
			clusters: []cache.Cluster{personCluster(0, axisAtSq(0.3))},
			assigner: Assigner{MaxSq: 0.25},
			want:     Decision{Cluster: -1, New: true},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			as := tc.assigner
			if got := as.Decide(tc.v, tc.quality, tc.hint, tc.clusters); got != tc.want {
				t.Fatalf("Decide = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestAssignerDeterministic guards that Decide is a pure function of its inputs:
// the per-person nearest distances are collected in a map, so repeated calls
// must not change the outcome through map iteration order.
func TestAssignerDeterministic(t *testing.T) {
	// Three people whose exemplars sit at exactly the same distance from the
	// query (sq 0.04). The margin can never fire on a tie, so the decision is
	// the nearest centroid — and must be the same on every run.
	tied := []Exemplar{
		{PersonID: 11, Embedding: axisVec(0.2)},
		{PersonID: 22, Embedding: axisVec(0.2)},
		{PersonID: 33, Embedding: axisVec(0.2)},
	}
	clusters := []cache.Cluster{personCluster(0, axisVec(0.1))}
	as := &Assigner{Exemplars: tied}
	want := Decision{Cluster: 0}
	for i := range 50 {
		if got := as.Decide(axisVec(0), 0.9, cache.TagHint{}, clusters); got != want {
			t.Fatalf("run %d with tied exemplars: Decide = %+v, want %+v", i, got, want)
		}
	}

	// Same when the exemplar step does fire: person 7's exemplar is 0.1 away
	// (best) and the other two 0.45 (sq 0.2025), so the margin holds and the
	// face must land in person 7's cluster — not the nearer unnamed one — on
	// every run.
	picked := []Exemplar{
		{PersonID: 7, Embedding: axisVec(0.1)},
		{PersonID: 8, Embedding: axisVec(0.45)},
		{PersonID: 9, Embedding: axisVec(0.45)},
	}
	clusters = []cache.Cluster{
		personCluster(0, axisVec(0.05)),
		personCluster(7, axisVal(1, 1)),
	}
	as = &Assigner{Exemplars: picked}
	want = Decision{Cluster: 1}
	for i := range 50 {
		if got := as.Decide(axisVec(0), 0.9, cache.TagHint{}, clusters); got != want {
			t.Fatalf("run %d with a clear exemplar winner: Decide = %+v, want %+v", i, got, want)
		}
	}
}
