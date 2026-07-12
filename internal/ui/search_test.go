package ui

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// newSearchIndex opens a fresh SQLite index in a temp dir seeded with n photo
// entries under root, returning the index and root. Cleanup is registered via
// t.Cleanup so callers don't have to.
func newSearchIndex(t *testing.T, n int) (*cache.Index, string) {
	t.Helper()
	dir := t.TempDir()
	idx, err := cache.Load(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	t.Cleanup(func() { idx.Close() })

	root := filepath.Join(dir, "lib")
	seedSearchIndex(t, idx, root, 0, n)
	return idx, root
}

// seedSearchIndex upserts n photo entries named <start>..<start+n-1> under root.
func seedSearchIndex(t *testing.T, idx *cache.Index, root string, start, n int) {
	t.Helper()
	results := make([]scan.Result, 0, n)
	for i := start; i < start+n; i++ {
		results = append(results, scan.Result{
			Path:    filepath.Join(root, string(rune('a'+i%26))+".jpg"),
			Type:    scan.TypePhoto,
			Size:    int64(1000 + i),
			ModTime: time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC),
		})
	}
	idx.ReconcileBatch(results)
}

// TestFuzzySearchMemoizesCandidates is the G-04 guard: a second Show within the
// TTL, with the index unchanged, must NOT rebuild the candidate list. We prove
// "no rebuild / no re-query" three ways — the readiness flag stays true, the
// build timestamp is unchanged, and the candidate slice keeps the exact same
// backing array (a rebuild would allocate a fresh one via make()).
func TestFuzzySearchMemoizesCandidates(t *testing.T) {
	idx, root := newSearchIndex(t, 8)
	v := NewFuzzySearchView(idx)

	// Prime the cache synchronously (the same work Show's goroutine does).
	v.rebuildCandidates(root)
	if !v.candidatesReady {
		t.Fatal("candidates not ready after initial build")
	}
	if len(v.candidates) == 0 {
		t.Fatal("expected candidates from a seeded index")
	}

	built0 := v.builtAt
	slice0 := v.candidates
	first0 := &v.candidates[0]

	if !v.candidatesFresh() {
		t.Fatal("candidatesFresh = false immediately after a build, want true")
	}

	// A second open with nothing changed must reuse the memoized list.
	v.Show(root)

	if !v.candidatesReady {
		t.Fatal("Show cleared candidatesReady on a fresh cache (unexpected rebuild)")
	}
	if !v.builtAt.Equal(built0) {
		t.Errorf("builtAt changed on a memoized Show: %v -> %v (rebuild happened)", built0, v.builtAt)
	}
	if len(v.candidates) == 0 || &v.candidates[0] != first0 {
		t.Error("candidate slice was reallocated on a memoized Show (rebuild happened)")
	}
	if &v.candidates[0] != &slice0[0] {
		t.Error("candidate backing array changed on a memoized Show")
	}
}

// TestFuzzySearchRebuildsAfterClear checks the generation signal: Rebuild/Clear
// bumps Index.Generation(), which must invalidate the memoized candidates even
// though the row count coincidentally matches (both are compared).
func TestFuzzySearchRebuildsAfterClear(t *testing.T) {
	idx, root := newSearchIndex(t, 6)
	v := NewFuzzySearchView(idx)

	v.rebuildCandidates(root)
	if !v.candidatesFresh() {
		t.Fatal("expected fresh cache after build")
	}

	if err := idx.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if v.candidatesFresh() {
		t.Error("candidatesFresh = true after Clear bumped the generation, want false")
	}
}

// TestFuzzySearchRebuildsOnRowCountChange checks the incremental signal: adding
// entries without bumping the generation still invalidates the cache via the
// row-count probe. countValid is false right after a build, so the first probe
// re-counts and sees the change immediately (no searchCountTTL blindness).
func TestFuzzySearchRebuildsOnRowCountChange(t *testing.T) {
	idx, root := newSearchIndex(t, 4)
	v := NewFuzzySearchView(idx)

	v.rebuildCandidates(root)
	if !v.candidatesReady {
		t.Fatal("expected a completed build")
	}

	// Add rows the way an incremental SelectDir scan would — no generation bump.
	// Note: we must not probe candidatesFresh/rowCount before this, or the
	// searchCountTTL cache would (by design) coalesce away the change for a few
	// seconds. countValid is false straight after a build, so the probe below
	// re-counts and sees the new rows immediately.
	seedSearchIndex(t, idx, root, 100, 3)

	if v.candidatesFresh() {
		t.Error("candidatesFresh = true after rows were added, want false")
	}
}

