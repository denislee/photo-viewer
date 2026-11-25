package cache

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// People and face tags: the durable, user-owned layer of the face data
// (schema v9, see migrateV9). Every write here runs in one transaction and
// leaves faces.person_id / faces.assign consistent with the derivation rule:
//
//  1. a face with a linked confirmed tag is that person (assign 'user');
//  2. a face with a linked ignored tag is nobody (assign 'ignored');
//  3. otherwise it follows its cluster's person (assign 'cluster'), unless the
//     face has a linked rejection of exactly that person, then nobody.
//
// Callers that also hold an in-memory cluster cache (face.Pipeline) must run
// these through Pipeline.Mutate so the cache is dropped afterwards.

// Verdict is a user decision about one face.
type Verdict string

const (
	VerdictConfirmed Verdict = "confirmed" // "this is P"
	VerdictRejected  Verdict = "rejected"  // "this is NOT P"
	VerdictIgnored   Verdict = "ignored"   // "not a face / don't care"
	// VerdictNone clears the face's confirmed/ignored decision (rejections
	// stay). It is never stored.
	VerdictNone Verdict = ""
)

// Face assignment states (faces.assign).
const (
	AssignCluster = "cluster"
	AssignUser    = "user"
	AssignIgnored = "ignored"
)

// ErrNameExists is returned by CreatePerson/RenamePerson when another person
// already has the name (case-insensitive). ID is that person, so the UI can
// offer to merge instead.
type ErrNameExists struct {
	ID   int64
	Name string
}

func (e *ErrNameExists) Error() string {
	return fmt.Sprintf("a person named %q already exists", e.Name)
}

// ErrEmptyName rejects blank person names.
var ErrEmptyName = errors.New("person name is empty")

// Person is one row of the people overview.
type Person struct {
	ID     int64
	Name   string
	Hidden bool
	Photos int // distinct files with at least one face of this person
	Faces  int
	// Cover is the face shown as the avatar: the user-chosen cover when it
	// still resolves, else the best face available. ID == 0 when none.
	Cover Face
}

// Group is an unnamed cluster (face_clusters.person_id IS NULL).
type Group struct {
	ClusterID int64
	Faces     int
	Photos    int
	Sample    Face
}

// FaceInfo is a face plus the identity and ordering data the UI needs.
type FaceInfo struct {
	Face
	PersonName string
	// Dist is the squared-Euclidean distance to the centroid of the set it
	// was listed from (FacesOfPerson/FacesOfCluster); 0 elsewhere.
	Dist float64
}

// FaceOrder selects how FacesOfPerson/FacesOfCluster sort their result.
type FaceOrder int

const (
	// OrderOutlierFirst sorts by distance from the centroid, farthest first:
	// wrong faces are the outliers, so they land in the first row.
	OrderOutlierFirst FaceOrder = iota
	// OrderPath sorts by file path, then face id.
	OrderPath
)

// faceCols is the column list scanFace reads.
const faceCols = `f.id, f.path, f.thumb_mtime, f.bbox_x, f.bbox_y, f.bbox_w, f.bbox_h,
	f.embedding, f.cluster_id, f.person_id, f.assign, f.quality,
	f.nx, f.ny, f.nw, f.nh`

func scanFace(r rowScanner, f *Face) error {
	var blob []byte
	var q, nx, ny, nw, nh sql.NullFloat64
	if err := r.Scan(&f.ID, &f.Path, &f.ThumbMtime, &f.BBox[0], &f.BBox[1], &f.BBox[2], &f.BBox[3],
		&blob, &f.ClusterID, &f.PersonID, &f.Assign, &q, &nx, &ny, &nw, &nh); err != nil {
		return err
	}
	f.Embedding = DecodeEmbedding(blob)
	f.Quality = q.Float64
	if nx.Valid && ny.Valid && nw.Valid && nh.Valid {
		f.NBox = [4]float64{nx.Float64, ny.Float64, nw.Float64, nh.Float64}
	} else {
		f.NBox = NoNBox
	}
	return nil
}

func normName(name string) string { return strings.Join(strings.Fields(name), " ") }

