package cache

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// TestMigrationV6SweepsOrphanFaces proves the one-time v6 hygiene (C-01):
// face rows keyed to paths no longer in entries — left behind by deletes/moves
// before the RemoveEntry/MoveFaces cleanup landed — are swept on upgrade, and
// any cluster sample_face_id pointing at a swept face is nulled. Valid face
// rows and valid sample pointers are left untouched.
func TestMigrationV6SweepsOrphanFaces(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv-v6-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "index.db")

	idx, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	// A real entry with a valid face + cluster (sample points at the valid face).
	root := "/lib"
	seed(t, idx, root, 3)
	kept := root + "/000000.jpg"
	if _, err := idx.WriteFacesForPath(kept, []FaceOp{{
		Face:               Face{Path: kept, ThumbMtime: 1, BBox: [4]int{1, 2, 3, 4}, Embedding: []float32{0.1, 0.2}},
		NewClusterCentroid: []float32{0.1, 0.2},
	}}); err != nil {
		t.Fatalf("WriteFacesForPath: %v", err)
	}

	// An orphan face: its path has no entries row (simulates a pre-v6 delete or
	// organize move that never cleaned faces). A cluster's sample points at it so
	// we can assert the dangling pointer is nulled once the orphan is swept.
	res, err := idx.db.Exec(
		"INSERT INTO faces (path, thumb_mtime, bbox_x, bbox_y, bbox_w, bbox_h, embedding, cluster_id) VALUES (?,?,?,?,?,?,?,?)",
		"/lib/deleted.jpg", 1, 0, 0, 1, 1, []byte{0, 1, 2, 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	orphanID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.db.Exec(
		"INSERT INTO face_clusters (label, centroid, sample_face_id) VALUES (?,?,?)",
		"ghost", []byte{0, 1}, orphanID); err != nil {
		t.Fatal(err)
	}

	// Downgrade the schema stamp so reopening re-runs the v6 migration.
	if _, err := idx.db.Exec("PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: v6 must sweep the orphan face and null the dangling sample_face_id
	// while leaving the valid face row intact.
	idx2, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()

	fresh := idx2.LoadFaceFreshness()
	if _, ok := fresh["/lib/deleted.jpg"]; ok {
		t.Errorf("orphan face row for deleted path should have been swept")
	}
	if _, ok := fresh[kept]; !ok {
		t.Errorf("valid face row for %s should have been kept", kept)
	}

	var ghost sql.NullInt64
	if err := idx2.db.QueryRow(
		"SELECT sample_face_id FROM face_clusters WHERE label = 'ghost'").Scan(&ghost); err != nil {
		t.Fatal(err)
	}
	if ghost.Valid {
		t.Errorf("dangling sample_face_id = %d, want NULL after sweep", ghost.Int64)
	}

	// The valid cluster's sample still points at a live face, so it must survive.
	var live sql.NullInt64
	if err := idx2.db.QueryRow(
		"SELECT sample_face_id FROM face_clusters WHERE label IS NULL").Scan(&live); err != nil {
		t.Fatal(err)
	}
	if !live.Valid {
		t.Errorf("valid sample_face_id should not have been nulled")
	}

	var uv int
	_ = idx2.db.QueryRow("PRAGMA user_version").Scan(&uv)
	if uv != 7 {
		t.Errorf("user_version = %d, want 7", uv)
	}
}

// indexNames returns the set of index names present on the DB.
func indexNames(t *testing.T, idx *Index) map[string]bool {
	t.Helper()
	have := map[string]bool{}
	rows, err := idx.db.Query("SELECT name FROM sqlite_master WHERE type='index'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err == nil {
			have[n] = true
		}
	}
	return have
}

// TestMigrationV6UpgradesIndexes exercises the real v5 -> v6 transition on a
// database that already holds the pre-v6 index shapes: the single-column
// idx_entries_favorite and the path-less idx_entries_yearexpr. This is the
// scenario the v6 DROP+CREATE guards — the top-level CREATE IF NOT EXISTS is a
// no-op when an index of that name already exists, so without the explicit
// recreate the path-less year index would silently survive the upgrade.
func TestMigrationV6UpgradesIndexes(t *testing.T) {
	dir, err := os.MkdirTemp("", "pv-v6up-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	dbPath := filepath.Join(dir, "index.db")

	idx, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, idx, "/lib", 50)

	// Rewind the schema to a genuine v5 layout: drop the v6 index forms, put the
	// pre-v6 ones back, and stamp user_version=5.
	for _, stmt := range []string{
		"DROP INDEX IF EXISTS idx_entries_favorite_path",
		"DROP INDEX IF EXISTS idx_entries_yearexpr",
		"CREATE INDEX idx_entries_favorite ON entries(favorite)",
		"CREATE INDEX idx_entries_yearexpr ON entries(" + yearExpr + ")",
		"PRAGMA user_version = 5",
	} {
		if _, err := idx.db.Exec(stmt); err != nil {
			t.Fatalf("rewind %q: %v", stmt, err)
		}
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen: the v6 migration now runs against the v5 layout.
	idx2, err := Load(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer idx2.Close()

	have := indexNames(t, idx2)
	if have["idx_entries_favorite"] {
		t.Errorf("old single-column idx_entries_favorite should be dropped on upgrade")
	}
	if !have["idx_entries_favorite_path"] {
		t.Errorf("idx_entries_favorite_path should exist after upgrade")
	}
	if !have["idx_entries_yearexpr"] {
		t.Errorf("idx_entries_yearexpr should exist after upgrade")
	}

	// The recreated year index must include path — a path-only year query is now
	// served entirely from the index (COVERING), which the path-less v5 index
	// could not do.
	yearPlan := explainPlan(t, idx2, "SELECT path FROM entries WHERE "+yearExpr+" = ?", 2024)
	mustContain(t, "upgraded-year", yearPlan, "SEARCH entries USING COVERING INDEX idx_entries_yearexpr")

	// The favorites page must stream off (favorite, path) with no temp b-tree.
	v := View{Kind: "favorites"}
	where, args := v.whereClause()
	pageArgs := append(append([]any{}, args...), 60, 0)
	favPlan := explainPlan(t, idx2,
		"SELECT path, type, size, mtime, thumb_id, favorite, duration_ms FROM entries WHERE "+
			where+" ORDER BY path LIMIT ? OFFSET ?", pageArgs...)
	mustContain(t, "upgraded-fav", favPlan, "SEARCH entries USING INDEX idx_entries_favorite_path")
	mustNotContain(t, "upgraded-fav", favPlan, "TEMP B-TREE")
}
