package cache

import (
	"database/sql"
	"time"
)

// migrateV9 adds the face-recognition data model (schema v9).
//
// The face data is split into two layers:
//
//   - machine (disposable): faces, face_clusters. Rewritten by re-detection,
//     wiped by Clear (Rebuild).
//   - user (durable): people, face_tags. Never touched by re-detection or
//     Clear; re-applied onto fresh face rows by applyTagsTx.
//
// Every step is idempotent (IF NOT EXISTS / addColumn), so a crash before
// user_version is bumped just re-runs it. The one-time data step — turning
// the never-written face_clusters.label into people rows — only runs below
// v9 and is itself idempotent (INSERT OR IGNORE, person_id IS NULL guard).
func migrateV9(db *sql.DB, userVersion int) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS people (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			name        TEXT NOT NULL,
			hidden      INTEGER NOT NULL DEFAULT 0,
			cover_path  TEXT,
			cover_bbox  TEXT,
			created_at  INTEGER NOT NULL
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_people_name ON people(name COLLATE NOCASE);
		CREATE TABLE IF NOT EXISTS face_tags (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			path       TEXT NOT NULL,
			nx REAL NOT NULL, ny REAL NOT NULL, nw REAL NOT NULL, nh REAL NOT NULL,
			embedding  BLOB NOT NULL,
			verdict    TEXT NOT NULL CHECK (verdict IN ('confirmed','rejected','ignored')),
			person_id  INTEGER,
			updated_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_face_tags_path ON face_tags(path);
		CREATE INDEX IF NOT EXISTS idx_face_tags_person ON face_tags(person_id, verdict);
	`); err != nil {
		return err
	}
	for _, alter := range []string{
		"ALTER TABLE face_clusters ADD COLUMN person_id INTEGER",
		"ALTER TABLE faces ADD COLUMN person_id INTEGER",
		"ALTER TABLE faces ADD COLUMN assign TEXT NOT NULL DEFAULT 'cluster'",
		"ALTER TABLE faces ADD COLUMN quality REAL",
		// The bbox normalised to 0..1 of the rendition it was detected on. Not
		// in the plan's sketch but needed by it: face_tags match on normalised
		// boxes (so a detector-rendition change keeps matching), and the
		// viewer draws boxes onto whatever size it displays. NULL on rows
		// written before v9 (their rendition size was never recorded); the
		// tag matcher falls back to the embedding for those.
		"ALTER TABLE faces ADD COLUMN nx REAL",
		"ALTER TABLE faces ADD COLUMN ny REAL",
		"ALTER TABLE faces ADD COLUMN nw REAL",
		"ALTER TABLE faces ADD COLUMN nh REAL",
	} {
		if err := addColumn(db, alter); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_faces_person ON faces(person_id, path);
		CREATE INDEX IF NOT EXISTS idx_face_clusters_person ON face_clusters(person_id);
	`); err != nil {
		return err
	}
	if userVersion >= 9 {
		return nil
	}

	// Carry any legacy cluster labels into people. Nothing wrote label before
	// v9, but RenameCluster existed, so a hand-edited database may have some.
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO people (name, created_at)
		SELECT DISTINCT TRIM(label), ? FROM face_clusters
		WHERE label IS NOT NULL AND TRIM(label) <> ''`, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE face_clusters SET person_id =
			(SELECT p.id FROM people p WHERE p.name = TRIM(face_clusters.label) COLLATE NOCASE)
		WHERE person_id IS NULL AND label IS NOT NULL AND TRIM(label) <> ''`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE faces SET person_id =
			(SELECT c.person_id FROM face_clusters c WHERE c.id = faces.cluster_id)
		WHERE assign = 'cluster' AND cluster_id IS NOT NULL`); err != nil {
		return err
	}
	return tx.Commit()
}
