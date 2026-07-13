package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// hasColumn reports whether table has a column of the given name.
func hasColumn(t *testing.T, idx *Index, table, col string) bool {
	t.Helper()
	rows, err := idx.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, ctyp string
			notNull    int
			dflt       any
			pk         int
		)
		if err := rows.Scan(&cid, &name, &ctyp, &notNull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == col {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}

// TestMigrationV7AddsMediaDate proves the v6 -> v7 migration (U-14): a fresh
// database is created at v7 with the media_date column; a genuine v6 database
// (media_date dropped, user_version stamped 6) gains the column and is bumped to
// 7 on reopen; and reopening an already-v7 database is a no-op.
func TestMigrationV7AddsMediaDate(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv-v7-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "index.db")

	// Fresh database: created at v7, media_date present.
	idx, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, idx, "/lib", 5)
	if !hasColumn(t, idx, "entries", "media_date") {
		t.Fatalf("fresh database is missing the media_date column")
	}
	if uv := userVersion(t, idx); uv != 7 {
		t.Fatalf("fresh database user_version = %d, want 7", uv)
	}

	// Rewind to a genuine v6 layout: drop media_date, stamp user_version = 6.
	// (DROP COLUMN needs SQLite >= 3.35, the same floor ToggleFavorite's
	// RETURNING already relies on.)
	for _, stmt := range []string{
		"ALTER TABLE entries DROP COLUMN media_date",
		"PRAGMA user_version = 6",
	} {
		if _, err := idx.db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if hasColumn(t, idx, "entries", "media_date") {
		t.Fatalf("rewind failed: media_date still present before upgrade")
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the v7 migration must add the column back and bump to 7 without
	// disturbing the existing rows.
	idx2, err := Load(dbPath)
	if err != nil {
		t.Fatalf("v6 -> v7 upgrade: %v", err)
	}
	if !hasColumn(t, idx2, "entries", "media_date") {
		t.Errorf("media_date column should exist after v6 -> v7 upgrade")
	}
	if uv := userVersion(t, idx2); uv != 7 {
		t.Errorf("user_version = %d after upgrade, want 7", uv)
	}
	if got := idx2.Count(); got != 5 {
		t.Errorf("row count = %d after upgrade, want 5 (upgrade must not drop data)", got)
	}
	if err := idx2.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen an already-v7 database: idempotent, still v7, column intact.
	idx3, err := Load(dbPath)
	if err != nil {
		t.Fatalf("reopen v7: %v", err)
	}
	defer idx3.Close()
	if !hasColumn(t, idx3, "entries", "media_date") {
		t.Errorf("media_date column vanished on idempotent reopen")
	}
	if uv := userVersion(t, idx3); uv != 7 {
		t.Errorf("user_version = %d on idempotent reopen, want 7", uv)
	}
}

func userVersion(t *testing.T, idx *Index) int {
	t.Helper()
	var uv int
	if err := idx.db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatal(err)
	}
	return uv
}

// TestReconcileNullsMediaDateOnMtimeChange proves the mtime-keyed invalidation
// (U-14): a persisted media_date survives a reconcile that reports the same
// size/mtime, but is nulled when either changes — exactly mirroring how the
// upsert already invalidates content_hash/quick_hash. An edited file therefore
// re-probes on the next Organize scan.
func TestReconcileNullsMediaDateOnMtimeChange(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	base := scan.Result{
		Path:    "/lib/clip.mp4",
		Type:    scan.TypeVideo,
		Size:    1000,
		ModTime: time.Unix(1_600_000_000, 0),
	}
	idx.ReconcileBatch([]scan.Result{base})

	md := time.Date(2024, 3, 15, 12, 0, 0, 0, time.UTC)
	if err := idx.SetProbedMediaDate(base.Path, md); err != nil {
		t.Fatalf("SetProbedMediaDate: %v", err)
	}
	if got, ok, err := idx.ProbedMediaDate(base.Path); err != nil || !ok || !got.Equal(md) {
		t.Fatalf("seed media_date: got=%v ok=%v err=%v, want %v true nil", got, ok, err, md)
	}

	// Same size + mtime: media_date preserved.
	idx.ReconcileBatch([]scan.Result{base})
	if got, ok, err := idx.ProbedMediaDate(base.Path); err != nil || !ok || !got.Equal(md) {
		t.Fatalf("after same-mtime reconcile: got=%v ok=%v err=%v, want preserved %v", got, ok, err, md)
	}

	// Changed mtime: media_date nulled.
	changedMtime := base
	changedMtime.ModTime = time.Unix(1_700_000_000, 0)
	idx.ReconcileBatch([]scan.Result{changedMtime})
	if _, ok, err := idx.ProbedMediaDate(base.Path); err != nil || ok {
		t.Fatalf("after mtime change: ok=%v err=%v, want media_date nulled (ok=false)", ok, err)
	}

	// Re-persist, then change size: media_date nulled again.
	if err := idx.SetProbedMediaDate(changedMtime.Path, md); err != nil {
		t.Fatalf("re-persist media_date: %v", err)
	}
	changedSize := changedMtime
	changedSize.Size = 2000
	idx.ReconcileBatch([]scan.Result{changedSize})
	if _, ok, err := idx.ProbedMediaDate(base.Path); err != nil || ok {
		t.Fatalf("after size change: ok=%v err=%v, want media_date nulled (ok=false)", ok, err)
	}
}

// TestProbedMediaDateRoundTrip covers the point read/persist pair: an unprobed
// row reports ok=false, a persisted value round-trips, setting one row leaves
// its neighbours untouched, and a missing path is a harmless no-op for both
// read and write.
func TestProbedMediaDateRoundTrip(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()
	entries := seed(t, idx, "/lib", 3)
	path := entries[0].Path

	// A freshly reconciled row has media_date NULL -> not probed.
	if _, ok, err := idx.ProbedMediaDate(path); err != nil || ok {
		t.Fatalf("unprobed row: ok=%v err=%v, want ok=false err=nil", ok, err)
	}

	want := time.Date(2019, 6, 2, 8, 30, 45, 0, time.UTC)
	if err := idx.SetProbedMediaDate(path, want); err != nil {
		t.Fatalf("SetProbedMediaDate: %v", err)
	}
	got, ok, err := idx.ProbedMediaDate(path)
	if err != nil || !ok {
		t.Fatalf("read after persist: ok=%v err=%v, want ok=true", ok, err)
	}
	if !got.Equal(want) {
		t.Errorf("round-trip media_date = %v, want %v", got, want)
	}

	// Neighbours are untouched.
	for _, n := range entries[1:] {
		if _, ok, _ := idx.ProbedMediaDate(n.Path); ok {
			t.Errorf("neighbour %s should still be unprobed", n.Path)
		}
	}

	// A missing path reads as unprobed with no error and writes nothing.
	if _, ok, err := idx.ProbedMediaDate("/lib/nope.mp4"); err != nil || ok {
		t.Errorf("missing-path read: ok=%v err=%v, want ok=false err=nil", ok, err)
	}
	if err := idx.SetProbedMediaDate("/lib/nope.mp4", want); err != nil {
		t.Errorf("missing-path write err = %v, want nil", err)
	}
}
