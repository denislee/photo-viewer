package cache

import (
	"database/sql"
	"sort"
)

// Undo is the inverse of one people/tag operation: snapshots of every durable
// row the operation could have changed, taken inside its transaction. Apply
// it with Index.ApplyUndo. It is in-memory only — face_tags is the persisted
// state, so an undo stack does not need to survive a restart.
type Undo struct {
	Label string

	people  []personRow
	created []int64 // people the op created; deleted on undo

	tagPaths []string
	tags     []tagRow

	clusters map[int64]sql.NullInt64 // face_clusters.person_id before
	all      bool                    // op touched rows beyond tagPaths/clusters

	moves []faceMove // cluster moves made after the tag write (face.Retag)
}

type faceMove struct{ face, from int64 }

// AddMove records that face was moved out of cluster from (0 = it was
// unclustered) after this op, so ApplyUndo moves it back.
func (u *Undo) AddMove(face, from int64) {
	if u != nil {
		u.moves = append(u.moves, faceMove{face, from})
	}
}

type personRow struct {
	id                  int64
	name                string
	hidden              int
	coverPath, coverBox sql.NullString
	createdAt           int64
}

type tagRow struct {
	id             int64
	path           string
	nx, ny, nw, nh float64
	embedding      []byte
	verdict        string
	personID       sql.NullInt64
	updatedAt      int64
}

// undoBuilder collects the snapshots while an operation runs.
type undoBuilder struct {
	Undo
	seenPeople map[int64]bool
	seenPaths  map[string]bool
}

func (u *undoBuilder) person(tx *sql.Tx, id int64) error {
	if u.seenPeople == nil {
		u.seenPeople = map[int64]bool{}
	}
	if u.seenPeople[id] {
		return nil
	}
	var r personRow
	if err := tx.QueryRow("SELECT id, name, hidden, cover_path, cover_bbox, created_at FROM people WHERE id = ?", id).
		Scan(&r.id, &r.name, &r.hidden, &r.coverPath, &r.coverBox, &r.createdAt); err != nil {
		if err == sql.ErrNoRows {
			return &personNotFound{id}
		}
		return err
	}
	u.seenPeople[id] = true
	u.people = append(u.people, r)
	return nil
}

type personNotFound struct{ id int64 }

func (e *personNotFound) Error() string { return "person " + itoa(e.id) + " not found" }

func (u *undoBuilder) tagPath(tx *sql.Tx, path string) error {
	if u.seenPaths == nil {
		u.seenPaths = map[string]bool{}
	}
	if u.seenPaths[path] {
		return nil
	}
	u.seenPaths[path] = true
	u.tagPaths = append(u.tagPaths, path)
	rows, err := tx.Query(`SELECT id, path, nx, ny, nw, nh, embedding, verdict, person_id, updated_at
		FROM face_tags WHERE path = ?`, path)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var t tagRow
		if err := rows.Scan(&t.id, &t.path, &t.nx, &t.ny, &t.nw, &t.nh, &t.embedding, &t.verdict, &t.personID, &t.updatedAt); err != nil {
			return err
		}
		u.tags = append(u.tags, t)
	}
	return rows.Err()
}

func (u *undoBuilder) tagPathsWhere(tx *sql.Tx, where string, args ...any) error {
	paths, err := queryStrings(tx, "SELECT DISTINCT path FROM face_tags WHERE "+where, args...)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := u.tagPath(tx, p); err != nil {
			return err
		}
	}
	return nil
}

func (u *undoBuilder) clustersWhere(tx *sql.Tx, where string, args ...any) error {
	if u.clusters == nil {
		u.clusters = map[int64]sql.NullInt64{}
	}
	rows, err := tx.Query("SELECT id, person_id FROM face_clusters WHERE "+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p sql.NullInt64
		if err := rows.Scan(&id, &p); err != nil {
			return err
		}
		if _, ok := u.clusters[id]; !ok {
			u.clusters[id] = p
		}
	}
	return rows.Err()
}

func (u *undoBuilder) affectClusters(ids ...int64) {
	if u.clusters == nil {
		u.clusters = map[int64]sql.NullInt64{}
	}
	for _, id := range ids {
		if _, ok := u.clusters[id]; !ok {
			u.clusters[id] = sql.NullInt64{} // snapshot taken by clustersWhere when it matters
		}
	}
}

