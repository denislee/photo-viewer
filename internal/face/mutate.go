package face

import (
	"context"

	"github.com/dns/photo-viewer/internal/cache"
)

// Mutate is the single serialisation point for every write to clusters or
// identity (plan §3.6). It holds clusterMu — the lock the workers' assign
// step takes — while fn runs, then drops the in-memory cluster and exemplar
// cache, so a worker can never write centroids computed from a view of the
// clusters that predates the edit. The freshness mirror is kept (an edit
// does not change which files were scanned), which is why this is not
// InvalidateClusters.
//
// It works on a disabled pipeline (helper missing) and on a nil *Pipeline,
// so people stay editable without the helper.
func (p *Pipeline) Mutate(fn func(*cache.Index) error) error {
	if p == nil {
		return nil
	}
	p.clusterMu.Lock()
	defer p.clusterMu.Unlock()
	err := fn(p.idx)
	p.cachedClusters = nil
	p.exemplars = nil
	p.clusterCacheOK = false
	return err
}

// Retag records a user decision on faces (cache.Index.TagFaces) and then
// re-places each face the decision moves:
//
//   - rejected(P): out of P's cluster into the nearest cluster of anyone
//     else, or a new singleton;
//   - confirmed(P): into P's nearest cluster unless its cluster is already
//     P's;
//   - ignored: out of clustering.
//
// The returned undo also moves the faces back.
func (p *Pipeline) Retag(faceIDs []int64, verdict cache.Verdict, personID int64) (*cache.Undo, error) {
	var undo *cache.Undo
	err := p.Mutate(func(idx *cache.Index) error {
		var err error
		undo, err = Retag(idx, p.maxSq, faceIDs, verdict, personID)
		return err
	})
	return undo, err
}

// Retag is Pipeline.Retag for callers without a pipeline (tests).
// maxSq is the strictness threshold (0 = default).
func Retag(idx *cache.Index, maxSq float64, faceIDs []int64, verdict cache.Verdict, personID int64) (*cache.Undo, error) {
	undo, err := idx.TagFaces(faceIDs, verdict, personID)
	if err != nil || verdict == cache.VerdictNone {
		return undo, err
	}
	clusters := idx.AllClusters()
	ex, err := idx.Exemplars()
	if err != nil {
		return undo, err
	}
	as := &Assigner{MaxSq: maxSq, Exemplars: ex}
	for _, id := range faceIDs {
		f, ok := idx.Face(id)
		if !ok {
			continue
		}
		hint, err := idx.FaceHint(id)
		if err != nil {
			return undo, err
		}
		cur := -1
		for k := range clusters {
			if f.ClusterID.Valid && clusters[k].ID == f.ClusterID.Int64 {
				cur = k
				break
			}
		}
		if verdict == cache.VerdictConfirmed && cur >= 0 &&
			clusters[cur].PersonID.Valid && clusters[cur].PersonID.Int64 == personID {
			continue // already in one of P's clusters
		}
		if verdict == cache.VerdictRejected && (cur < 0 ||
			!clusters[cur].PersonID.Valid || clusters[cur].PersonID.Int64 != personID) {
			// Its cluster isn't P's (it was P by an exemplar or nothing):
			// the rejection alone already un-attributes it.
			continue
		}
		// Decide against the clusters without this face in them.
		others := clusters
		if cur >= 0 {
			others = append(append([]cache.Cluster(nil), clusters[:cur]...), clusters[cur+1:]...)
		}
		d := as.Decide(f.Embedding, f.Quality, hint, others)
		to := int64(-1)
		newPerson := int64(0)
		switch {
		case d.Cluster >= 0:
			to = others[d.Cluster].ID
		case d.New:
			to, newPerson = 0, d.Person
		}
		from := f.ClusterID.Int64
		if to == from && to > 0 {
			continue
		}
		if _, err := idx.ReassignFace(id, to, newPerson); err != nil {
			return undo, err
		}
		undo.AddMove(id, from)
		clusters = idx.AllClusters() // counts/centroids changed
	}
	return undo, nil
}

// Regroup runs face.Regroup under Mutate, with the pipeline's strictness
// when opt.MaxSq is 0. Workers wait on the assign lock while it runs.
func (p *Pipeline) Regroup(ctx context.Context, opt RegroupOptions) error {
	return p.Mutate(func(idx *cache.Index) error {
		if opt.MaxSq <= 0 {
			opt.MaxSq = p.maxSq
		}
		return Regroup(ctx, idx, opt)
	})
}
