package cache

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestMovePathCarriesState verifies MovePath relocates the entry and its faces
// while keeping every cached attribute: favorite, duration, hashes and the
// probed media_date all survive a rename because the bytes (size/mtime) are
// unchanged. It also re-derives thumb_id and year from the new path.
func TestMovePathCarriesState(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	oldPath := "/lib/2023-05-01/clip.mp4"
	newPath := "/lib/2024-07-09/clip.mp4"

	idx.ReconcileBatch([]scan.Result{{
		Path:       oldPath,
		Type:       scan.TypeVideo,
		Size:       10,
		ModTime:    time.Unix(1_600_000_000, 0),
		DurationMs: 4200,
	}})
	if err := idx.SetFavorite(oldPath, true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}
	probed := time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := idx.SetProbedMediaDate(oldPath, probed); err != nil {
		t.Fatalf("SetProbedMediaDate: %v", err)
	}
	if _, err := idx.db.Exec("UPDATE entries SET quick_hash = 'q', content_hash = 'c' WHERE path = ?", oldPath); err != nil {
		t.Fatalf("seed hashes: %v", err)
	}
	writeFace(t, idx, oldPath)

	if err := idx.MovePath(oldPath, newPath); err != nil {
		t.Fatalf("MovePath: %v", err)
	}

	if _, ok := idx.GetEntry(oldPath); ok {
		t.Errorf("GetEntry(old) still present after MovePath")
	}
	got, ok := idx.GetEntry(newPath)
	if !ok {
		t.Fatalf("GetEntry(new) not found after MovePath")
	}
	if !got.Favorite {
		t.Errorf("favorite = false, want true")
	}
	if got.DurationMs != 4200 {
		t.Errorf("duration_ms = %d, want 4200", got.DurationMs)
	}
	if want := ThumbIDFor(newPath); got.ThumbID != want {
		t.Errorf("thumb_id = %q, want %q", got.ThumbID, want)
	}

	date, ok, err := idx.ProbedMediaDate(newPath)
	if err != nil {
		t.Fatalf("ProbedMediaDate: %v", err)
	}
	if !ok {
		t.Fatalf("ProbedMediaDate(new) not cached after move")
	}
	if !date.Equal(probed) {
		t.Errorf("media_date = %v, want %v", date, probed)
	}

	var quick, content string
	if err := idx.db.QueryRow("SELECT quick_hash, content_hash FROM entries WHERE path = ?", newPath).Scan(&quick, &content); err != nil {
		t.Fatalf("select hashes: %v", err)
	}
	if quick != "q" || content != "c" {
		t.Errorf("hashes = (%q, %q), want (\"q\", \"c\")", quick, content)
	}

	var year int
	if err := idx.db.QueryRow("SELECT year FROM entries WHERE path = ?", newPath).Scan(&year); err != nil {
		t.Fatalf("select year: %v", err)
	}
	if year != 2024 {
		t.Errorf("year = %d, want 2024", year)
	}

	fresh := idx.LoadFaceFreshness()
	if _, ok := fresh[newPath]; !ok {
		t.Errorf("face freshness missing new path")
	}
	if _, ok := fresh[oldPath]; ok {
		t.Errorf("face freshness still keyed to old path")
	}

	if n := idx.Count(); n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
}

// TestMovePathYearNullWhenUndated verifies the year column is cleared (NULL, not
// 0 or the stale year) when the destination's parent dir isn't YYYY-MM-DD, so
// the sidebar falls back to the mtime-derived year.
func TestMovePathYearNullWhenUndated(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	oldPath := "/lib/2023-05-01/a.jpg"
	newPath := "/lib/misc/a.jpg"
	idx.ReconcileBatch([]scan.Result{{
		Path:    oldPath,
		Type:    scan.TypePhoto,
		Size:    10,
		ModTime: time.Unix(1_600_000_000, 0),
	}})

	if err := idx.MovePath(oldPath, newPath); err != nil {
		t.Fatalf("MovePath: %v", err)
	}

	var year sql.NullInt64
	if err := idx.db.QueryRow("SELECT year FROM entries WHERE path = ?", newPath).Scan(&year); err != nil {
		t.Fatalf("select year: %v", err)
	}
	if year.Valid {
		t.Errorf("year = %d (valid), want NULL", year.Int64)
	}
}

// TestMovePathNoRow verifies a move for an unindexed source reports sql.ErrNoRows
// and leaves the index untouched, letting ApplyMove fall back to a fresh stat.
func TestMovePathNoRow(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	err := idx.MovePath("/lib/nope/a.jpg", "/lib/elsewhere/a.jpg")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("MovePath error = %v, want sql.ErrNoRows", err)
	}
	if n := idx.Count(); n != 0 {
		t.Errorf("Count = %d, want 0", n)
	}
}

// TestMovePathReplacesStaleTarget verifies that when both paths have rows the
// source wins: the stale target row is dropped, not collided with, and its
// favorite flag is replaced by the source's.
func TestMovePathReplacesStaleTarget(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	src := "/lib/x/a.jpg"
	dst := "/lib/y/a.jpg"
	idx.ReconcileBatch([]scan.Result{
		{Path: src, Type: scan.TypePhoto, Size: 10, ModTime: time.Unix(1_600_000_000, 0)},
		{Path: dst, Type: scan.TypePhoto, Size: 20, ModTime: time.Unix(1_600_000_100, 0)},
	})
	if err := idx.SetFavorite(src, true); err != nil {
		t.Fatalf("SetFavorite: %v", err)
	}

	if err := idx.MovePath(src, dst); err != nil {
		t.Fatalf("MovePath: %v", err)
	}

	if n := idx.Count(); n != 1 {
		t.Errorf("Count = %d, want 1", n)
	}
	got, ok := idx.GetEntry(dst)
	if !ok {
		t.Fatalf("GetEntry(dst) not found after MovePath")
	}
	if !got.Favorite {
		t.Errorf("GetEntry(dst).Favorite = false, want true")
	}
}