// TestFuzzySearchRebuildsAfterTTL checks the age backstop: even with an
// unchanged generation and row count, a build older than searchCacheTTL is
// considered stale so an in-place edit that leaves the count identical can't
// pin a permanently stale list.
func TestFuzzySearchRebuildsAfterTTL(t *testing.T) {
	idx, root := newSearchIndex(t, 5)
	v := NewFuzzySearchView(idx)

	v.rebuildCandidates(root)
	if !v.candidatesFresh() {
		t.Fatal("expected fresh cache after build")
	}

	// Backdate the build past the TTL; generation and row count are untouched.
	v.mu.Lock()
	v.builtAt = time.Now().Add(-2 * searchCacheTTL)
	v.mu.Unlock()

	if v.candidatesFresh() {
		t.Error("candidatesFresh = true for a build older than searchCacheTTL, want false")
	}
}

// --- G-05: incremental narrowing on query extension --------------------------

// mkCand builds a searchCandidate the way rebuildCandidates would, so tests can
// assemble a fixture list without touching SQLite.
func mkCand(root, rel string, isDir bool) searchCandidate {
	base := filepath.Base(rel)
	return searchCandidate{
		path:      filepath.Join(root, rel),
		rel:       rel,
		base:      base,
		isDir:     isDir,
		baseLower: strings.ToLower(base),
		relLower:  strings.ToLower(rel),
	}
}

// setCandidates installs a fixed candidate slice as if the background build had
// completed, bypassing the index entirely.
func setCandidates(v *FuzzySearchView, cands []searchCandidate) {
	v.mu.Lock()
	v.candidates = cands
	v.candidatesReady = true
	v.mu.Unlock()
}

// typeQuery drives one recompute for query q through the real Layout entry
// point (editor text → recomputeResults), exercising whichever scan path the
// guards select.
func typeQuery(v *FuzzySearchView, q string) {
	v.editor.SetText(q)
	v.recomputeResults()
}

type resultKey struct {
	rel   string
	score int
}

// orderedKeys projects v.results to (rel, score) in result order — captures both
// membership and the exact ordering for comparison across scan paths.
func orderedKeys(v *FuzzySearchView) []resultKey {
	keys := make([]resultKey, 0, len(v.results))
	for _, r := range v.results {
		keys = append(keys, resultKey{rel: v.activeCandidates[r.idx].rel, score: r.score})
	}
	return keys
}

// relSet collects the matched relative paths, order-independent.
func relSet(v *FuzzySearchView) map[string]bool {
	m := make(map[string]bool, len(v.results))
	for _, r := range v.results {
		m[v.activeCandidates[r.idx].rel] = true
	}
	return m
}

func incrementalFixture(root string) []searchCandidate {
	rels := []struct {
		rel   string
		isDir bool
	}{
		{"apple", true},
		{"apple/pie.jpg", false},
		{"apple/pattern.png", false},
		{"apple/apricot.jpg", false},
		{"banana", true},
		{"banana/apricot.jpg", false},
		{"banana/bread.jpg", false},
		{"apricot", true},
		{"apricot/banana.jpg", false},
		{"photos/2024", true},
		{"photos/2024/app_store.png", false},
		{"photos/2024/beach.jpg", false},
		{"photos/2024/backup.jpg", false},
		{"misc/ab.jpg", false},
		{"misc/abc.jpg", false},
		{"misc/acb.jpg", false},
		{"misc/bca.jpg", false},
		{"zed/aabbcc.jpg", false},
		{"zed/babble.png", false},
		{"random/cabbage.jpg", false},
		{"random/scrapbook.jpg", false},
		{"deep/nested/path/to/album.jpg", false},
	}
	cands := make([]searchCandidate, 0, len(rels))
	for _, r := range rels {
		cands = append(cands, mkCand(root, r.rel, r.isDir))
	}
	return cands
}

