package cache

import (
	"fmt"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestReconcileUnchangedRowsSkipped checks the C4-01 guard: a second reconcile
// of an identical batch must touch no row at all. The connection pool is
// capped at one so total_changes() is observed on the same connection the
// reconcile used.
func TestReconcileUnchangedRowsSkipped(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	idx.db.SetMaxOpenConns(1)

	const n = 50
	results := make([]scan.Result, 0, n)
	for i := 0; i < n; i++ {
		results = append(results, scan.Result{
			Path:    fmt.Sprintf("/lib/2024-01-02/%03d.jpg", i),
			Type:    scan.TypePhoto,
			Size:    int64(1000 + i),
			ModTime: time.Unix(1_600_000_000+int64(i), 0),
		})
	}

	if got := idx.ReconcileBatchChanged(results); got != n {
		t.Fatalf("first ReconcileBatchChanged = %d, want %d (all inserts)", got, n)
	}

	var before int
	if err := idx.db.QueryRow("SELECT total_changes()").Scan(&before); err != nil {
		t.Fatalf("read total_changes() after first batch: %v", err)
	}
	if before < n {
		t.Fatalf("total_changes() after first batch = %d, want at least %d", before, n)
	}

	if got := idx.ReconcileBatchChanged(results); got != 0 {
		t.Fatalf("second ReconcileBatchChanged = %d, want 0 (unchanged rows must be skipped)", got)
	}

	var after int
	if err := idx.db.QueryRow("SELECT total_changes()").Scan(&after); err != nil {
		t.Fatalf("read total_changes() after second batch: %v", err)
	}
	if after != before {
		t.Fatalf("total_changes() moved from %d to %d on an unchanged rescan; no row should be rewritten", before, after)
	}
}

// TestReconcileMtimeBumpNullsHashes checks that only a file whose mtime moved is
// updated, and that its hash and probed media-date caches are invalidated while
// the untouched siblings keep theirs.
func TestReconcileMtimeBumpNullsHashes(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	base := time.Unix(1_700_000_000, 0)
	results := []scan.Result{
		{Path: "/lib/a.jpg", Type: scan.TypePhoto, Size: 100, ModTime: base},
		{Path: "/lib/b.jpg", Type: scan.TypePhoto, Size: 200, ModTime: base},
		{Path: "/lib/c.jpg", Type: scan.TypePhoto, Size: 300, ModTime: base},
	}
	if entries := idx.ReconcileBatch(results); len(entries) != 3 {
		t.Fatalf("ReconcileBatch returned %d entries, want 3", len(entries))
	}

	if _, err := idx.db.Exec("UPDATE entries SET quick_hash='q', content_hash='c'"); err != nil {
		t.Fatalf("seed hashes: %v", err)
	}
	for i, r := range results {
		if err := idx.SetProbedMediaDate(r.Path, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("SetProbedMediaDate(%s): %v", r.Path, err)
		}
	}

	bumped := results[1].Path
	results[1].ModTime = results[1].ModTime.Add(10 * time.Second)

	if got := idx.ReconcileBatchChanged(results); got != 1 {
		t.Fatalf("ReconcileBatchChanged after mtime bump = %d, want 1", got)
	}

	rows, err := idx.db.Query("SELECT path FROM entries WHERE quick_hash IS NULL AND content_hash IS NULL")
	if err != nil {
		t.Fatalf("query null-hash rows: %v", err)
	}
	var nulled []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			t.Fatalf("scan null-hash path: %v", err)
		}
		nulled = append(nulled, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatalf("iterate null-hash rows: %v", err)
	}
	rows.Close()

	if len(nulled) != 1 || nulled[0] != bumped {
		t.Fatalf("rows with both hashes NULL = %v, want exactly [%s]", nulled, bumped)
	}

	if _, ok, err := idx.ProbedMediaDate(bumped); err != nil {
		t.Fatalf("ProbedMediaDate(%s): %v", bumped, err)
	} else if ok {
		t.Fatalf("ProbedMediaDate(%s) reported ok=true after an mtime bump, want false", bumped)
	}
	for _, r := range []scan.Result{results[0], results[2]} {
		if _, ok, err := idx.ProbedMediaDate(r.Path); err != nil {
			t.Fatalf("ProbedMediaDate(%s): %v", r.Path, err)
		} else if !ok {
			t.Fatalf("ProbedMediaDate(%s) reported ok=false, want true (untouched row)", r.Path)
		}
	}
}

// TestReconcileDurationZeroPreserved checks that an unprobed (zero) duration
// never overwrites a known one, while a new non-zero duration does.
func TestReconcileDurationZeroPreserved(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	const path = "/lib/clip.mp4"
	video := func(durMs int64) scan.Result {
		return scan.Result{
			Path:       path,
			Type:       scan.TypeVideo,
			Size:       10,
			ModTime:    time.Unix(1_700_000_000, 0),
			DurationMs: durMs,
		}
	}

	if got := idx.ReconcileBatchChanged([]scan.Result{video(5000)}); got != 1 {
		t.Fatalf("insert video with duration 5000 = %d changed, want 1", got)
	}

	if got := idx.ReconcileBatchChanged([]scan.Result{video(0)}); got != 0 {
		t.Fatalf("reconcile with duration 0 = %d changed, want 0 (zero must not overwrite)", got)
	}
	if e, ok := idx.GetEntry(path); !ok {
		t.Fatalf("GetEntry(%s) missing after zero-duration reconcile", path)
	} else if e.DurationMs != 5000 {
		t.Fatalf("DurationMs after zero-duration reconcile = %d, want 5000", e.DurationMs)
	}

	if got := idx.ReconcileBatchChanged([]scan.Result{video(5000)}); got != 0 {
		t.Fatalf("reconcile with unchanged duration 5000 = %d changed, want 0", got)
	}

	if got := idx.ReconcileBatchChanged([]scan.Result{video(7000)}); got != 1 {
		t.Fatalf("reconcile with duration 7000 = %d changed, want 1", got)
	}
	if e, ok := idx.GetEntry(path); !ok {
		t.Fatalf("GetEntry(%s) missing after duration update", path)
	} else if e.DurationMs != 7000 {
		t.Fatalf("DurationMs after duration update = %d, want 7000", e.DurationMs)
	}
}

// TestReconcileTypeChangeCounts checks that a type change with identical size
// and mtime still counts as a changed row and is persisted.
func TestReconcileTypeChangeCounts(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	const path = "/lib/shot.jpg"
	mtime := time.Unix(1_700_000_000, 0)
	photo := scan.Result{Path: path, Type: scan.TypePhoto, Size: 42, ModTime: mtime}
	raw := scan.Result{Path: path, Type: scan.TypeRAW, Size: 42, ModTime: mtime}

	if got := idx.ReconcileBatchChanged([]scan.Result{photo}); got != 1 {
		t.Fatalf("insert as photo = %d changed, want 1", got)
	}
	if got := idx.ReconcileBatchChanged([]scan.Result{raw}); got != 1 {
		t.Fatalf("reconcile same size/mtime with a new type = %d changed, want 1", got)
	}
	if e, ok := idx.GetEntry(path); !ok {
		t.Fatalf("GetEntry(%s) missing after type change", path)
	} else if e.Type != scan.TypeRAW {
		t.Fatalf("Type after type change = %v, want %v", e.Type, scan.TypeRAW)
	}
}
