package face

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

func workerJob(path string) Job {
	return Job{Entry: cache.Entry{Type: scan.TypePhoto, Path: path}, ThumbMod: 1}
}

// TestMutateNilAndDisabled pins the two graceful-degrade cases: a nil pipeline
// is a safe no-op (people stay editable without a helper), and a disabled
// pipeline still runs the mutation and propagates its error.
func TestMutateNilAndDisabled(t *testing.T) {
	var nilPipeline *Pipeline
	called := false
	if err := nilPipeline.Mutate(func(*cache.Index) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("nil Pipeline.Mutate = %v, want nil", err)
	}
	if called {
		t.Fatal("nil Pipeline.Mutate ran fn")
	}

	idx := testIndex(t)
	p := NewPipeline(idx, nil)
	p.enabled.Store(false) // test the disabled path even when the helper exists

	want := errors.New("mutation failed")
	called = false
	err := p.Mutate(func(got *cache.Index) error {
		called = true
		if got != idx {
			t.Errorf("Mutate passed %p, want the pipeline's index %p", got, idx)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("disabled Pipeline.Mutate = %v, want %v", err, want)
	}
	if !called {
		t.Fatal("disabled Pipeline.Mutate did not run fn")
	}
}

// TestMutateDropsCache pins the serialisation contract: after a mutation the
// in-memory cluster/exemplar cache must be dropped so no worker assigns against
// a view that predates the edit.
func TestMutateDropsCache(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)
	persistDetections(t, p, "/a.jpg", faceDetection(axisVal(0, 0)))

	p.clusterMu.Lock()
	primed := p.clusterCacheOK
	p.clusterMu.Unlock()
	if !primed {
		t.Fatal("assignAndPersist did not prime the cluster cache")
	}

	if err := p.Mutate(func(*cache.Index) error { return nil }); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	p.clusterMu.Lock()
	ok, cached, ex := p.clusterCacheOK, p.cachedClusters, p.exemplars
	p.clusterMu.Unlock()
	if ok {
		t.Error("Mutate left clusterCacheOK true")
	}
	if cached != nil {
		t.Errorf("Mutate left %d cached clusters", len(cached))
	}
	if ex != nil {
		t.Errorf("Mutate left %d cached exemplars", len(ex))
	}
}

// TestMutateConcurrentWithWorkers runs the assign step from eight goroutines
// while another goroutine repeatedly mutates identity, then checks that no
// write was lost (all 200 faces present) and that every stored centroid is
// still exactly the mean of its members — the invariant a stale cache would
// break. Run it under -race.
func TestMutateConcurrentWithWorkers(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	person, err := idx.CreatePerson("Worker-edited")
	if err != nil {
		t.Fatalf("CreatePerson: %v", err)
	}

	const (
		workers   = 8
		perWorker = 25
		axes      = 3
	)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWorker {
				axis := (w + i) % axes
				emb := axisVal(axis, 1+0.01*float32(i%5))
				p.assignAndPersist(workerJob(fmt.Sprintf("/w%d-%d.jpg", w, i)),
					[]Detection{faceDetection(emb)})
			}
		}()
	}

	// One mutator, hammering the same "name the biggest group" edit the People
	// sidebar makes, while the workers assign.
	stop := make(chan struct{})
	mutated := make(chan error, 1)
	var mutWG sync.WaitGroup
	mutWG.Add(1)
	go func() {
		defer mutWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			err := p.Mutate(func(idx *cache.Index) error {
				clusters := idx.AllClusters()
				if len(clusters) == 0 {
					return nil
				}
				_, err := idx.AssignClusterToPerson(clusters[0].ID, person)
				return err
			})
			if err != nil {
				mutated <- err
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
	close(stop)
	mutWG.Wait()
	select {
	case err := <-mutated:
		t.Fatalf("concurrent Mutate: %v", err)
	default:
	}

	clusters := idx.AllClusters()
	total := 0
	for _, c := range clusters {
		faces, err := idx.FacesOfCluster(c.ID, cache.OrderPath)
		if err != nil {
			t.Fatalf("FacesOfCluster(%d): %v", c.ID, err)
		}
		total += len(faces)
		if len(faces) == 0 {
			continue
		}
		if len(c.Centroid) != len(faces[0].Embedding) {
			t.Errorf("cluster %d centroid dims = %d, want %d", c.ID, len(c.Centroid), len(faces[0].Embedding))
			continue
		}
		sum := make([]float64, len(c.Centroid))
		for _, f := range faces {
			for k, v := range f.Embedding {
				sum[k] += float64(v)
			}
		}
		for k := range sum {
			want := sum[k] / float64(len(faces))
			if diff := math.Abs(float64(c.Centroid[k]) - want); diff > 1e-4 {
				t.Errorf("cluster %d centroid[%d] = %v, want mean %v (diff %v)",
					c.ID, k, c.Centroid[k], want, diff)
				break
			}
		}
	}
	if total != workers*perWorker {
		t.Errorf("stored face rows reachable from clusters = %d, want %d", total, workers*perWorker)
	}
}

// TestRetagRejectMovesAndUndo pins the rejection flow end to end: rejecting a
// face for its cluster's person moves it out of that person's group, and the
// undo moves it back into the original cluster with the original identity.
func TestRetagRejectMovesAndUndo(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	personA, err := idx.CreatePerson("A")
	if err != nil {
		t.Fatalf("CreatePerson(A): %v", err)
	}

	for i := range 3 {
		persistDetections(t, p, fmt.Sprintf("/a-%d.jpg", i), faceDetection(axisVal(0, 0.01*float32(i))))
	}
	// A separate, far-away unnamed cluster so "others" is never empty.
	persistDetections(t, p, "/far.jpg", faceDetection(axisVal(7, 1)))

	clusters := idx.AllClusters()
	var aCluster cache.Cluster
	for _, c := range clusters {
		if c.Count == 3 {
			aCluster = c
			break
		}
	}
	if aCluster.ID == 0 {
		t.Fatalf("setup produced no 3-face cluster: %+v", clusters)
	}
	if _, err := idx.AssignClusterToPerson(aCluster.ID, personA); err != nil {
		t.Fatalf("AssignClusterToPerson: %v", err)
	}

	faces, err := idx.FacesOfCluster(aCluster.ID, cache.OrderPath)
	if err != nil || len(faces) == 0 {
		t.Fatalf("FacesOfCluster: %d faces, %v", len(faces), err)
	}
	faceID := faces[0].ID

	undo, err := p.Retag([]int64{faceID}, cache.VerdictRejected, personA)
	if err != nil {
		t.Fatalf("Retag(rejected): %v", err)
	}
	if undo == nil {
		t.Fatal("Retag(rejected) returned no undo")
	}

	moved, ok := idx.Face(faceID)
	if !ok {
		t.Fatalf("face %d missing after Retag", faceID)
	}
	if !moved.ClusterID.Valid || moved.ClusterID.Int64 == aCluster.ID {
		t.Errorf("rejected face %d cluster = %+v, want a cluster other than %d",
			faceID, moved.ClusterID, aCluster.ID)
	}
	if moved.PersonID.Valid && moved.PersonID.Int64 == personA {
		t.Errorf("rejected face %d still resolves to A", faceID)
	}

	if err := idx.ApplyUndo(undo); err != nil {
		t.Fatalf("ApplyUndo: %v", err)
	}
	restored, ok := idx.Face(faceID)
	if !ok {
		t.Fatalf("face %d missing after undo", faceID)
	}
	if !restored.ClusterID.Valid || restored.ClusterID.Int64 != aCluster.ID {
		t.Errorf("after undo face %d cluster = %+v, want %d", faceID, restored.ClusterID, aCluster.ID)
	}
	if !restored.PersonID.Valid || restored.PersonID.Int64 != personA {
		t.Errorf("after undo face %d person = %+v, want A (%d)", faceID, restored.PersonID, personA)
	}
}

// TestRetagIgnoredAndConfirm pins the remaining two verdicts: "ignore" pulls a
// face out of clustering, and "confirm B" moves a face from A's cluster into
// B's, where it resolves to B.
func TestRetagIgnoredAndConfirm(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	personA, err := idx.CreatePerson("A")
	if err != nil {
		t.Fatalf("CreatePerson(A): %v", err)
	}
	personB, err := idx.CreatePerson("B")
	if err != nil {
		t.Fatalf("CreatePerson(B): %v", err)
	}

	// Cluster A: three faces near axis 0, named A.
	for i := range 3 {
		persistDetections(t, p, fmt.Sprintf("/a-%d.jpg", i), faceDetection(axisVal(0, 0.01*float32(i))))
	}
	aCluster := idx.AllClusters()[0]
	if aCluster.Count != 3 {
		t.Fatalf("A cluster count = %d, want 3", aCluster.Count)
	}
	if _, err := idx.AssignClusterToPerson(aCluster.ID, personA); err != nil {
		t.Fatalf("AssignClusterToPerson(A): %v", err)
	}

	// Cluster B: its own cluster far away, named B.
	persistDetections(t, p, "/b.jpg", faceDetection(axisVal(5, 1)))
	bFaceID := faceIDsForPath(t, idx, "/b.jpg")[0]
	bFace, ok := idx.Face(bFaceID)
	if !ok || !bFace.ClusterID.Valid {
		t.Fatalf("B face %d was not clustered: %+v", bFaceID, bFace)
	}
	bClusterID := bFace.ClusterID.Int64
	if bClusterID == aCluster.ID {
		t.Fatalf("B face joined A's cluster %d", aCluster.ID)
	}
	if _, err := idx.AssignClusterToPerson(bClusterID, personB); err != nil {
		t.Fatalf("AssignClusterToPerson(B): %v", err)
	}

	// Ignore one of A's faces: out of clustering.
	aFaces, err := idx.FacesOfCluster(aCluster.ID, cache.OrderPath)
	if err != nil || len(aFaces) != 3 {
		t.Fatalf("FacesOfCluster(A): %d faces, %v", len(aFaces), err)
	}
	ignoredID := aFaces[0].ID
	if _, err := p.Retag([]int64{ignoredID}, cache.VerdictIgnored, 0); err != nil {
		t.Fatalf("Retag(ignored): %v", err)
	}
	ignored, ok := idx.Face(ignoredID)
	if !ok {
		t.Fatalf("ignored face %d missing", ignoredID)
	}
	if ignored.ClusterID.Valid {
		t.Errorf("ignored face %d still in cluster %d", ignoredID, ignored.ClusterID.Int64)
	}

	// Confirm another of A's faces as B: it must move into B's cluster.
	aFaces, err = idx.FacesOfCluster(aCluster.ID, cache.OrderPath)
	if err != nil || len(aFaces) == 0 {
		t.Fatalf("FacesOfCluster(A) after ignore: %d faces, %v", len(aFaces), err)
	}
	confirmedID := aFaces[0].ID
	if _, err := p.Retag([]int64{confirmedID}, cache.VerdictConfirmed, personB); err != nil {
		t.Fatalf("Retag(confirmed): %v", err)
	}
	confirmed, ok := idx.Face(confirmedID)
	if !ok {
		t.Fatalf("confirmed face %d missing", confirmedID)
	}
	if !confirmed.ClusterID.Valid || confirmed.ClusterID.Int64 != bClusterID {
		t.Errorf("confirmed face %d cluster = %+v, want B's cluster %d",
			confirmedID, confirmed.ClusterID, bClusterID)
	}
	if !confirmed.PersonID.Valid || confirmed.PersonID.Int64 != personB {
		t.Errorf("confirmed face %d person = %+v, want B (%d)", confirmedID, confirmed.PersonID, personB)
	}
}
