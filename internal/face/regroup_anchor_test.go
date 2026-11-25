package face

import (
	"context"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// TestRebuildKeepsGroupNames guards the person_anchors path: a group named
// without confirming any face lives only in the machine layer, so Clear()
// would erase its name. Clear saves the named centroid, the re-detected faces
// regroup, and the new cluster near that centroid takes the name back. A
// cluster far from every anchor stays unnamed (negative control).
func TestRebuildKeepsGroupNames(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)
	job := func(path string) Job {
		return Job{Entry: cache.Entry{Type: scan.TypePhoto, Path: path}, ThumbMod: 1}
	}
	write := func() {
		for k, path := range []string{"/a.jpg", "/b.jpg", "/c.jpg"} {
			p.assignAndPersist(job(path), []Detection{{BBox: [4]int{1, 1, 5, 5}, ImgW: 10, ImgH: 10, Embedding: axisVec(0.01 * float32(k))}})
		}
		p.assignAndPersist(job("/far.jpg"), []Detection{{BBox: [4]int{1, 1, 5, 5}, ImgW: 10, ImgH: 10, Embedding: farVec()}})
	}
	write()
	pid, err := idx.CreatePerson("Ana")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := idx.FacesInPhoto("/a.jpg")
	if _, err := idx.AssignClusterToPerson(f[0].ClusterID.Int64, pid); err != nil {
		t.Fatal(err)
	}

	if err := idx.Clear(); err != nil {
		t.Fatal(err)
	}
	p.InvalidateClusters()
	write()
	if got, _ := idx.FacesOfPerson(pid, cache.OrderPath); len(got) != 0 {
		t.Fatalf("before regroup Ana already has %d faces; the test would prove nothing", len(got))
	}
	if err := p.Regroup(context.Background(), RegroupOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := idx.FacesOfPerson(pid, cache.OrderPath)
	if len(got) != 3 {
		t.Fatalf("after rebuild+regroup Ana has %d faces, want 3", len(got))
	}
	if far, _ := idx.FacesInPhoto("/far.jpg"); len(far) != 1 || far[0].PersonID.Valid {
		t.Errorf("far face picked up a name: %+v", far)
	}
	if a, _ := idx.Anchors(); len(a) != 0 {
		t.Errorf("anchors not consumed by regroup: %d left", len(a))
	}
}

func farVec() []float32 {
	v := make([]float32, 128)
	v[64] = 1
	return v
}
