package cache

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrationV8BackfillsFaceFreshness proves the v7 -> v8 migration (F5-01):
// face freshness used to be derivable only from the face rows, so the v8
// migration must seed entries.face_thumb_mtime from the faces written before
// the column existed. Otherwise every path that already had faces would be
// re-detected once on the first run against the upgraded database.
func TestMigrationV8BackfillsFaceFreshness(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv-v8-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "index.db")

	idx, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, idx, "/lib", 3)

	// The existing write path already records freshness on the entries row; the
	// point of the test is rewinding that row to look like a pre-v8 database.
	path := "/lib/000000.jpg"
	if _, err := idx.WriteFacesForPath(path, 7, []FaceOp{{
		Face:               Face{Path: path, ThumbMtime: 7, BBox: [4]int{1, 2, 3, 4}, Embedding: []float32{0.1, 0.2}},
		NewClusterCentroid: []float32{0.1, 0.2},
	}}); err != nil {
		t.Fatalf("WriteFacesForPath(%s): %v", path, err)
	}

	// Simulate a genuine v7 layout: no freshness on the entries row (the column
	// did not exist) and user_version stamped 7.
	for _, stmt := range []string{
		"UPDATE entries SET face_thumb_mtime = NULL",
		"PRAGMA user_version = 7",
	} {
		if _, err := idx.db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if got := idx.LoadFaceFreshness(); len(got) != 0 {
		t.Fatalf("rewind failed: freshness = %v, want empty before upgrade", got)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the v8 migration backfills from the face rows and bumps to 8.
	idx2, err := Load(dbPath)
	if err != nil {
		t.Fatalf("v7 -> v8 upgrade: %v", err)
	}
	defer idx2.Close()
	if uv := userVersion(t, idx2); uv != 9 {
		t.Errorf("user_version = %d after upgrade, want 9", uv)
	}
	if !hasColumn(t, idx2, "entries", "face_thumb_mtime") {
		t.Error("entries.face_thumb_mtime should exist after v7 -> v8 upgrade")
	}
	fresh := idx2.LoadFaceFreshness()
	if len(fresh) != 1 {
		t.Fatalf("freshness after upgrade = %v, want exactly one key", fresh)
	}
	if got, ok := fresh[path]; !ok {
		t.Errorf("no freshness backfilled for %s", path)
	} else if got != 7 {
		t.Errorf("backfilled freshness for %s = %d, want 7 (the face row's thumb_mtime)", path, got)
	}
}

// TestZeroFaceWritePersistsAcrossLoad is the core F5-01 guarantee: a run that
// found no faces still records the thumbnail mtime it ran against, and that
// record survives a process restart (index close + reopen) even though the
// path has no face rows at all.
func TestZeroFaceWritePersistsAcrossLoad(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv-v8-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "index.db")

	idx, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, idx, "/lib", 2)

	path := "/lib/000001.jpg"
	if _, err := idx.WriteFacesForPath(path, 42, nil); err != nil {
		t.Fatalf("WriteFacesForPath(%s, 42, nil): %v", path, err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	idx2, err := Load(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer idx2.Close()
	if got, ok := idx2.LoadFaceFreshness()[path]; !ok {
		t.Errorf("no freshness for a zero-face path after reopen")
	} else if got != 42 {
		t.Errorf("zero-face freshness after reopen = %d, want 42", got)
	}

	var n int
	if err := idx2.db.QueryRow("SELECT COUNT(*) FROM faces WHERE path = ?", path).Scan(&n); err != nil {
		t.Fatalf("count faces for %s: %v", path, err)
	}
	if n != 0 {
		t.Errorf("faces for a zero-face path = %d, want 0", n)
	}
}

// TestFaceFreshnessDroppedWithEntry verifies freshness follows the entries row:
// a zero-face path is only "already scanned" while its entry exists, so
// deleting the entry (its file vanished from disk) must drop the record —
// otherwise a re-appearing path would be skipped without ever being detected.
func TestFaceFreshnessDroppedWithEntry(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	seed(t, idx, "/lib", 1)
	path := "/lib/000000.jpg"
	if _, err := idx.WriteFacesForPath(path, 42, nil); err != nil {
		t.Fatalf("WriteFacesForPath(%s, 42, nil): %v", path, err)
	}
	if got, ok := idx.LoadFaceFreshness()[path]; !ok || got != 42 {
		t.Fatalf("freshness before RemoveEntry = %v (ok=%v), want %s:42", got, ok, path)
	}

	if err := idx.RemoveEntry(path); err != nil {
		t.Fatalf("RemoveEntry: %v", err)
	}
	if got, ok := idx.LoadFaceFreshness()[path]; ok {
		t.Errorf("freshness for %s survived RemoveEntry (value %d), want it dropped with the row", path, got)
	}
}
