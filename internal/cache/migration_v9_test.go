package cache

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// v8Schema is the face-related layout a v8 database had: the v9 migration
// must bring it forward without losing rows.
const v8Schema = `
	CREATE TABLE entries (
		path TEXT PRIMARY KEY, type INTEGER, size INTEGER, mtime INTEGER,
		thumb_id TEXT, content_hash TEXT, favorite INTEGER NOT NULL DEFAULT 0,
		quick_hash TEXT, year INTEGER, duration_ms INTEGER NOT NULL DEFAULT 0,
		media_date INTEGER, face_thumb_mtime INTEGER
	);
	CREATE TABLE face_clusters (
		id INTEGER PRIMARY KEY AUTOINCREMENT, label TEXT,
		centroid BLOB NOT NULL, sample_face_id INTEGER
	);
	CREATE TABLE faces (
		id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT NOT NULL,
		thumb_mtime INTEGER NOT NULL, bbox_x INTEGER NOT NULL, bbox_y INTEGER NOT NULL,
		bbox_w INTEGER NOT NULL, bbox_h INTEGER NOT NULL, embedding BLOB NOT NULL,
		cluster_id INTEGER
	);
	INSERT INTO entries (path, type, size, mtime, thumb_id, face_thumb_mtime) VALUES ('/lib/a.jpg', 0, 1, 1, 'x', 5);
	INSERT INTO face_clusters (id, label, centroid) VALUES (1, ' Ana ', x'00000000');
	INSERT INTO face_clusters (id, label, centroid) VALUES (2, 'ana', x'00000000');
	INSERT INTO face_clusters (id, label, centroid) VALUES (3, NULL, x'00000000');
	INSERT INTO faces (path, thumb_mtime, bbox_x, bbox_y, bbox_w, bbox_h, embedding, cluster_id)
		VALUES ('/lib/a.jpg', 5, 1, 2, 3, 4, x'00000000', 1), ('/lib/a.jpg', 5, 1, 2, 3, 4, x'00000000', 3);
	PRAGMA user_version = 8;
`

func TestMigrationV9FreshDatabase(t *testing.T) {
	idx, err := Load(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if uv := userVersion(t, idx); uv != 9 {
		t.Errorf("user_version = %d, want 9", uv)
	}
	for _, c := range [][2]string{
		{"people", "name"}, {"people", "hidden"}, {"people", "cover_path"},
		{"face_tags", "verdict"}, {"face_tags", "embedding"},
		{"face_clusters", "person_id"},
		{"faces", "person_id"}, {"faces", "assign"}, {"faces", "quality"}, {"faces", "nx"},
	} {
		if !hasColumn(t, idx, c[0], c[1]) {
			t.Errorf("%s.%s missing on a fresh v9 database", c[0], c[1])
		}
	}
}

func TestMigrationV9UpgradeCarriesLabels(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	raw, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(v8Schema); err != nil {
		t.Fatalf("seed v8 schema: %v", err)
	}
	_ = raw.Close()

	for round := range 2 { // second round = idempotent re-open
		idx, err := Load(dbPath)
		if err != nil {
			t.Fatalf("round %d: Load: %v", round, err)
		}
		if uv := userVersion(t, idx); uv != 9 {
			t.Errorf("round %d: user_version = %d, want 9", round, uv)
		}
		var people int
		var name string
		if err := idx.db.QueryRow("SELECT COUNT(*), MIN(name) FROM people").Scan(&people, &name); err != nil {
			t.Fatal(err)
		}
		// " Ana " and "ana" are the same person (trimmed, case-insensitive).
		if people != 1 || name != "Ana" {
			t.Errorf("round %d: people = %d (%q), want exactly one 'Ana'", round, people, name)
		}
		var c1, c2 sql.NullInt64
		var c3 sql.NullInt64
		_ = idx.db.QueryRow("SELECT person_id FROM face_clusters WHERE id = 1").Scan(&c1)
		_ = idx.db.QueryRow("SELECT person_id FROM face_clusters WHERE id = 2").Scan(&c2)
		_ = idx.db.QueryRow("SELECT person_id FROM face_clusters WHERE id = 3").Scan(&c3)
		if !c1.Valid || c1 != c2 {
			t.Errorf("round %d: labelled clusters person_id = %v, %v; want the same person", round, c1, c2)
		}
		if c3.Valid {
			t.Errorf("round %d: unlabelled cluster got person_id %v", round, c3)
		}
		var labelled, unlabelled sql.NullInt64
		var assign string
		_ = idx.db.QueryRow("SELECT person_id, assign FROM faces WHERE cluster_id = 1").Scan(&labelled, &assign)
		_ = idx.db.QueryRow("SELECT person_id FROM faces WHERE cluster_id = 3").Scan(&unlabelled)
		if labelled != c1 || assign != "cluster" {
			t.Errorf("round %d: face in labelled cluster = (%v, %q), want (%v, cluster)", round, labelled, assign, c1)
		}
		if unlabelled.Valid {
			t.Errorf("round %d: face in unlabelled cluster got person %v", round, unlabelled)
		}
		var faces int
		_ = idx.db.QueryRow("SELECT COUNT(*) FROM faces").Scan(&faces)
		if faces != 2 {
			t.Errorf("round %d: faces = %d, want 2 (upgrade must not drop rows)", round, faces)
		}
		_ = idx.Close()
	}
}
