package cache

import (
	"database/sql"
	"math"
)

// Re-attaching durable tags to (re-)detected faces (plan §3.4).
//
// A tag is identified by (path, normalised bbox, embedding), never by
// faces.id, which changes on every re-detection. linkTagsTx matches each tag
// of a path to the best current face: highest bbox IoU (>= TagMinIoU), else
// the nearest embedding (< TagMaxSqDistance — the same face in the same
// image). A tag that matches nothing stays dormant; the face may come back on
// the next detector run.
const (
	TagMinIoU        = 0.5
	TagMaxSqDistance = 0.05
)

type linkFace struct {
	id   int64
	box  [4]float64
	emb  []float32
	used bool
}

// linkTagsTx rebuilds face_tag_links for path's faces and re-derives them.
func linkTagsTx(tx *sql.Tx, path string) error {
	if _, err := tx.Exec("DELETE FROM face_tag_links WHERE face_id IN (SELECT id FROM faces WHERE path = ?)", path); err != nil {
		return err
	}
	type tag struct {
		id  int64
		box [4]float64
		emb []float32
	}
	var tags []tag
	rows, err := tx.Query("SELECT id, nx, ny, nw, nh, embedding FROM face_tags WHERE path = ? ORDER BY id", path)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t tag
		var blob []byte
		if err := rows.Scan(&t.id, &t.box[0], &t.box[1], &t.box[2], &t.box[3], &blob); err != nil {
			rows.Close()
			return err
		}
		t.emb = DecodeEmbedding(blob)
		tags = append(tags, t)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(tags) > 0 {
		var faces []linkFace
		rows, err := tx.Query("SELECT id, nx, ny, nw, nh, embedding FROM faces WHERE path = ? ORDER BY id", path)
		if err != nil {
			return err
		}
		for rows.Next() {
			var f linkFace
			var nx, ny, nw, nh sql.NullFloat64
			var blob []byte
			if err := rows.Scan(&f.id, &nx, &ny, &nw, &nh, &blob); err != nil {
				rows.Close()
				return err
			}
			f.box = NoNBox
			if nx.Valid && ny.Valid && nw.Valid && nh.Valid {
				f.box = [4]float64{nx.Float64, ny.Float64, nw.Float64, nh.Float64}
			}
			f.emb = DecodeEmbedding(blob)
			faces = append(faces, f)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, t := range tags {
			if fid := matchTag(t.box, t.emb, faces); fid > 0 {
				if _, err := tx.Exec("INSERT OR IGNORE INTO face_tag_links (face_id, tag_id) VALUES (?, ?)", fid, t.id); err != nil {
					return err
				}
			}
		}
	}
	return rederivePathsTx(tx, path)
}

// matchTag picks the face a tag belongs to, or 0.
func matchTag(box [4]float64, emb []float32, faces []linkFace) int64 {
	best, bestIoU := int64(0), TagMinIoU
	for _, f := range faces {
		if s := NBoxIoU(box, f.box); s >= bestIoU {
			best, bestIoU = f.id, s
		}
	}
	if best > 0 {
		return best
	}
	bestD := float64(TagMaxSqDistance)
	for _, f := range faces {
		if d := sqDist(emb, f.emb); d < bestD {
			best, bestD = f.id, d
		}
	}
	if math.IsInf(bestD, 1) {
		return 0
	}
	return best
}

// MoveTags re-keys the durable tags of oldPath to newPath. Used when a trash
// restore had to pick a new name ("x (restored 1).jpg"); MovePath/MoveFaces
// already carry tags along on organize moves.
func (i *Index) MoveTags(oldPath, newPath string) error {
	if oldPath == newPath {
		return nil
	}
	_, err := i.db.Exec("UPDATE face_tags SET path = ? WHERE path = ?", newPath, oldPath)
	return err
}

// SweepOrphanTags deletes tags whose path is neither indexed nor waiting in
// the trash (keep reports original paths of trashed files). Deletes keep
// tags so a restore brings names back; this low-priority sweep runs after a
// rebuild's rescan and reaps the rest. Returns the number of tags removed.
func (i *Index) SweepOrphanTags(keep map[string]bool) (int, error) {
	tx, err := i.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	paths, err := queryStrings(tx, "SELECT DISTINCT path FROM face_tags WHERE path NOT IN (SELECT path FROM entries)")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range paths {
		if keep[p] {
			continue
		}
		res, err := tx.Exec("DELETE FROM face_tags WHERE path = ?", p)
		if err != nil {
			return 0, err
		}
		k, _ := res.RowsAffected()
		n += int(k)
	}
	// A person whose cover pointed at a reaped file falls back to the best
	// remaining face (coverFace), so the cover columns can stay.
	return n, tx.Commit()
}
