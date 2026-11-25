package cache

import (
	"database/sql"
	"sort"
)

// RegroupFace is one stored face as the regroup pass sees it.
type RegroupFace struct {
	ID         int64
	Embedding  []float32
	Quality    float64
	Ignored    bool    // linked ignored tag
	Confirmed  int64   // linked confirmed tag's person, 0 if none
	Rejected   []int64 // people this face is NOT
	OldCluster int64   // cluster before the regroup, 0 if none
}

// RegroupCluster is one cluster the regroup pass produced.
type RegroupCluster struct {
	Centroid []float32
	PersonID int64 // 0 = unnamed
	Members  []int64
}

// LoadForRegroup returns every face with its tag state, in id order, plus the
// person of every current cluster.
func (i *Index) LoadForRegroup() ([]RegroupFace, map[int64]int64, error) {
	rows, err := i.db.Query("SELECT id, embedding, quality, cluster_id FROM faces ORDER BY id")
	if err != nil {
		return nil, nil, err
	}
	var out []RegroupFace
	pos := map[int64]int{}
	for rows.Next() {
		var f RegroupFace
		var blob []byte
		var q sql.NullFloat64
		var c sql.NullInt64
		if err := rows.Scan(&f.ID, &blob, &q, &c); err != nil {
			rows.Close()
			return nil, nil, err
		}
		f.Embedding = DecodeEmbedding(blob)
		f.Quality = q.Float64
		f.OldCluster = c.Int64
		pos[f.ID] = len(out)
		out = append(out, f)
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	rows, err = i.db.Query(`SELECT l.face_id, t.verdict, t.person_id FROM face_tag_links l
		JOIN face_tags t ON t.id = l.tag_id ORDER BY t.updated_at, t.id`)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var fid int64
		var verdict string
		var pid sql.NullInt64
		if err := rows.Scan(&fid, &verdict, &pid); err != nil {
			rows.Close()
			return nil, nil, err
		}
		k, ok := pos[fid]
		if !ok {
			continue
		}
		switch Verdict(verdict) {
		case VerdictConfirmed:
			out[k].Confirmed, out[k].Ignored = pid.Int64, false
		case VerdictIgnored:
			out[k].Confirmed, out[k].Ignored = 0, true
		case VerdictRejected:
			out[k].Rejected = append(out[k].Rejected, pid.Int64)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, nil, err
	}
	persons := map[int64]int64{}
	rows, err = i.db.Query("SELECT id, person_id FROM face_clusters WHERE person_id IS NOT NULL")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, p int64
		if err := rows.Scan(&id, &p); err != nil {
			return nil, nil, err
		}
		persons[id] = p
	}
	return out, persons, rows.Err()
}

// ReplaceClusters swaps the whole cluster layer for the regroup result in one
// transaction: every cluster is recreated, every face re-pointed (faces in no
// cluster are left unclustered) and every identity re-derived. Tags are not
// touched.
func (i *Index) ReplaceClusters(clusters []RegroupCluster) error {
	tx, err := i.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("UPDATE faces SET cluster_id = NULL"); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM face_clusters"); err != nil {
		return err
	}
	ins, err := tx.Prepare("INSERT INTO face_clusters (centroid, sample_face_id, person_id) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer ins.Close()
	set, err := tx.Prepare("UPDATE faces SET cluster_id = ? WHERE id = ?")
	if err != nil {
		return err
	}
	defer set.Close()
	for _, c := range clusters {
		if len(c.Members) == 0 {
			continue
		}
		var pid any
		if c.PersonID > 0 {
			pid = c.PersonID
		}
		members := append([]int64(nil), c.Members...)
		sort.Slice(members, func(a, b int) bool { return members[a] < members[b] })
		res, err := ins.Exec(EncodeEmbedding(c.Centroid), members[0], pid)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for _, f := range members {
			if _, err := set.Exec(id, f); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(rederiveSQL); err != nil {
		return err
	}
	return tx.Commit()
}
