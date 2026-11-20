package cache

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
)

// Face is one face detection stored in the index.
type Face struct {
	ID         int64
	Path       string
	ThumbMtime int64
	BBox       [4]int // x, y, w, h in thumbnail pixel space
	Embedding  []float32
	ClusterID  sql.NullInt64
}

// Cluster is a group of faces that share an identity.
type Cluster struct {
	ID           int64
	Label        sql.NullString
	Centroid     []float32
	SampleFaceID sql.NullInt64
	Count        int
}

// EncodeEmbedding packs a float32 vector into a little-endian BLOB. 128 floats
// produce 512 bytes; we don't validate the length so callers can experiment
// with different embedding sizes.
func EncodeEmbedding(v []float32) []byte {
	buf := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// DecodeEmbedding reverses EncodeEmbedding. Returns nil if the byte length is
// not a multiple of 4.
func DecodeEmbedding(b []byte) []float32 {
	if len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// LoadFaceFreshness returns, per path, the thumbnail mtime face detection last
// ran against (entries.face_thumb_mtime) — for paths with and without faces
// (F5-01). The face pipeline pre-loads this on start so its per-job freshness
// checks are answered from memory rather than one query per submit.
func (i *Index) LoadFaceFreshness() map[string]int64 {
	rows, err := i.db.Query("SELECT path, face_thumb_mtime FROM entries WHERE face_thumb_mtime IS NOT NULL")
	if err != nil {
		return map[string]int64{}
	}
	defer rows.Close()
	out := make(map[string]int64, 1024)
	for rows.Next() {
		var p string
		var mt int64
		if err := rows.Scan(&p, &mt); err == nil {
			out[p] = mt
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("cache: LoadFaceFreshness: %v", err)
	}
	return out
}

// FaceOp is one face write decided by the caller. Exactly one of
// ExistingClusterID or NewClusterCentroid is meaningful per op:
//   - ExistingClusterID > 0 → assign the face to that cluster and replace its
//     centroid with UpdatedCentroid.
//   - ExistingClusterID == 0 → create a new cluster seeded by
//     NewClusterCentroid, with sample_face_id pointing at the new face.
type FaceOp struct {
	Face               Face
	ExistingClusterID  int64
	UpdatedCentroid    []float32
	NewClusterCentroid []float32
}

// FaceOpResult parallels the input slice from WriteFacesForPath.
type FaceOpResult struct {
	FaceID    int64
	ClusterID int64
}

// faceStatements lazily prepares the long-lived statements used by
// WriteFacesForPath. The statements are reused for the life of the *Index; each
// face-detection scan would otherwise reprepare them per call.
func (i *Index) faceStatements() (del, ins, updCluster, newCluster, setCluster *sql.Stmt, err error) {
	i.stmtMu.Lock()
	defer i.stmtMu.Unlock()
	if i.faceDelStmt == nil {
		if i.faceDelStmt, err = i.db.Prepare(
			"DELETE FROM faces WHERE path = ?",
		); err != nil {
			return
		}
	}
	if i.faceInsStmt == nil {
		if i.faceInsStmt, err = i.db.Prepare(
			`INSERT INTO faces (path, thumb_mtime, bbox_x, bbox_y, bbox_w, bbox_h, embedding, cluster_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		); err != nil {
			return
		}
	}
	if i.faceUpdClusterStmt == nil {
		if i.faceUpdClusterStmt, err = i.db.Prepare(
			"UPDATE face_clusters SET centroid = ? WHERE id = ?",
		); err != nil {
			return
		}
	}
	if i.faceNewClusterStmt == nil {
		if i.faceNewClusterStmt, err = i.db.Prepare(
			"INSERT INTO face_clusters (centroid, sample_face_id) VALUES (?, ?)",
		); err != nil {
			return
		}
	}
	if i.faceSetClusterStmt == nil {
		if i.faceSetClusterStmt, err = i.db.Prepare(
			"UPDATE faces SET cluster_id = ? WHERE id = ?",
		); err != nil {
			return
		}
	}
	return i.faceDelStmt, i.faceInsStmt, i.faceUpdClusterStmt, i.faceNewClusterStmt, i.faceSetClusterStmt, nil
}

// WriteFacesForPath wipes existing face rows for path and writes the given
// ops in a single transaction (1 fsync per image instead of ~3 per face).
// thumbMtime, the mtime of the thumbnail detection ran on, is recorded on the
// entries row in the same transaction, so an image with no faces (nil ops) is
// remembered as processed too and not re-detected next run (F5-01).
// Callers (the face pipeline) are expected to have already chosen
// existing-cluster vs. new-cluster against an in-memory cache.
//
// Every op's Face.Path must equal path: the wipe is keyed on path, so a face
// inserted under any other path would survive the next rewrite of path and
// orphan (or clobber another file's faces). A mismatch is rejected up front,
// before anything is written.
func (i *Index) WriteFacesForPath(path string, thumbMtime int64, ops []FaceOp) ([]FaceOpResult, error) {
	for k, op := range ops {
		if op.Face.Path != path {
			return nil, fmt.Errorf("WriteFacesForPath(%q): op %d has Face.Path %q", path, k, op.Face.Path)
		}
	}
	delStmt, insStmt, updClusterStmt, newClusterStmt, setClusterStmt, err := i.faceStatements()
	if err != nil {
		return nil, err
	}

	tx, err := i.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	del := tx.Stmt(delStmt)
	defer del.Close()
	if _, err := del.Exec(path); err != nil {
		return nil, err
	}

	ins := tx.Stmt(insStmt)
	defer ins.Close()
	updCluster := tx.Stmt(updClusterStmt)
	defer updCluster.Close()
	newCluster := tx.Stmt(newClusterStmt)
	defer newCluster.Close()
	setCluster := tx.Stmt(setClusterStmt)
	defer setCluster.Close()

	results := make([]FaceOpResult, len(ops))
	for k, op := range ops {
		f := op.Face
		// A face joining an existing cluster already knows its cluster id, so
		// it is written with a single INSERT carrying cluster_id. Only a new
		// cluster needs the face id first — it becomes the cluster's
		// sample_face_id — which is why that path inserts the face unclustered,
		// creates the cluster, then points the face at it.
		if op.ExistingClusterID > 0 {
			clusterID := op.ExistingClusterID
			if _, err := updCluster.Exec(EncodeEmbedding(op.UpdatedCentroid), clusterID); err != nil {
				return nil, err
			}
			res, err := ins.Exec(
				f.Path, f.ThumbMtime, f.BBox[0], f.BBox[1], f.BBox[2], f.BBox[3], EncodeEmbedding(f.Embedding), clusterID,
			)
			if err != nil {
				return nil, err
			}
			faceID, err := res.LastInsertId()
			if err != nil {
				return nil, err
			}
			results[k] = FaceOpResult{FaceID: faceID, ClusterID: clusterID}
			continue
		}

		res, err := ins.Exec(
			f.Path, f.ThumbMtime, f.BBox[0], f.BBox[1], f.BBox[2], f.BBox[3], EncodeEmbedding(f.Embedding), nil,
		)
		if err != nil {
			return nil, err
		}
		faceID, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}

		cres, err := newCluster.Exec(EncodeEmbedding(op.NewClusterCentroid), faceID)
		if err != nil {
			return nil, err
		}
		clusterID, err := cres.LastInsertId()
		if err != nil {
			return nil, err
		}
		if _, err := setCluster.Exec(clusterID, faceID); err != nil {
			return nil, err
		}
		results[k] = FaceOpResult{FaceID: faceID, ClusterID: clusterID}
	}

	if _, err := tx.Exec("UPDATE entries SET face_thumb_mtime = ? WHERE path = ?", thumbMtime, path); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

// MoveFaces relocates any face rows from oldPath to newPath after a media file
// has been renamed on disk (the organize move flow). Because faces are keyed by
// path, the moved file would otherwise have no rows under newPath — forcing the
// pipeline to re-detect it from scratch (the exact cost the thumbnail carry-over
// in ApplyMove avoids) while the old rows orphan. The embeddings and thumb_mtime
// stay valid because the thumbnail file was renamed, not regenerated.
//
// The face-detection freshness (entries.face_thumb_mtime) moves too: it is
// taken from the old row when there is one, else from the moved face rows, so
// the relocated file isn't re-detected (F5-01).
func (i *Index) MoveFaces(oldPath, newPath string) error {
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE faces SET path = ? WHERE path = ?", newPath, oldPath); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE entries SET face_thumb_mtime = COALESCE(
			(SELECT face_thumb_mtime FROM entries WHERE path = ?),
			(SELECT MAX(thumb_mtime) FROM faces WHERE path = ?),
			face_thumb_mtime)
		WHERE path = ?`, oldPath, newPath, newPath); err != nil {
		return err
	}
	// The old path's faces are gone, so it no longer counts as scanned.
	if _, err := tx.Exec("UPDATE entries SET face_thumb_mtime = NULL WHERE path = ?", oldPath); err != nil {
		return err
	}
	return tx.Commit()
}

// AllClusters returns every cluster with its current face count.
//
// The face count comes from one GROUP BY scan over faces joined onto the
// clusters table rather than a correlated subquery per row, so the cost
// stays O(faces) instead of O(clusters × faces).
func (i *Index) AllClusters() []Cluster {
	rows, err := i.db.Query(`
		SELECT c.id, c.label, c.centroid, c.sample_face_id,
			COALESCE(f.cnt, 0) AS cnt
		FROM face_clusters c
		LEFT JOIN (
			SELECT cluster_id, COUNT(*) AS cnt
			FROM faces
			WHERE cluster_id IS NOT NULL
			GROUP BY cluster_id
		) f ON f.cluster_id = c.id
		ORDER BY cnt DESC, c.id ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Cluster
	for rows.Next() {
		var c Cluster
		var blob []byte
		if err := rows.Scan(&c.ID, &c.Label, &blob, &c.SampleFaceID, &c.Count); err == nil {
			c.Centroid = DecodeEmbedding(blob)
			out = append(out, c)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("cache: AllClusters: %v", err)
	}
	return out
}

// The four cluster read/write APIs below (RenameCluster, PathsInCluster,
// SampleFace, MergeClusters) back the not-yet-built People sidebar. No caller
// exists yet, but the face detection + clustering pipeline is actively
// maintained and these are its intended read surface, so they are retained
// deliberately rather than deleted (see C-04).

// RenameCluster updates the user-visible label for a cluster.
func (i *Index) RenameCluster(clusterID int64, label string) error {
	_, err := i.db.Exec("UPDATE face_clusters SET label = ? WHERE id = ?", label, clusterID)
	return err
}

// PathsInCluster returns the distinct entry paths whose faces belong to a cluster.
func (i *Index) PathsInCluster(clusterID int64) []string {
	rows, err := i.db.Query(
		"SELECT DISTINCT path FROM faces WHERE cluster_id = ? ORDER BY path",
		clusterID,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("cache: PathsInCluster: %v", err)
	}
	return out
}

// SampleFace returns the face row used as a cluster's preview thumbnail.
func (i *Index) SampleFace(faceID int64) (Face, bool) {
	var f Face
	var blob []byte
	err := i.db.QueryRow(
		"SELECT id, path, thumb_mtime, bbox_x, bbox_y, bbox_w, bbox_h, embedding, cluster_id FROM faces WHERE id = ?",
		faceID,
	).Scan(&f.ID, &f.Path, &f.ThumbMtime, &f.BBox[0], &f.BBox[1], &f.BBox[2], &f.BBox[3], &blob, &f.ClusterID)
	if err != nil {
		return Face{}, false
	}
	f.Embedding = DecodeEmbedding(blob)
	return f, true
}

// MergeClusters reassigns every face from src into dst, recomputes dst's
// centroid as the count-weighted mean, and deletes the now-empty src cluster
// in a single transaction.
func (i *Index) MergeClusters(srcID, dstID int64) error {
	if srcID == dstID {
		return nil
	}
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var srcCentroid, dstCentroid []byte
	var srcCount, dstCount int
	if err := tx.QueryRow(
		"SELECT centroid, (SELECT COUNT(*) FROM faces WHERE cluster_id = ?) FROM face_clusters WHERE id = ?",
		srcID, srcID,
	).Scan(&srcCentroid, &srcCount); err != nil {
		return err
	}
	if err := tx.QueryRow(
		"SELECT centroid, (SELECT COUNT(*) FROM faces WHERE cluster_id = ?) FROM face_clusters WHERE id = ?",
		dstID, dstID,
	).Scan(&dstCentroid, &dstCount); err != nil {
		return err
	}

	merged := mergeCentroids(DecodeEmbedding(dstCentroid), dstCount, DecodeEmbedding(srcCentroid), srcCount)
	if _, err := tx.Exec("UPDATE faces SET cluster_id = ? WHERE cluster_id = ?", dstID, srcID); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE face_clusters SET centroid = ? WHERE id = ?", EncodeEmbedding(merged), dstID); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM face_clusters WHERE id = ?", srcID); err != nil {
		return err
	}
	return tx.Commit()
}

func mergeCentroids(a []float32, na int, b []float32, nb int) []float32 {
	if len(a) == 0 {
		return b
	}
	if len(b) != len(a) {
		return a
	}
	total := float32(na + nb)
	if total == 0 {
		return a
	}
	out := make([]float32, len(a))
	for i := range a {
		out[i] = (a[i]*float32(na) + b[i]*float32(nb)) / total
	}
	return out
}

// GetEntry returns the index row for path, or (Entry{}, false) if unknown.
func (i *Index) GetEntry(path string) (Entry, bool) {
	e, err := scanEntry(i.db.QueryRow(
		entrySelect+" WHERE path = ?",
		path,
	))
	if err != nil {
		return Entry{}, false
	}
	return e, true
}

// GetEntryByThumbID returns the index row whose thumb_id matches id.
// Used by the webserver to look up media by an opaque ID without exposing
// filesystem paths to clients.
func (i *Index) GetEntryByThumbID(id string) (Entry, bool) {
	e, err := scanEntry(i.db.QueryRow(
		entrySelect+" WHERE thumb_id = ? LIMIT 1",
		id,
	))
	if err != nil {
		return Entry{}, false
	}
	return e, true
}
