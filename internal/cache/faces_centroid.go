package cache

import "database/sql"

// CentroidWithout is the inverse of face.UpdatedCentroid: the running mean of
// a cluster of size count whose centroid is centroid, after removing the
// member v. A cluster left empty (count <= 1) keeps its centroid unchanged;
// with a count of 0 the next face to join replaces it outright.
func CentroidWithout(centroid, v []float32, count int) []float32 {
	if count <= 1 || len(centroid) != len(v) {
		return centroid
	}
	out := make([]float32, len(centroid))
	n := float32(count)
	for i := range centroid {
		out[i] = (centroid[i]*n - v[i]) / (n - 1)
	}
	return out
}

// FacesForPath returns the clustered face rows stored for path (ID, cluster
// and embedding only). The face pipeline reads them before a re-detection so
// it can take the old faces out of its in-memory centroids, mirroring what
// WriteFacesForPath does to the stored ones.
func (i *Index) FacesForPath(path string) []Face {
	rows, err := i.db.Query(
		"SELECT id, cluster_id, embedding FROM faces WHERE path = ? AND cluster_id IS NOT NULL ORDER BY id", path)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Face
	for rows.Next() {
		f := Face{Path: path}
		var emb []byte
		if err := rows.Scan(&f.ID, &f.ClusterID, &emb); err != nil {
			return nil
		}
		f.Embedding = DecodeEmbedding(emb)
		out = append(out, f)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

// subtractFacesTx takes the clustered faces of paths out of their clusters'
// stored running-mean centroids, inside tx and before the caller deletes
// those face rows (I-04 6c). Without it a deleted or re-detected face keeps
// weighting its cluster's centroid forever, and a re-detection counts it
// twice. Faces are removed one at a time in id order with the count taken
// from the faces table, so the arithmetic matches FacesForPath + the
// pipeline's in-memory CentroidWithout exactly.
func subtractFacesTx(tx *sql.Tx, paths ...string) error {
	type member struct {
		cluster int64
		emb     []float32
	}
	var members []member
	for _, p := range paths {
		rows, err := tx.Query(
			"SELECT cluster_id, embedding FROM faces WHERE path = ? AND cluster_id IS NOT NULL ORDER BY id", p)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m member
			var emb []byte
			if err := rows.Scan(&m.cluster, &emb); err != nil {
				rows.Close()
				return err
			}
			m.emb = DecodeEmbedding(emb)
			members = append(members, m)
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if len(members) == 0 {
		return nil
	}

	type state struct {
		centroid []float32
		count    int
	}
	clusters := map[int64]*state{}
	order := []int64{}
	for _, m := range members {
		st, ok := clusters[m.cluster]
		if !ok {
			var blob []byte
			var n int
			err := tx.QueryRow(
				"SELECT centroid, (SELECT COUNT(*) FROM faces WHERE cluster_id = ?) FROM face_clusters WHERE id = ?",
				m.cluster, m.cluster).Scan(&blob, &n)
			if err == sql.ErrNoRows {
				continue // dangling cluster_id: nothing to correct
			}
			if err != nil {
				return err
			}
			st = &state{centroid: DecodeEmbedding(blob), count: n}
			clusters[m.cluster] = st
			order = append(order, m.cluster)
		}
		st.centroid = CentroidWithout(st.centroid, m.emb, st.count)
		st.count--
	}
	for _, id := range order {
		if _, err := tx.Exec("UPDATE face_clusters SET centroid = ? WHERE id = ?",
			EncodeEmbedding(clusters[id].centroid), id); err != nil {
			return err
		}
	}
	return nil
}
