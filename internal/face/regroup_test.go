package face

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// faceDetection builds a detection with a fixed, normalisable box and the
// given embedding, so tests can exercise the production assignment path
// (assignAndPersist) without a detector daemon.
func faceDetection(emb []float32) Detection {
	return Detection{
		BBox:      [4]int{0, 0, 10, 10},
		Embedding: emb,
		Quality:   1,
		ImgW:      100,
		ImgH:      100,
	}
}

// persistDetections writes one photo's detections through assignAndPersist, so
// clusters, centroids and tags are exactly what a scan would have produced.
func persistDetections(t *testing.T, p *Pipeline, path string, dets ...Detection) {
	t.Helper()
	p.assignAndPersist(Job{
		Entry:    cache.Entry{Type: scan.TypePhoto, Path: path},
		ThumbMod: 1,
	}, dets)
}

// faceIDsForPath returns the ids of the faces stored for path, which are the
// ids TagFaces and Retag take.
func faceIDsForPath(t *testing.T, idx *cache.Index, path string) []int64 {
	t.Helper()
	faces, err := idx.FacesInPhoto(path)
	if err != nil {
		t.Fatalf("FacesInPhoto(%q): %v", path, err)
	}
	ids := make([]int64, len(faces))
	for k := range faces {
		ids[k] = faces[k].ID
	}
	return ids
}

// clusterByID finds one cluster in an AllClusters snapshot.
func clusterByID(clusters []cache.Cluster, id int64) (cache.Cluster, bool) {
	for _, c := range clusters {
		if c.ID == id {
			return c, true
		}
	}
	return cache.Cluster{}, false
}

// sortedClusterIDs makes an order-independent snapshot of the cluster layer,
// for "nothing changed" assertions.
func sortedClusterIDs(clusters []cache.Cluster) []int64 {
	ids := make([]int64, len(clusters))
	for k := range clusters {
		ids[k] = clusters[k].ID
	}
	slices.Sort(ids)
	return ids
}

// TestRegroupKeepsConfirmed pins the must-link rule: confirmed faces are
// seeded into their person's cluster and stay there, while unrelated faces at
// a distance still form their own cluster.
func TestRegroupKeepsConfirmed(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	personA, err := idx.CreatePerson("A")
	if err != nil {
		t.Fatalf("CreatePerson(A): %v", err)
	}

	// Three confirmed faces, close together on axis 0.
	var confirmedIDs []int64
	for i := range 3 {
		path := fmt.Sprintf("/confirmed-%d.jpg", i)
		persistDetections(t, p, path, faceDetection(axisVal(0, 0.01*float32(i))))
		ids := faceIDsForPath(t, idx, path)
		confirmedIDs = append(confirmedIDs, ids[0])
	}
	if _, err := idx.TagFaces(confirmedIDs, cache.VerdictConfirmed, personA); err != nil {
		t.Fatalf("TagFaces(confirmed): %v", err)
	}

	// Three unrelated faces on a different axis, far from person A's centroid.
	for i := range 3 {
		path := fmt.Sprintf("/other-%d.jpg", i)
		persistDetections(t, p, path, faceDetection(axisVal(5, 1+0.01*float32(i))))
	}

	if err := Regroup(context.Background(), idx, RegroupOptions{}); err != nil {
		t.Fatalf("Regroup: %v", err)
	}

	clusters := idx.AllClusters()
	for _, id := range confirmedIDs {
		f, ok := idx.Face(id)
		if !ok {
			t.Fatalf("face %d missing after regroup", id)
		}
		if !f.PersonID.Valid || f.PersonID.Int64 != personA {
			t.Errorf("confirmed face %d person = %+v, want %d", id, f.PersonID, personA)
		}
		if !f.ClusterID.Valid {
			t.Errorf("confirmed face %d has no cluster after regroup", id)
			continue
		}
		c, ok := clusterByID(clusters, f.ClusterID.Int64)
		if !ok {
			t.Errorf("confirmed face %d points at missing cluster %d", id, f.ClusterID.Int64)
			continue
		}
		if !c.PersonID.Valid || c.PersonID.Int64 != personA {
			t.Errorf("confirmed face %d cluster %d person = %+v, want %d",
				id, c.ID, c.PersonID, personA)
		}
	}
}