func (u *undoBuilder) affectAll() error { u.all = true; return nil }

func queryStrings(tx *sql.Tx, q string, args ...any) ([]string, error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// peopleTx runs fn in one transaction, then drops links to deleted tags and
// re-derives the identity of every face the op could have affected.
func (i *Index) peopleTx(label string, fn func(*sql.Tx, *undoBuilder) error) (*Undo, error) {
	tx, err := i.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	u := &undoBuilder{Undo: Undo{Label: label}}
	if err := fn(tx, u); err != nil {
		return nil, err
	}
	if err := u.finish(tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	out := u.Undo
	return &out, nil
}

func (u *Undo) finish(tx *sql.Tx) error {
	if u.all {
		if _, err := tx.Exec("DELETE FROM face_tag_links WHERE tag_id NOT IN (SELECT id FROM face_tags)"); err != nil {
			return err
		}
		_, err := tx.Exec(rederiveSQL)
		return err
	}
	for _, p := range u.tagPaths {
		if _, err := tx.Exec(`DELETE FROM face_tag_links WHERE face_id IN (SELECT id FROM faces WHERE path = ?)
			AND tag_id NOT IN (SELECT id FROM face_tags WHERE path = ?)`, p, p); err != nil {
			return err
		}
	}
	if err := rederivePathsTx(tx, u.tagPaths...); err != nil {
		return err
	}
	ids := make([]int64, 0, len(u.clusters))
	for id := range u.clusters {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(a, b int) bool { return ids[a] < ids[b] })
	return rederiveClustersTx(tx, ids...)
}

// ApplyUndo restores the rows snapshotted in u and re-derives the affected
// faces. Restored tags are re-attached by the matcher, so an undo still lands
// correctly after the faces were re-detected in between.
func (i *Index) ApplyUndo(u *Undo) error {
	if u == nil {
		return nil
	}
	// Moves first, newest first, so the faces are back in their clusters
	// before identities are re-derived below. A cluster that no longer exists
	// (emptied and dropped) is recreated as a singleton.
	for k := len(u.moves) - 1; k >= 0; k-- {
		m := u.moves[k]
		to := m.from
		if to == 0 {
			to = -1
		} else if !i.clusterExists(to) {
			to = 0
		}
		if _, err := i.ReassignFace(m.face, to, 0); err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range u.created {
		if _, err := tx.Exec("DELETE FROM people WHERE id = ?", id); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE face_clusters SET person_id = NULL WHERE person_id = ?", id); err != nil {
			return err
		}
	}
	for _, r := range u.people {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO people (id, name, hidden, cover_path, cover_bbox, created_at)
			VALUES (?, ?, ?, ?, ?, ?)`, r.id, r.name, r.hidden, r.coverPath, r.coverBox, r.createdAt); err != nil {
			return err
		}
	}
	for _, p := range u.tagPaths {
		if _, err := tx.Exec("DELETE FROM face_tags WHERE path = ?", p); err != nil {
			return err
		}
	}
	for _, t := range u.tags {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO face_tags (id, path, nx, ny, nw, nh, embedding, verdict, person_id, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.id, t.path, t.nx, t.ny, t.nw, t.nh, t.embedding, t.verdict, t.personID, t.updatedAt); err != nil {
			return err
		}
	}
	for id, p := range u.clusters {
		if _, err := tx.Exec("UPDATE face_clusters SET person_id = ? WHERE id = ?", p, id); err != nil {
			return err
		}
	}
	for _, p := range u.tagPaths {
		if err := linkTagsTx(tx, p); err != nil {
			return err
		}
	}
	// Undo touches people/clusters broadly enough that a full re-derive is
	// the safe choice; it is one UPDATE.
	if _, err := tx.Exec("DELETE FROM face_tag_links WHERE tag_id NOT IN (SELECT id FROM face_tags)"); err != nil {
		return err
	}
	if _, err := tx.Exec(rederiveSQL); err != nil {
		return err
	}
	return tx.Commit()
}

func itoa(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = digits[v%10]
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func (i *Index) clusterExists(id int64) bool {
	var one int
	return i.db.QueryRow("SELECT 1 FROM face_clusters WHERE id = ?", id).Scan(&one) == nil
}