// TestFuzzySearchIncrementalMatchesFullScan is the G-05 property test: feeding a
// query one character at a time (so every step after the first goes through the
// incremental-narrowing path) must, at *every* prefix, produce exactly the same
// ordered result set as a single from-scratch recomputeResults on that prefix.
func TestFuzzySearchIncrementalMatchesFullScan(t *testing.T) {
	root := "/lib"
	cands := incrementalFixture(root)

	finals := []string{"a", "ap", "app", "apr", "ban", "ab", "abc", "photo", "2024", "b", "bc", "bread", "cab", "aab"}
	for _, final := range finals {
		// One view typed up incrementally across the whole run of prefixes.
		inc := NewFuzzySearchView(nil)
		setCandidates(inc, cands)
		for i := 1; i <= len(final); i++ {
			prefix := final[:i]
			typeQuery(inc, prefix)

			// A fresh view scanning this prefix from scratch (lastQuery == ""
			// forces the full-scan path).
			full := NewFuzzySearchView(nil)
			setCandidates(full, cands)
			typeQuery(full, prefix)

			if inc.resultsTruncated {
				t.Fatalf("fixture unexpectedly truncated at prefix %q; keep it under the cap", prefix)
			}
			got, want := orderedKeys(inc), orderedKeys(full)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("prefix %q (typing toward %q): incremental result set diverged\n got=%v\nwant=%v", prefix, final, got, want)
			}
		}
	}
}

// TestFuzzySearchIncrementalNarrowsMonotonically checks the shape of the
// optimization: as the query grows, the incremental result set can only shrink
// (subsequence matching is monotonic), and each match is always a subset of the
// previous step's matches.
func TestFuzzySearchIncrementalNarrowsMonotonically(t *testing.T) {
	root := "/lib"
	cands := incrementalFixture(root)

	v := NewFuzzySearchView(nil)
	setCandidates(v, cands)

	prev := map[string]bool{}
	first := true
	for i := 1; i <= len("apri"); i++ {
		typeQuery(v, "apri"[:i])
		cur := relSet(v)
		if !first {
			if len(cur) > len(prev) {
				t.Errorf("result set grew on extension to %q: %d > %d", "apri"[:i], len(cur), len(prev))
			}
			for rel := range cur {
				if !prev[rel] {
					t.Errorf("extension to %q introduced %q not present in the shorter query's matches", "apri"[:i], rel)
				}
			}
		}
		prev, first = cur, false
	}
}

// TestFuzzySearchTruncationForcesFullRescan verifies the cap guard: when the
// previous pass was truncated at the 500 result cap, extending the query must
// fall back to a full rescan. An incremental rescan over only the truncated set
// would miss candidates that rank in for the longer query but sat outside the
// earlier top-500 — this test constructs exactly that situation.
func TestFuzzySearchTruncationForcesFullRescan(t *testing.T) {
	root := "/lib"
	var cands []searchCandidate
	// 501 candidates that match "a" (score 0, basename starts with 'a') and
	// sort early by rel, but contain no 'b' so they never match "ab".
	for i := range 501 {
		cands = append(cands, mkCand(root, fmt.Sprintf("a_%03d.jpg", i), false))
	}
	// A handful that DO match "ab" but sort late and score worse for "a"
	// (the 'a' is deep in the name), so they land outside the top-500 for "a".
	late := []string{"zzz_ab.jpg", "zzz_abc.jpg", "zzy_ab.jpg"}
	for _, rel := range late {
		cands = append(cands, mkCand(root, rel, false))
	}

	v := NewFuzzySearchView(nil)
	setCandidates(v, cands)

	typeQuery(v, "a")
	if !v.resultsTruncated {
		t.Fatalf("query %q should truncate at the cap: got %d results, truncated=%v", "a", len(v.results), v.resultsTruncated)
	}

	// Extend to "ab": the cap guard must force a full rescan.
	typeQuery(v, "ab")
	got := relSet(v)

	full := NewFuzzySearchView(nil)
	setCandidates(full, cands)
	typeQuery(full, "ab")
	want := relSet(full)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("truncated extension diverged from full scan:\n got=%v\nwant=%v", got, want)
	}
	if len(got) != len(late) {
		t.Fatalf("expected the %d late %q matches after fallback, got %d (%v) — incremental path missed them", len(late), "ab", len(got), got)
	}
}

// TestFuzzySearchRowCountProbeCoalesces verifies the SELECT COUNT(*) probe is
// TTL-cached: after one probe populates the cache, a table mutation within
// searchCountTTL is not observed by rowCount — so a burst of opens runs at most
// one count query.
func TestFuzzySearchRowCountProbeCoalesces(t *testing.T) {
	idx, root := newSearchIndex(t, 7)
	v := NewFuzzySearchView(idx)

	if got := v.rowCount(); got != 7 {
		t.Fatalf("rowCount = %d, want 7", got)
	}
	// Grow the table, then probe again inside the TTL window: the cached count
	// must be returned, proving the second call didn't re-run COUNT(*).
	seedSearchIndex(t, idx, root, 100, 3)
	if got := v.rowCount(); got != 7 {
		t.Errorf("rowCount re-queried within TTL: got %d, want cached 7", got)
	}
}
