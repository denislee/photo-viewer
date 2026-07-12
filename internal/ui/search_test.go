package ui

import (
	"path/filepath"
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
