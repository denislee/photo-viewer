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

// TagHint is what the durable tags already say about one detection, so the
// face pipeline can honour them while assigning (before the face row exists).
type TagHint struct {
	Confirmed int64   // confirmed person, 0 if none
	Ignored   bool    // the user said "not a face"
	Rejected  []int64 // people this face is NOT
}

// PreMatchTags matches path's tags against detections that are about to be
// written (boxes normalised, embeddings) with the same rule linkTagsTx uses
// afterwards, and returns one hint per detection.
func (i *Index) PreMatchTags(path string, boxes [][4]float64, embs [][]float32) ([]TagHint, error) {
	hints := make([]TagHint, len(boxes))
	rows, err := i.db.Query(`SELECT nx, ny, nw, nh, embedding, verdict, person_id FROM face_tags
		WHERE path = ? ORDER BY updated_at, id`, path)
	if err != nil {
		return hints, err
	}
	defer rows.Close()
	faces := make([]linkFace, len(boxes))
	for k := range boxes {
		faces[k] = linkFace{id: int64(k + 1), box: boxes[k], emb: embs[k]}
	}
	for rows.Next() {
		var box [4]float64
		var blob []byte
		var verdict string
		var pid sql.NullInt64
		if err := rows.Scan(&box[0], &box[1], &box[2], &box[3], &blob, &verdict, &pid); err != nil {
			return hints, err
		}
		k := matchTag(box, DecodeEmbedding(blob), faces) - 1
		if k < 0 {
			continue
		}
		switch Verdict(verdict) {
		case VerdictConfirmed: // later rows win (ordered by updated_at)
			hints[k].Confirmed, hints[k].Ignored = pid.Int64, false
		case VerdictIgnored:
			hints[k].Confirmed, hints[k].Ignored = 0, true
		case VerdictRejected:
			hints[k].Rejected = append(hints[k].Rejected, pid.Int64)
		}
	}
	return hints, rows.Err()
}

// ReassignFace moves one face to another cluster (toCluster > 0), to a new
// singleton cluster (toCluster == 0, named newPerson when > 0), or out of
// clustering (toCluster < 0), keeping both running-mean centroids exact.
// Returns the face's new cluster id (0 when unclustered).
func (i *Index) ReassignFace(faceID, toCluster, newPerson int64) (int64, error) {
	tx, err := i.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var path string
	var from sql.NullInt64
	var blob []byte
	if err := tx.QueryRow("SELECT path, cluster_id, embedding FROM faces WHERE id = ?", faceID).Scan(&path, &from, &blob); err != nil {
		return 0, err
	}
	emb := DecodeEmbedding(blob)
	if from.Valid && from.Int64 == toCluster {
		return toCluster, nil
	}
	if from.Valid {
		var cb []byte
		var n int
		err := tx.QueryRow("SELECT centroid, (SELECT COUNT(*) FROM faces WHERE cluster_id = ?) FROM face_clusters WHERE id = ?",
			from.Int64, from.Int64).Scan(&cb, &n)
		if err == nil {
			if n <= 1 {
				// Its last member is leaving: drop the empty cluster.
				if _, err := tx.Exec("DELETE FROM face_clusters WHERE id = ?", from.Int64); err != nil {
					return 0, err
				}
			} else if _, err := tx.Exec("UPDATE face_clusters SET centroid = ?, sample_face_id = CASE WHEN sample_face_id = ? THEN NULL ELSE sample_face_id END WHERE id = ?",
				EncodeEmbedding(CentroidWithout(DecodeEmbedding(cb), emb, n)), faceID, from.Int64); err != nil {
				return 0, err
			}
		} else if err != sql.ErrNoRows {
			return 0, err
		}
	}
	var dest any
	switch {
	case toCluster > 0:
		var cb []byte
		var n int
		if err := tx.QueryRow("SELECT centroid, (SELECT COUNT(*) FROM faces WHERE cluster_id = ?) FROM face_clusters WHERE id = ?",
			toCluster, toCluster).Scan(&cb, &n); err != nil {
			return 0, err
		}
		if _, err := tx.Exec("UPDATE face_clusters SET centroid = ? WHERE id = ?",
			EncodeEmbedding(updatedCentroid(DecodeEmbedding(cb), emb, n)), toCluster); err != nil {
			return 0, err
		}
		dest = toCluster
	case toCluster == 0:
		var pid any
		if newPerson > 0 {
			pid = newPerson
		}
		res, err := tx.Exec("INSERT INTO face_clusters (centroid, sample_face_id, person_id) VALUES (?, ?, ?)", blob, faceID, pid)
		if err != nil {
			return 0, err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, err
		}
		dest, toCluster = id, id
	default:
		toCluster = 0
	}
	if _, err := tx.Exec("UPDATE faces SET cluster_id = ? WHERE id = ?", dest, faceID); err != nil {
		return 0, err
	}
	if err := rederivePathsTx(tx, path); err != nil {
		return 0, err
	}
	return toCluster, tx.Commit()
}

// updatedCentroid mirrors face.UpdatedCentroid (cache cannot import face).
func updatedCentroid(centroid, v []float32, count int) []float32 {
	if count <= 0 || len(centroid) != len(v) {
		return append([]float32(nil), v...)
	}
	out := make([]float32, len(centroid))
	n := float32(count)
	for k := range centroid {
		out[k] = (centroid[k]*n + v[k]) / (n + 1)
	}
	return out
}