// TestRegroupHonoursRejections pins the cannot-link rule: a face the user says
// is not person A must not be re-seeded (or re-absorbed) into A's cluster even
// when it sits right next to A's confirmed exemplar.
func TestRegroupHonoursRejections(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	personA, err := idx.CreatePerson("A")
	if err != nil {
		t.Fatalf("CreatePerson(A): %v", err)
	}

	persistDetections(t, p, "/a.jpg", faceDetection(axisVal(0, 0)))
	aID := faceIDsForPath(t, idx, "/a.jpg")[0]
	if _, err := idx.TagFaces([]int64{aID}, cache.VerdictConfirmed, personA); err != nil {
		t.Fatalf("TagFaces(confirmed): %v", err)
	}

	// X is almost on top of A's exemplar but the user rejected A for it.
	persistDetections(t, p, "/x.jpg", faceDetection(axisVal(0, 0.02)))
	xID := faceIDsForPath(t, idx, "/x.jpg")[0]
	if _, err := idx.TagFaces([]int64{xID}, cache.VerdictRejected, personA); err != nil {
		t.Fatalf("TagFaces(rejected): %v", err)
	}

	if err := Regroup(context.Background(), idx, RegroupOptions{}); err != nil {
		t.Fatalf("Regroup: %v", err)
	}

	f, ok := idx.Face(xID)
	if !ok {
		t.Fatalf("rejected face %d missing after regroup", xID)
	}
	if f.PersonID.Valid && f.PersonID.Int64 == personA {
		t.Errorf("rejected face %d was re-attributed to A", xID)
	}
	if !f.ClusterID.Valid {
		return // unclustered is fine; there is no cluster person to check
	}
	c, ok := clusterByID(idx.AllClusters(), f.ClusterID.Int64)
	if !ok {
		t.Fatalf("rejected face %d points at missing cluster %d", xID, f.ClusterID.Int64)
	}
	if c.PersonID.Valid && c.PersonID.Int64 == personA {
		t.Errorf("rejected face %d landed in A's cluster %d", xID, c.ID)
	}
}

// TestRegroupRelinksNames pins name survival: a group the user named on the
// cluster (not face by face) keeps its name across a regroup, because the old
// cluster owned more than half of the new cluster's members.
func TestRegroupRelinksNames(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	personB, err := idx.CreatePerson("B")
	if err != nil {
		t.Fatalf("CreatePerson(B): %v", err)
	}

	// Four unconfirmed faces close together on axis 2: one unnamed cluster.
	for i := range 4 {
		persistDetections(t, p, fmt.Sprintf("/b-%d.jpg", i), faceDetection(axisVal(2, 0.9+0.01*float32(i))))
	}
	clusters := idx.AllClusters()
	if len(clusters) != 1 {
		t.Fatalf("setup produced %d clusters, want 1", len(clusters))
	}
	oldCluster := clusters[0].ID
	if _, err := idx.AssignClusterToPerson(oldCluster, personB); err != nil {
		t.Fatalf("AssignClusterToPerson: %v", err)
	}

	if err := Regroup(context.Background(), idx, RegroupOptions{}); err != nil {
		t.Fatalf("Regroup: %v", err)
	}

	for i := range 4 {
		ids := faceIDsForPath(t, idx, fmt.Sprintf("/b-%d.jpg", i))
		f, ok := idx.Face(ids[0])
		if !ok {
			t.Fatalf("face %d missing after regroup", ids[0])
		}
		if !f.PersonID.Valid || f.PersonID.Int64 != personB {
			t.Errorf("face %d person = %+v, want B (%d) after relink", ids[0], f.PersonID, personB)
		}
	}
}

// TestRegroupIgnoredUnclustered pins that "not a face" is durable: an ignored
// face is stored but must come out of every cluster after a regroup.
func TestRegroupIgnoredUnclustered(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	persistDetections(t, p, "/ignored.jpg", faceDetection(axisVal(3, 1)))
	ignoredID := faceIDsForPath(t, idx, "/ignored.jpg")[0]
	if _, err := idx.TagFaces([]int64{ignoredID}, cache.VerdictIgnored, 0); err != nil {
		t.Fatalf("TagFaces(ignored): %v", err)
	}

	// A normal face too, so the regroup pass has work to do.
	persistDetections(t, p, "/normal.jpg", faceDetection(axisVal(3, 0.5)))

	if err := Regroup(context.Background(), idx, RegroupOptions{}); err != nil {
		t.Fatalf("Regroup: %v", err)
	}

	f, ok := idx.Face(ignoredID)
	if !ok {
		t.Fatalf("ignored face %d missing after regroup", ignoredID)
	}
	if f.ClusterID.Valid {
		t.Errorf("ignored face %d still in cluster %d after regroup", ignoredID, f.ClusterID.Int64)
	}
}