// personByNameTx returns the id of the person called name (case-insensitive).
func personByNameTx(q interface {
	QueryRow(string, ...any) *sql.Row
}, name string) (int64, bool) {
	var id int64
	if err := q.QueryRow("SELECT id FROM people WHERE name = ? COLLATE NOCASE", name).Scan(&id); err != nil {
		return 0, false
	}
	return id, true
}

// CreatePerson adds a person. A taken name returns *ErrNameExists.
func (i *Index) CreatePerson(name string) (int64, error) {
	name = normName(name)
	if name == "" {
		return 0, ErrEmptyName
	}
	if id, ok := personByNameTx(i.db, name); ok {
		return 0, &ErrNameExists{ID: id, Name: name}
	}
	res, err := i.db.Exec("INSERT INTO people (name, created_at) VALUES (?, ?)", name, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PersonByName returns the person called name (case-insensitive).
func (i *Index) PersonByName(name string) (int64, bool) {
	return personByNameTx(i.db, normName(name))
}

// PersonName returns a person's name.
func (i *Index) PersonName(id int64) (string, bool) {
	var n string
	if err := i.db.QueryRow("SELECT name FROM people WHERE id = ?", id).Scan(&n); err != nil {
		return "", false
	}
	return n, true
}

// RenamePerson renames a person. A name held by a different person returns
// *ErrNameExists (call MergePeople to combine them); a case-only change of
// the person's own name is allowed.
func (i *Index) RenamePerson(id int64, name string) (*Undo, error) {
	name = normName(name)
	if name == "" {
		return nil, ErrEmptyName
	}
	if other, ok := personByNameTx(i.db, name); ok && other != id {
		return nil, &ErrNameExists{ID: other, Name: name}
	}
	return i.peopleTx("Rename person", func(tx *sql.Tx, u *undoBuilder) error {
		if err := u.person(tx, id); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE people SET name = ? WHERE id = ?", name, id)
		return err
	})
}

// HidePerson sets the "don't show in People" flag.
func (i *Index) HidePerson(id int64, hidden bool) (*Undo, error) {
	return i.peopleTx("Hide person", func(tx *sql.Tx, u *undoBuilder) error {
		if err := u.person(tx, id); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE people SET hidden = ? WHERE id = ?", boolInt(hidden), id)
		return err
	})
}

// SetCover makes faceID the avatar of personID. The cover is stored by its
// durable key (path + normalised bbox), so it survives re-detection.
func (i *Index) SetCover(personID, faceID int64) (*Undo, error) {
	return i.peopleTx("Set cover", func(tx *sql.Tx, u *undoBuilder) error {
		if err := u.person(tx, personID); err != nil {
			return err
		}
		f, err := faceTx(tx, faceID)
		if err != nil {
			return err
		}
		_, err = tx.Exec("UPDATE people SET cover_path = ?, cover_bbox = ? WHERE id = ?",
			f.Path, formatNBox(f.NBox), personID)
		return err
	})
}

// MergePeople folds src into dst: every cluster and tag of src moves to dst
// and src is deleted. A rejection of src on a face dst has confirmed is
// dropped (they are now the same person, so the two decisions conflict).
func (i *Index) MergePeople(srcID, dstID int64) (*Undo, error) {
	if srcID == dstID {
		return nil, nil
	}
	return i.peopleTx("Merge people", func(tx *sql.Tx, u *undoBuilder) error {
		if err := u.person(tx, srcID); err != nil {
			return err
		}
		if err := u.person(tx, dstID); err != nil {
			return err
		}
		if err := u.clustersWhere(tx, "person_id IN (?, ?)", srcID, dstID); err != nil {
			return err
		}
		if err := u.tagPathsWhere(tx, "person_id IN (?, ?)", srcID, dstID); err != nil {
			return err
		}
		for _, q := range []string{
			"UPDATE face_clusters SET person_id = ? WHERE person_id = ?",
			"UPDATE face_tags SET person_id = ? WHERE person_id = ?",
			"UPDATE faces SET person_id = ? WHERE person_id = ?",
		} {
			if _, err := tx.Exec(q, dstID, srcID); err != nil {
				return err
			}
		}
		// Drop rejections of dst on faces that also carry a confirmed(dst).
		if _, err := tx.Exec(`DELETE FROM face_tags WHERE verdict = 'rejected' AND person_id = ?
			AND id IN (SELECT l.tag_id FROM face_tag_links l WHERE l.face_id IN (
				SELECT l2.face_id FROM face_tag_links l2 JOIN face_tags t2 ON t2.id = l2.tag_id
				WHERE t2.verdict = 'confirmed' AND t2.person_id = ?))`, dstID, dstID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE people SET
				cover_path = COALESCE(cover_path, (SELECT cover_path FROM people WHERE id = ?)),
				cover_bbox = COALESCE(cover_bbox, (SELECT cover_bbox FROM people WHERE id = ?))
			WHERE id = ?`, srcID, srcID, dstID); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM people WHERE id = ?", srcID); err != nil {
			return err
		}
		return u.affectAll()
	})
}

// DeletePerson removes a person and every tag naming them; their clusters go
// back to unnamed.
func (i *Index) DeletePerson(id int64) (*Undo, error) {
	return i.peopleTx("Delete person", func(tx *sql.Tx, u *undoBuilder) error {
		if err := u.person(tx, id); err != nil {
			return err
		}
		if err := u.clustersWhere(tx, "person_id = ?", id); err != nil {
			return err
		}
		if err := u.tagPathsWhere(tx, "person_id = ?", id); err != nil {
			return err
		}
		for _, q := range []string{
			"UPDATE face_clusters SET person_id = NULL WHERE person_id = ?",
			"DELETE FROM face_tags WHERE person_id = ?",
			"DELETE FROM people WHERE id = ?",
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return err
			}
		}
		return u.affectAll()
	})
}

// AssignClusterToPerson names a group: the cluster's faces that follow their
// cluster now resolve to personID (0 unnames it). It deliberately does not
// confirm the faces — they stay attributed, not confirmed, so a regroup can
// still move them.
func (i *Index) AssignClusterToPerson(clusterID, personID int64) (*Undo, error) {
	return i.peopleTx("Name group", func(tx *sql.Tx, u *undoBuilder) error {
		if err := u.clustersWhere(tx, "id = ?", clusterID); err != nil {
			return err
		}
		var pid any
		if personID > 0 {
			pid = personID
		}
		if _, err := tx.Exec("UPDATE face_clusters SET person_id = ? WHERE id = ?", pid, clusterID); err != nil {
			return err
		}
		u.affectClusters(clusterID)
		return nil
	})
}

// NamePersonFromCluster is the "type a name on a group" action: an existing
// name (case-insensitive) assigns the cluster to that person, a new one
// creates the person first. Returns the person id.
func (i *Index) NamePersonFromCluster(clusterID int64, name string) (int64, *Undo, error) {
	name = normName(name)
	if name == "" {
		return 0, nil, ErrEmptyName
	}
	var pid int64
	u, err := i.peopleTx("Name group", func(tx *sql.Tx, u *undoBuilder) error {
		if id, ok := personByNameTx(tx, name); ok {
			pid = id
		} else {
			res, err := tx.Exec("INSERT INTO people (name, created_at) VALUES (?, ?)", name, time.Now().Unix())
			if err != nil {
				return err
			}
			if pid, err = res.LastInsertId(); err != nil {
				return err
			}
			u.created = append(u.created, pid)
		}
		if err := u.clustersWhere(tx, "id = ?", clusterID); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE face_clusters SET person_id = ? WHERE id = ?", pid, clusterID); err != nil {
			return err
		}
		u.affectClusters(clusterID)
		return nil
	})
	return pid, u, err
}

// TagFaces records one user decision for each face. personID is required for
// confirmed/rejected and ignored otherwise.
//
//   - confirmed(P): replaces the face's confirmed/ignored decision and drops a
//     rejection of P.
//   - rejected(P): drops a confirmed(P) and adds the rejection.
//   - ignored: replaces the confirmed/ignored decision (rejections stay).
//   - VerdictNone: drops the confirmed/ignored decision.
func (i *Index) TagFaces(faceIDs []int64, verdict Verdict, personID int64) (*Undo, error) {
	switch verdict {
	case VerdictConfirmed, VerdictRejected:
		if personID <= 0 {
			return nil, fmt.Errorf("TagFaces(%s): person id required", verdict)
		}
	case VerdictIgnored, VerdictNone:
		personID = 0
	default:
		return nil, fmt.Errorf("TagFaces: unknown verdict %q", verdict)
	}
	label := map[Verdict]string{
		VerdictConfirmed: "Confirm", VerdictRejected: "Not this person",
		VerdictIgnored: "Ignore", VerdictNone: "Clear decision",
	}[verdict]
	return i.peopleTx(label, func(tx *sql.Tx, u *undoBuilder) error {
		now := time.Now().UnixNano()
		for _, id := range faceIDs {
			f, err := faceTx(tx, id)
			if err != nil {
				return err
			}
			if err := u.tagPath(tx, f.Path); err != nil {
				return err
			}
			// Existing tags currently attached to this face.
			var drop string
			var args []any
			switch verdict {
			case VerdictConfirmed:
				drop = "(t.verdict IN ('confirmed','ignored') OR (t.verdict = 'rejected' AND t.person_id = ?))"
				args = []any{personID}
			case VerdictRejected:
				drop = "(t.person_id = ? AND t.verdict IN ('confirmed','rejected'))"
				args = []any{personID}
			default:
				drop = "t.verdict IN ('confirmed','ignored')"
			}
			if _, err := tx.Exec(`DELETE FROM face_tags WHERE id IN (
					SELECT t.id FROM face_tags t JOIN face_tag_links l ON l.tag_id = t.id
					WHERE l.face_id = ? AND `+drop+`)`, append([]any{id}, args...)...); err != nil {
				return err
			}
			if verdict == VerdictNone {
				continue
			}
			var pid any
			if personID > 0 {
				pid = personID
			}
			res, err := tx.Exec(`INSERT INTO face_tags (path, nx, ny, nw, nh, embedding, verdict, person_id, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				f.Path, f.NBox[0], f.NBox[1], f.NBox[2], f.NBox[3], EncodeEmbedding(f.Embedding),
				string(verdict), pid, now)
			if err != nil {
				return err
			}
			tagID, err := res.LastInsertId()
			if err != nil {
				return err
			}
			// Link directly: the tag was made from this exact face, so it
			// must not be left to the matcher (two near-identical faces in one
			// photo could otherwise swap).
			if _, err := tx.Exec("INSERT OR IGNORE INTO face_tag_links (face_id, tag_id) VALUES (?, ?)", id, tagID); err != nil {
				return err
			}
			now++
		}
		return nil
	})
}

// faceTx loads one face row.
func faceTx(tx *sql.Tx, id int64) (Face, error) {
	var f Face
	err := scanFace(tx.QueryRow("SELECT "+faceCols+" FROM faces f WHERE f.id = ?", id), &f)
	if err == sql.ErrNoRows {
		return f, fmt.Errorf("face %d not found", id)
	}
	return f, err
}

// Face returns one face row.
func (i *Index) Face(id int64) (Face, bool) {
	var f Face
	if err := scanFace(i.db.QueryRow("SELECT "+faceCols+" FROM faces f WHERE f.id = ?", id), &f); err != nil {
		return Face{}, false
	}
	return f, true
}

// rederiveSQL recomputes person_id/assign from links and clusters (see the
// file comment for the rule). The caller appends a WHERE clause.
const rederiveSQL = `UPDATE faces SET
	assign = CASE
		WHEN EXISTS (SELECT 1 FROM face_tag_links l JOIN face_tags t ON t.id = l.tag_id
			WHERE l.face_id = faces.id AND t.verdict = 'confirmed') THEN 'user'
		WHEN EXISTS (SELECT 1 FROM face_tag_links l JOIN face_tags t ON t.id = l.tag_id
			WHERE l.face_id = faces.id AND t.verdict = 'ignored') THEN 'ignored'
		ELSE 'cluster' END,
	person_id = CASE
		WHEN EXISTS (SELECT 1 FROM face_tag_links l JOIN face_tags t ON t.id = l.tag_id
			WHERE l.face_id = faces.id AND t.verdict = 'confirmed')
		THEN (SELECT t.person_id FROM face_tag_links l JOIN face_tags t ON t.id = l.tag_id
			WHERE l.face_id = faces.id AND t.verdict = 'confirmed'
			ORDER BY t.updated_at DESC, t.id DESC LIMIT 1)
		WHEN EXISTS (SELECT 1 FROM face_tag_links l JOIN face_tags t ON t.id = l.tag_id
			WHERE l.face_id = faces.id AND t.verdict = 'ignored') THEN NULL
		ELSE (SELECT c.person_id FROM face_clusters c WHERE c.id = faces.cluster_id
			AND NOT EXISTS (SELECT 1 FROM face_tag_links l JOIN face_tags t ON t.id = l.tag_id
				WHERE l.face_id = faces.id AND t.verdict = 'rejected' AND t.person_id = c.person_id))
		END `

// rederivePathsTx re-derives every face of the given paths.
func rederivePathsTx(tx *sql.Tx, paths ...string) error {
	for _, p := range paths {
		if _, err := tx.Exec(rederiveSQL+"WHERE path = ?", p); err != nil {
			return err
		}
	}
	return nil
}

// rederiveClustersTx re-derives every face of the given clusters.
func rederiveClustersTx(tx *sql.Tx, ids ...int64) error {
	for _, id := range ids {
		if _, err := tx.Exec(rederiveSQL+"WHERE cluster_id = ?", id); err != nil {
			return err
		}
	}
	return nil
}

// RederiveAll recomputes every face's identity. Used after bulk cluster
// rewrites (regroup).
func (i *Index) RederiveAll() error {
	_, err := i.db.Exec(rederiveSQL)
	return err
}

// ListPeople returns every person with photo/face counts and a cover face,
// sorted by photo count (descending), then name.
func (i *Index) ListPeople() ([]Person, error) {
	rows, err := i.db.Query(`
		SELECT p.id, p.name, p.hidden, p.cover_path, p.cover_bbox,
			COALESCE(s.photos, 0), COALESCE(s.faces, 0)
		FROM people p
		LEFT JOIN (SELECT person_id, COUNT(DISTINCT path) AS photos, COUNT(*) AS faces
			FROM faces WHERE person_id IS NOT NULL GROUP BY person_id) s ON s.person_id = p.id
		ORDER BY 6 DESC, p.name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	type cover struct {
		path sql.NullString
		bbox sql.NullString
	}
	var out []Person
	var covers []cover
	for rows.Next() {
		var p Person
		var c cover
		var hidden int
		if err := rows.Scan(&p.ID, &p.Name, &hidden, &c.path, &c.bbox, &p.Photos, &p.Faces); err != nil {
			rows.Close()
			return nil, err
		}
		p.Hidden = hidden != 0
		out = append(out, p)
		covers = append(covers, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for k := range out {
		out[k].Cover = i.coverFace(out[k].ID, covers[k].path, covers[k].bbox)
	}
	return out, nil
}

// coverFace resolves a person's avatar: the stored cover when a face of the
// person still matches it, else the most recently confirmed face, else the
// highest-quality face.
func (i *Index) coverFace(personID int64, path, bbox sql.NullString) Face {
	if path.Valid {
		want, ok := parseNBox(bbox.String)
		rows, err := i.db.Query("SELECT "+faceCols+" FROM faces f WHERE f.path = ? AND f.person_id = ?", path.String, personID)
		if err == nil {
			var best Face
			bestIoU := -1.0
			for rows.Next() {
				var f Face
				if scanFace(rows, &f) != nil {
					continue
				}
				s := 0.0
				if ok {
					s = NBoxIoU(f.NBox, want)
				}
				if s > bestIoU {
					best, bestIoU = f, s
				}
			}
			rows.Close()
			if best.ID != 0 {
				return best
			}
		}
	}
	var f Face
	err := scanFace(i.db.QueryRow(`SELECT `+faceCols+` FROM faces f
		LEFT JOIN face_tag_links l ON l.face_id = f.id
		LEFT JOIN face_tags t ON t.id = l.tag_id AND t.verdict = 'confirmed'
		WHERE f.person_id = ?
		ORDER BY t.updated_at IS NULL, t.updated_at DESC, f.quality IS NULL, f.quality DESC, f.id
		LIMIT 1`, personID), &f)
	if err != nil {
		return Face{}
	}
	return f
}

// ListGroups returns the unnamed clusters (person_id IS NULL) with at least
// minFaces clusterable faces, largest first.
func (i *Index) ListGroups(minFaces int) ([]Group, error) {
	rows, err := i.db.Query(`
		SELECT c.id, COUNT(f.id), COUNT(DISTINCT f.path),
			COALESCE(c.sample_face_id, MIN(f.id))
		FROM face_clusters c JOIN faces f ON f.cluster_id = c.id AND f.assign = 'cluster' AND f.person_id IS NULL
		WHERE c.person_id IS NULL
		GROUP BY c.id HAVING COUNT(f.id) >= ?
		ORDER BY 2 DESC, c.id`, max(minFaces, 1))
	if err != nil {
		return nil, err
	}
	var out []Group
	var samples []int64
	for rows.Next() {
		var g Group
		var sample int64
		if err := rows.Scan(&g.ClusterID, &g.Faces, &g.Photos, &sample); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, g)
		samples = append(samples, sample)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for k := range out {
		if f, ok := i.Face(samples[k]); ok && f.ClusterID.Int64 == out[k].ClusterID {
			out[k].Sample = f
		} else {
			// sample_face_id went stale (that face moved or was re-detected).
			var f Face
			if scanFace(i.db.QueryRow("SELECT "+faceCols+" FROM faces f WHERE f.cluster_id = ? ORDER BY f.quality IS NULL, f.quality DESC, f.id LIMIT 1",
				out[k].ClusterID), &f) == nil {
				out[k].Sample = f
			}
		}
	}
	return out, nil
}

// CountGroups returns how many unnamed groups have at least minFaces faces.
func (i *Index) CountGroups(minFaces int) int {
	var n int
	_ = i.db.QueryRow(`SELECT COUNT(*) FROM (SELECT c.id FROM face_clusters c
		JOIN faces f ON f.cluster_id = c.id AND f.assign = 'cluster' AND f.person_id IS NULL
		WHERE c.person_id IS NULL GROUP BY c.id HAVING COUNT(f.id) >= ?)`, max(minFaces, 1)).Scan(&n)
	return n
}

// CountPeople returns the number of visible (not hidden) people.
func (i *Index) CountPeople() int {
	var n int
	_ = i.db.QueryRow("SELECT COUNT(*) FROM people WHERE hidden = 0").Scan(&n)
	return n
}

func (i *Index) queryFaceInfos(q string, args ...any) ([]FaceInfo, error) {
	rows, err := i.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FaceInfo
	for rows.Next() {
		var fi FaceInfo
		var blob []byte
		var qual, nx, ny, nw, nh sql.NullFloat64
		var name sql.NullString
		f := &fi.Face
		if err := rows.Scan(&f.ID, &f.Path, &f.ThumbMtime, &f.BBox[0], &f.BBox[1], &f.BBox[2], &f.BBox[3],
			&blob, &f.ClusterID, &f.PersonID, &f.Assign, &qual, &nx, &ny, &nw, &nh, &name); err != nil {
			return nil, err
		}
		f.Embedding = DecodeEmbedding(blob)
		f.Quality = qual.Float64
		if nx.Valid && ny.Valid && nw.Valid && nh.Valid {
			f.NBox = [4]float64{nx.Float64, ny.Float64, nw.Float64, nh.Float64}
		} else {
			f.NBox = NoNBox
		}
		fi.PersonName = name.String
		out = append(out, fi)
	}
	return out, rows.Err()
}

const faceInfoSelect = "SELECT " + faceCols + ", p.name FROM faces f LEFT JOIN people p ON p.id = f.person_id "

// FacesOfPerson lists a person's faces with their distance to the person's
// centroid (the mean of those faces' embeddings).
func (i *Index) FacesOfPerson(personID int64, order FaceOrder) ([]FaceInfo, error) {
	fs, err := i.queryFaceInfos(faceInfoSelect+"WHERE f.person_id = ?", personID)
	if err != nil {
		return nil, err
	}
	return orderFaces(fs, meanEmbedding(fs), order), nil
}

// FacesOfCluster lists the faces of a cluster that follow it (not ignored,
// not resolved elsewhere), with their distance to the stored centroid.
func (i *Index) FacesOfCluster(clusterID int64, order FaceOrder) ([]FaceInfo, error) {
	fs, err := i.queryFaceInfos(faceInfoSelect+"WHERE f.cluster_id = ? AND f.assign = 'cluster'", clusterID)
	if err != nil {
		return nil, err
	}
	var blob []byte
	_ = i.db.QueryRow("SELECT centroid FROM face_clusters WHERE id = ?", clusterID).Scan(&blob)
	c := DecodeEmbedding(blob)
	if len(fs) > 0 && len(c) != len(fs[0].Embedding) {
		c = meanEmbedding(fs)
	}
	return orderFaces(fs, c, order), nil
}

// FacesInPhoto lists every face detected in path (ignored ones included, so
// the viewer can still show and un-ignore them), left to right.
func (i *Index) FacesInPhoto(path string) ([]FaceInfo, error) {
	fs, err := i.queryFaceInfos(faceInfoSelect+"WHERE f.path = ?", path)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(fs, func(a, b int) bool { return fs[a].BBox[0] < fs[b].BBox[0] })
	return fs, nil
}

// PeopleInPhoto returns the distinct names of the people in path.
func (i *Index) PeopleInPhoto(path string) []Person {
	rows, err := i.db.Query(`SELECT DISTINCT p.id, p.name FROM faces f JOIN people p ON p.id = f.person_id
		WHERE f.path = ? ORDER BY p.name COLLATE NOCASE`, path)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Person
	for rows.Next() {
		var p Person
		if rows.Scan(&p.ID, &p.Name) == nil {
			out = append(out, p)
		}
	}
	return out
}

func meanEmbedding(fs []FaceInfo) []float32 {
	if len(fs) == 0 {
		return nil
	}
	n := len(fs[0].Embedding)
	sum := make([]float64, n)
	cnt := 0
	for _, f := range fs {
		if len(f.Embedding) != n {
			continue
		}
		for k, v := range f.Embedding {
			sum[k] += float64(v)
		}
		cnt++
	}
	out := make([]float32, n)
	for k := range sum {
		out[k] = float32(sum[k] / float64(max(cnt, 1)))
	}
	return out
}

func orderFaces(fs []FaceInfo, centroid []float32, order FaceOrder) []FaceInfo {
	for k := range fs {
		fs[k].Dist = sqDist(fs[k].Embedding, centroid)
	}
	switch order {
	case OrderOutlierFirst:
		sort.SliceStable(fs, func(a, b int) bool {
			if fs[a].Dist != fs[b].Dist {
				return fs[a].Dist > fs[b].Dist
			}
			return fs[a].ID < fs[b].ID
		})
	default:
		sort.SliceStable(fs, func(a, b int) bool {
			if fs[a].Path != fs[b].Path {
				return fs[a].Path < fs[b].Path
			}
			return fs[a].ID < fs[b].ID
		})
	}
	return fs
}

// sqDist is squared-Euclidean distance; +Inf for mismatched/empty vectors.
// (face.SquaredEuclidean is the same function; cache cannot import face.)
func sqDist(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return math.Inf(1)
	}
	var s float64
	for k := range a {
		d := float64(a[k]) - float64(b[k])
		s += d * d
	}
	return s
}

// Exemplar is one confirmed face: an anchor for its person.
type Exemplar struct {
	PersonID  int64
	Embedding []float32
}

// Exemplars returns every confirmed face's embedding with its person. The
// embedding comes from the tag (the face the user saw), so it is valid even
// while the tag is dormant.
func (i *Index) Exemplars() ([]Exemplar, error) {
	rows, err := i.db.Query("SELECT person_id, embedding FROM face_tags WHERE verdict = 'confirmed' AND person_id IS NOT NULL ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Exemplar
	for rows.Next() {
		var e Exemplar
		var blob []byte
		if err := rows.Scan(&e.PersonID, &blob); err != nil {
			return nil, err
		}
		e.Embedding = DecodeEmbedding(blob)
		out = append(out, e)
	}
	return out, rows.Err()
}

// RejectionsByFace returns, per face id, the people that face has rejected.
func (i *Index) RejectionsByFace() (map[int64][]int64, error) {
	rows, err := i.db.Query(`SELECT l.face_id, t.person_id FROM face_tag_links l JOIN face_tags t ON t.id = l.tag_id
		WHERE t.verdict = 'rejected' AND t.person_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var f, p int64
		if err := rows.Scan(&f, &p); err != nil {
			return nil, err
		}
		out[f] = append(out[f], p)
	}
	return out, rows.Err()
}

// SuggestionMaxSqDistance is how close (squared Euclidean) an unattributed
// face must be to a confirmed exemplar to be suggested as that person.
const SuggestionMaxSqDistance = 0.36

// Suggestions returns, per person, the unattributed faces (following their
// cluster, which has no person) whose nearest confirmed exemplar belongs to
// that person within SuggestionMaxSqDistance, skipping faces that rejected
// that person. Nothing is assigned: the UI asks the user first.
func (i *Index) Suggestions() (map[int64][]FaceInfo, error) {
	ex, err := i.Exemplars()
	if err != nil || len(ex) == 0 {
		return map[int64][]FaceInfo{}, err
	}
	rej, err := i.RejectionsByFace()
	if err != nil {
		return nil, err
	}
	fs, err := i.queryFaceInfos(faceInfoSelect + "WHERE f.assign = 'cluster' AND f.person_id IS NULL")
	if err != nil {
		return nil, err
	}
	out := map[int64][]FaceInfo{}
	for _, f := range fs {
		best, bestD := int64(0), math.Inf(1)
		for _, e := range ex {
			if d := sqDist(f.Embedding, e.Embedding); d < bestD {
				best, bestD = e.PersonID, d
			}
		}
		if best == 0 || bestD >= SuggestionMaxSqDistance {
			continue
		}
		if containsID(rej[f.ID], best) {
			continue
		}
		f.Dist = bestD
		out[best] = append(out[best], f)
	}
	for p := range out {
		sort.SliceStable(out[p], func(a, b int) bool { return out[p][a].Dist < out[p][b].Dist })
	}
	return out, nil
}

// SuggestionsFor is Suggestions for one person.
func (i *Index) SuggestionsFor(personID int64) ([]FaceInfo, error) {
	all, err := i.Suggestions()
	if err != nil {
		return nil, err
	}
	return all[personID], nil
}

func containsID(ids []int64, id int64) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// NoNBox marks a face whose normalised bbox is unknown (rows written before
// schema v9 recorded the rendition size).
var NoNBox = [4]float64{-1, -1, -1, -1}

// HasNBox reports whether b is a real normalised bbox.
func HasNBox(b [4]float64) bool { return b[2] > 0 && b[3] > 0 && b[0] >= 0 && b[1] >= 0 }

// NBoxIoU is the intersection-over-union of two normalised boxes; 0 when
// either is unknown.
func NBoxIoU(a, b [4]float64) float64 {
	if !HasNBox(a) || !HasNBox(b) {
		return 0
	}
	iw := math.Min(a[0]+a[2], b[0]+b[2]) - math.Max(a[0], b[0])
	ih := math.Min(a[1]+a[3], b[1]+b[3]) - math.Max(a[1], b[1])
	if iw <= 0 || ih <= 0 {
		return 0
	}
	inter := iw * ih
	return inter / (a[2]*a[3] + b[2]*b[3] - inter)
}

// NormBox converts a pixel bbox detected on a w×h rendition to 0..1.
func NormBox(b [4]int, w, h int) [4]float64 {
	if w <= 0 || h <= 0 {
		return NoNBox
	}
	return [4]float64{
		float64(b[0]) / float64(w), float64(b[1]) / float64(h),
		float64(b[2]) / float64(w), float64(b[3]) / float64(h),
	}
}

func formatNBox(b [4]float64) string {
	parts := make([]string, 4)
	for k, v := range b {
		parts[k] = strconv.FormatFloat(v, 'f', 5, 64)
	}
	return strings.Join(parts, ",")
}

func parseNBox(s string) ([4]float64, bool) {
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return NoNBox, false
	}
	var b [4]float64
	for k, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil {
			return NoNBox, false
		}
		b[k] = v
	}
	return b, HasNBox(b)
}
