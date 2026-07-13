package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCloseRunsOptimize is the C-09 guard: Index.Close issues `PRAGMA optimize`
// on the still-open handle before closing it, so the query planner's statistics
// are refreshed from this session's churn instead of the DB running stat-less
// for its whole life. The assertions are:
//
//   - Close returns nil (the optimize call must not fail Close);
//   - re-opening the same file and reading the rows still works (the DB is
//     intact and usable after the optimize + close);
//   - sqlite_stat1 is populated after the cycle — that table only exists once
//     ANALYZE has run, and nothing else in Load/seed runs ANALYZE, so its
//     presence proves `PRAGMA optimize` did its incremental ANALYZE on close.
func TestCloseRunsOptimize(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv-optimize-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "index.db")

	idx, err := Load(dbPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	const want = 1500
	seed(t, idx, "/lib", want)

	// A freshly loaded/seeded DB has never been ANALYZEd, so sqlite_stat1 must
	// not exist yet. This anchors the post-close assertion to Close's optimize.
	if statTableExists(t, idx) {
		t.Fatalf("sqlite_stat1 present before Close: something already ran ANALYZE")
	}

	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Re-open: the DB must be intact and every row still readable.
	idx2, err := Load(dbPath)
	if err != nil {
		t.Fatalf("re-open Load: %v", err)
	}
	defer idx2.Close()
	if got := idx2.Count(); got != want {
		t.Fatalf("after re-open: Count = %d, want %d", got, want)
	}

	// sqlite_stat1 now exists and is populated, proving Close ran PRAGMA
	// optimize (which internally ANALYZEd the changed tables/indexes).
	if !statTableExists(t, idx2) {
		t.Fatalf("sqlite_stat1 missing after Close: PRAGMA optimize did not run")
	}
	var rows int
	if err := idx2.db.QueryRow("SELECT COUNT(*) FROM sqlite_stat1").Scan(&rows); err != nil {
		t.Fatalf("count sqlite_stat1: %v", err)
	}
	if rows == 0 {
		t.Fatalf("sqlite_stat1 empty after Close: PRAGMA optimize collected no stats")
	}
}

// statTableExists reports whether the ANALYZE-produced sqlite_stat1 table is
// present in the schema.
func statTableExists(t *testing.T, idx *Index) bool {
	t.Helper()
	var name string
	err := idx.db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1'",
	).Scan(&name)
	return err == nil && name == "sqlite_stat1"
}