// TestRegroupDeterministic pins the "deterministic for a fixed input" promise:
// the same faces regroup to a byte-identical result twice, and four well-
// separated blobs come out as exactly four pure clusters.
func TestRegroupDeterministic(t *testing.T) {
	const (
		blobs    = 4
		perBlob  = 15
		total    = blobs * perBlob
		sigma    = 0.05
		noiseDim = 4
	)
	rng := rand.New(rand.NewSource(42))
	axes := rng.Perm(128)

	var faces []cache.RegroupFace
	blobOf := make(map[int64]int, total)
	for b := range blobs {
		// Unit-ish centers on distinct axes: two centers are 2² apart, while a
		// blob stays tight around its own center.
		center := axisVal(axes[b], 1+float32(rng.NormFloat64()*0.02))
		for range perBlob {
			emb := make([]float32, 128)
			copy(emb, center)
			// Noise only on a few components: full 128-d noise at sigma 0.05
			// puts two samples of one blob ~0.8² apart, which would legitimately
			// split the blob at MaxClusterSqDistance.
			for d := range noiseDim {
				emb[d] += float32(rng.NormFloat64() * sigma)
			}
			id := int64(len(faces) + 1)
			faces = append(faces, cache.RegroupFace{
				ID:        id,
				Embedding: emb,
				Quality:   0.06 + rng.Float64()*0.94,
			})
			blobOf[id] = b
		}
	}
	if len(faces) != total {
		t.Fatalf("built %d faces, want %d", len(faces), total)
	}

	first, err := regroupCore(context.Background(), faces, nil, RegroupOptions{})
	if err != nil {
		t.Fatalf("first regroupCore: %v", err)
	}
	second, err := regroupCore(context.Background(), faces, nil, RegroupOptions{})
	if err != nil {
		t.Fatalf("second regroupCore: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("regroupCore is not deterministic:\nfirst:  %+v\nsecond: %+v", first, second)
	}

	if len(first) != blobs {
		t.Fatalf("clusters = %d, want %d", len(first), blobs)
	}
	seenBlob := map[int]int{} // blob -> cluster index
	members := 0
	for ci, c := range first {
		if len(c.Members) == 0 {
			t.Fatalf("cluster %d has no members", ci)
		}
		blob := blobOf[c.Members[0]]
		for _, id := range c.Members {
			if blobOf[id] != blob {
				t.Errorf("cluster %d mixes blob %d (face %d) and %d", ci, blob, id, blobOf[id])
			}
		}
		if prev, ok := seenBlob[blob]; ok {
			t.Errorf("blob %d split across clusters %d and %d", blob, prev, ci)
		}
		seenBlob[blob] = ci
		members += len(c.Members)
	}
	if members != total {
		t.Errorf("clustered %d faces, want %d", members, total)
	}
}

// TestRegroupCancel pins that a cancelled regroup leaves the cluster layer
// untouched — a partial replacement is never committed.
func TestRegroupCancel(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	for i := range 3 {
		persistDetections(t, p, fmt.Sprintf("/c-%d.jpg", i), faceDetection(axisVal(0, 0.1*float32(i))))
	}
	before := sortedClusterIDs(idx.AllClusters())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Regroup(ctx, idx, RegroupOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Regroup on a cancelled context = %v, want context.Canceled", err)
	}

	after := sortedClusterIDs(idx.AllClusters())
	if !slices.Equal(before, after) {
		t.Errorf("cluster ids changed after cancelled regroup: before %v, after %v", before, after)
	}
}

// TestRegroupProgress pins the progress contract: the final callback is
// (total, total), so a UI can rely on it to clear its progress bar.
func TestRegroupProgress(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	const total = 5
	for i := range total {
		persistDetections(t, p, fmt.Sprintf("/p-%d.jpg", i), faceDetection(axisVal(1, 0.5+0.01*float32(i))))
	}

	var calls int
	var lastDone, lastTotal int
	err := Regroup(context.Background(), idx, RegroupOptions{
		Progress: func(done, n int) {
			calls++
			lastDone, lastTotal = done, n
		},
	})
	if err != nil {
		t.Fatalf("Regroup: %v", err)
	}
	if calls == 0 {
		t.Fatal("Progress was never called")
	}
	if lastTotal != total {
		t.Errorf("last Progress total = %d, want %d", lastTotal, total)
	}
	if lastDone != lastTotal {
		t.Errorf("last Progress = (%d, %d), want (%d, %d)", lastDone, lastTotal, total, total)
	}
}
