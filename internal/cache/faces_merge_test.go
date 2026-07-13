package cache

import (
	"math"
	"testing"
)

// TestMergeCentroids pins the count-weighted-mean math that MergeClusters relies
// on to fold one cluster's centroid into another without re-reading every
// embedding: out = (a*na + b*nb) / (na + nb), plus the degenerate guards.
func TestMergeCentroids(t *testing.T) {
	const tol = 1e-6
	tests := []struct {
		name string
		a    []float32
		na   int
		b    []float32
		nb   int
		want []float32
	}{
		{
			name: "equal weights average",
			a:    []float32{2, 4}, na: 1,
			b: []float32{4, 8}, nb: 1,
			want: []float32{3, 6},
		},
		{
			name: "weighted toward heavier cluster",
			a:    []float32{0, 10}, na: 1,
			b: []float32{10, 0}, nb: 3,
			want: []float32{7.5, 2.5},
		},
		{
			name: "N vs M weighting",
			a:    []float32{1, 1, 1}, na: 2,
			b: []float32{4, 4, 4}, nb: 6,
			want: []float32{3.25, 3.25, 3.25},
		},
		{
			name: "order of weights matters (heavier a)",
			a:    []float32{8, 8}, na: 3,
			b: []float32{0, 0}, nb: 1,
			want: []float32{6, 6},
		},
		{
			name: "empty a returns b (identity seed)",
			a:    nil, na: 0,
			b: []float32{1, 2, 3}, nb: 5,
			want: []float32{1, 2, 3},
		},
		{
			name: "length mismatch returns a unchanged",
			a:    []float32{1, 2}, na: 1,
			b: []float32{1, 2, 3}, nb: 1,
			want: []float32{1, 2},
		},
		{
			name: "zero total weight returns a unchanged",
			a:    []float32{1, 2}, na: 0,
			b: []float32{3, 4}, nb: 0,
			want: []float32{1, 2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mergeCentroids(tc.a, tc.na, tc.b, tc.nb)
			if len(got) != len(tc.want) {
				t.Fatalf("len(got) = %d, want %d", len(got), len(tc.want))
			}
			for i := range tc.want {
				if math.Abs(float64(got[i]-tc.want[i])) > tol {
					t.Errorf("got[%d] = %v, want %v (tol %v)", i, got[i], tc.want[i], tol)
				}
			}
		})
	}
}

// TestMergeCentroidsWeightedTowardMean is a sanity property: the merged centroid
// of two equal-length clusters lies between the two inputs on every dimension,
// and lands nearer the heavier cluster.
func TestMergeCentroidsWeightedTowardMean(t *testing.T) {
	a := []float32{0, 0, 0}
	b := []float32{1, 1, 1}
	// a carries 9x the weight of b, so the merge should sit close to a.
	got := mergeCentroids(a, 9, b, 1)
	for i := range got {
		if got[i] <= 0 || got[i] >= 1 {
			t.Errorf("dim %d = %v, want strictly between 0 and 1", i, got[i])
		}
		if got[i] >= 0.5 {
			t.Errorf("dim %d = %v, want nearer the heavier cluster (< 0.5)", i, got[i])
		}
		if math.Abs(float64(got[i]-0.1)) > 1e-6 {
			t.Errorf("dim %d = %v, want 0.1 (9*0+1*1)/10", i, got[i])
		}
	}
}
