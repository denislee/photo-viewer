package face

import (
	"context"
	"sort"

	"github.com/dns/photo-viewer/internal/cache"
)

// Regroup (plan §3.5 item 4) re-clusters every stored embedding from scratch
// without re-detecting. It fixes the order dependence of the streaming
// assigner (one bad early face dragging a centroid): faces are replayed best
// quality first, around seeds made of the user's confirmed faces.
//
//  1. must-link: one cluster per person, seeded with all its confirmed faces;
//  2. every other face (not ignored, not under MinClusterQuality), in
//     descending quality then id order, goes through the same Assigner the
//     pipeline uses — exemplars, threshold and rejections (cannot-link);
//  3. names re-link: a cluster takes the person owning the majority of its
//     confirmed faces, else the person of the old cluster that holds more
//     than half of its members — so names survive on groups the user never
//     fixed face by face.
//
// The result is deterministic for a fixed input.

// RegroupOptions tunes one regroup run.
type RegroupOptions struct {
	MaxSq float64 // strictness (squared); 0 = MaxClusterSqDistance
	// Progress, when set, is called with (done, total) every few hundred
	// faces and once at the end.
	Progress func(done, total int)
}

// Regroup runs the regroup pass over idx and replaces its clusters. Callers
// with a running pipeline must go through Pipeline.Regroup so the pipeline's
// cluster cache is serialised with it.
func Regroup(ctx context.Context, idx *cache.Index, opt RegroupOptions) error {
	faces, oldPersons, err := idx.LoadForRegroup()
	if err != nil {
		return err
	}
	clusters, err := regroupCore(ctx, faces, oldPersons, opt)
	if err != nil {
		return err
	}
	return idx.ReplaceClusters(clusters)
}

func regroupCore(ctx context.Context, faces []cache.RegroupFace, oldPersons map[int64]int64, opt RegroupOptions) ([]cache.RegroupCluster, error) {
	total := len(faces)
	report := func(done int) {
		if opt.Progress != nil {
			opt.Progress(done, total)
		}
	}

	// 1. Seeds: one cluster per person with confirmed faces, persons in id
	// order so the output does not depend on map iteration.
	byPerson := map[int64][]int{}
	var persons []int64
	var exemplars []cache.Exemplar
	for k, f := range faces {
		if f.Confirmed > 0 && len(f.Embedding) > 0 {
			if _, ok := byPerson[f.Confirmed]; !ok {
				persons = append(persons, f.Confirmed)
			}
			byPerson[f.Confirmed] = append(byPerson[f.Confirmed], k)
			exemplars = append(exemplars, cache.Exemplar{PersonID: f.Confirmed, Embedding: f.Embedding})
		}
	}
	sort.Slice(persons, func(a, b int) bool { return persons[a] < persons[b] })

	var cl []cache.Cluster // working set for the Assigner (ID = slot index)
	var members [][]int    // face indexes per slot
	for _, p := range persons {
		c := cache.Cluster{ID: int64(len(cl))}
		c.PersonID.Int64, c.PersonID.Valid = p, true
		for _, k := range byPerson[p] {
			c.Centroid = UpdatedCentroid(c.Centroid, faces[k].Embedding, c.Count)
			c.Count++
		}
		cl = append(cl, c)
		members = append(members, byPerson[p])
	}

	// 2. Everything else, best quality first.
	var rest []int
	for k, f := range faces {
		if f.Confirmed == 0 {
			rest = append(rest, k)
		}
	}
	sort.SliceStable(rest, func(a, b int) bool {
		qa, qb := faces[rest[a]].Quality, faces[rest[b]].Quality
		if qa != qb {
			return qa > qb
		}
		return faces[rest[a]].ID < faces[rest[b]].ID
	})
	as := &Assigner{MaxSq: opt.MaxSq, Exemplars: exemplars}
	done := len(faces) - len(rest)
	for n, k := range rest {
		if n%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			report(done + n)
		}
		f := faces[k]
		hint := cache.TagHint{Ignored: f.Ignored, Rejected: f.Rejected}
		d := as.Decide(f.Embedding, f.Quality, hint, cl)
		switch {
		case d.Cluster >= 0:
			c := &cl[d.Cluster]
			c.Centroid = UpdatedCentroid(c.Centroid, f.Embedding, c.Count)
			c.Count++
			members[d.Cluster] = append(members[d.Cluster], k)
		case d.New:
			c := cache.Cluster{ID: int64(len(cl)), Centroid: append([]float32(nil), f.Embedding...), Count: 1}
			if d.Person > 0 {
				c.PersonID.Int64, c.PersonID.Valid = d.Person, true
			}
			cl = append(cl, c)
			members = append(members, []int{k})
		}
	}

	// 3. Re-link names.
	out := make([]cache.RegroupCluster, 0, len(cl))
	for s, c := range cl {
		rc := cache.RegroupCluster{Centroid: c.Centroid}
		for _, k := range members[s] {
			rc.Members = append(rc.Members, faces[k].ID)
		}
		switch {
		case c.PersonID.Valid:
			rc.PersonID = c.PersonID.Int64
		default:
			rc.PersonID = relinkPerson(faces, members[s], oldPersons)
		}
		out = append(out, rc)
	}
	report(total)
	return out, nil
}

// relinkPerson picks the name for a regrouped cluster without a seed: the
// majority person of its confirmed faces, else the person of the old
// cluster holding more than half of its members, unless any member rejected
// that person.
func relinkPerson(faces []cache.RegroupFace, members []int, oldPersons map[int64]int64) int64 {
	if len(members) == 0 {
		return 0
	}
	conf := map[int64]int{}
	old := map[int64]int{}
	for _, k := range members {
		if p := faces[k].Confirmed; p > 0 {
			conf[p]++
		}
		if c := faces[k].OldCluster; c > 0 {
			old[c]++
		}
	}
	if p := argmax(conf); p > 0 {
		return p
	}
	c := argmax(old)
	if c == 0 || old[c]*2 <= len(members) {
		return 0
	}
	p := oldPersons[c]
	if p == 0 {
		return 0
	}
	for _, k := range members {
		if rejected(cache.TagHint{Rejected: faces[k].Rejected}, p) {
			return 0
		}
	}
	return p
}

// argmax returns the key with the highest count, ties to the lower key.
func argmax(m map[int64]int) int64 {
	var best int64
	bestN := 0
	for k, n := range m {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}
