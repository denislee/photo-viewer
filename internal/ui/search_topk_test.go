package ui

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"testing"
)

// topKTestCandidates builds n deterministic pseudo-random candidates: files
// named "dir%03d/IMG_%05d.jpg" with random lowercase letters appended, plus a
// directory candidate every ten entries. The shared "IMG_" stem and the short
// random tail make score ties common, which is what exercises the heap's
// tie-breaking.
func topKTestCandidates(n int) []searchCandidate {
	rng := rand.New(rand.NewSource(20240117))
	out := make([]searchCandidate, 0, n+16)
	for i := 0; i < n; i++ {
		if i%10 == 0 {
			rel := fmt.Sprintf("dir%03d", i/10)
			out = append(out, newSearchCandidate(filepath.Join("/lib", rel), rel, true))
		}
		rel := fmt.Sprintf("dir%03d/IMG_%05d%s.jpg", i%97, i, randLetters(rng, 2))
		out = append(out, newSearchCandidate(filepath.Join("/lib", rel), rel, false))
	}
	return out
}

// randLetters returns k random lowercase ASCII letters.
func randLetters(rng *rand.Rand, k int) string {
	b := make([]byte, k)
	for i := range b {
		b[i] = byte('a' + rng.Intn(26))
	}
	return string(b)
}

// referenceTopSearchResults is the pre-G5-01 algorithm: append every match,
// sort the whole set, then truncate.
func referenceTopSearchResults(ql string, candidates []searchCandidate, k int) ([]searchResult, int) {
	all := make([]searchResult, 0, len(candidates))
	for i, c := range candidates {
		all = appendFuzzyResult(all, ql, i, c)
	}
	sortSearchResults(all, candidates)
	matched := len(all)
	if len(all) > k {
		all = all[:k]
	}
	return all, matched
}

// TestTopSearchResultsMatchesSortThenTruncate is the G5-01 equivalence guard:
// the bounded top-K selection must return exactly what the old
// append-everything / sort-everything / truncate did, in the same order, and
// report the same total match count.
func TestTopSearchResultsMatchesSortThenTruncate(t *testing.T) {
	candidates := topKTestCandidates(3000)
	queries := []string{"i", "img", "g_1", "2", "dir0", "zz", "a"}
	ks := []int{1, 10, 500, 5000}

	for _, q := range queries {
		want, wantMatched := referenceTopSearchResults(q, candidates, len(candidates))
		for _, k := range ks {
			got, matched := topSearchResults(q, candidates, k)

			if matched != wantMatched {
				t.Fatalf("query %q k=%d: matched = %d, want %d", q, k, matched, wantMatched)
			}
			if matched != len(want) {
				t.Fatalf("query %q k=%d: matched = %d but reference found %d matches", q, k, matched, len(want))
			}

			truncated := want
			if len(truncated) > k {
				truncated = truncated[:k]
			}
			if len(got) != len(truncated) {
				t.Fatalf("query %q k=%d: got %d results, want %d", q, k, len(got), len(truncated))
			}
			if !reflect.DeepEqual(got, truncated) {
				for i := range got {
					if got[i] != truncated[i] {
						t.Fatalf("query %q k=%d: result %d = %+v, want %+v (rel %q vs %q)",
							q, k, i, got[i], truncated[i],
							candidates[got[i].idx].rel, candidates[truncated[i].idx].rel)
					}
				}
				t.Fatalf("query %q k=%d: results differ (len %d vs %d)", q, k, len(got), len(truncated))
			}
		}
	}
}

// TestTopSearchResultsNonPositiveK covers the degenerate k: nothing is kept but
// every match is still counted.
func TestTopSearchResultsNonPositiveK(t *testing.T) {
	candidates := topKTestCandidates(200)
	_, wantMatched := referenceTopSearchResults("i", candidates, len(candidates))
	for _, k := range []int{0, -1} {
		got, matched := topSearchResults("i", candidates, k)
		if len(got) != 0 {
			t.Fatalf("k=%d: got %d results, want 0", k, len(got))
		}
		if matched != wantMatched {
			t.Fatalf("k=%d: matched = %d, want %d", k, matched, wantMatched)
		}
	}
}

// BenchmarkTopSearchResults measures the bounded full scan on a library-sized
// candidate set with a broad query.
func BenchmarkTopSearchResults(b *testing.B) {
	candidates := topKTestCandidates(100000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		topSearchResults("i", candidates, searchResultCap)
	}
}
