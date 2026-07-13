package face

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

// testIndex opens a throwaway on-disk index. acceptJob's freshness check
// dereferences the index, so the pipeline needs a real (empty) one.
func testIndex(t *testing.T) *cache.Index {
	t.Helper()
	idx, err := cache.Load(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("cache.Load: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func photoJob() Job {
	return Job{Entry: cache.Entry{Type: scan.TypePhoto, Path: "/x.jpg"}, ThumbMod: 1}
}

// TestAssignAndPersistClustersAndReleasesLock exercises the restructured
// clustering critical section (S-18). It checks that the greedy assignment is
// unchanged AND that clusterMu is released on every exit — the normal success
// path and the DB-error early return, both of which used to be manual Unlock()
// calls and are now subsumed by a single deferred Unlock.
func TestAssignAndPersistClustersAndReleasesLock(t *testing.T) {
	idx := testIndex(t)
	p := NewPipeline(idx, nil)

	assertUnlocked := func(when string) {
		t.Helper()
		if !p.clusterMu.TryLock() {
			t.Fatalf("clusterMu still held after %s", when)
		}
		p.clusterMu.Unlock()
	}

	// Two well-separated faces in one image => two new clusters, changed=true.
	job := func(path string) Job {
		return Job{Entry: cache.Entry{Type: scan.TypePhoto, Path: path}, ThumbMod: 1}
	}
	if changed := p.assignAndPersist(job("/a.jpg"), []Detection{
		{BBox: [4]int{0, 0, 10, 10}, Embedding: axisVec(0)},
		{BBox: [4]int{0, 0, 10, 10}, Embedding: axisVec(5)},
	}); !changed {
		t.Fatal("two new clusters should report changed=true")
	}
	assertUnlocked("first write")
	if got := len(idx.AllClusters()); got != 2 {
		t.Fatalf("expected 2 clusters after two separated faces, got %d", got)
	}

	// A near-duplicate of cluster 0 merges into it: no new cluster, changed=false.
	if changed := p.assignAndPersist(job("/b.jpg"), []Detection{
		{BBox: [4]int{0, 0, 10, 10}, Embedding: axisVec(0.1)},
	}); changed {
		t.Fatal("merging into an existing cluster should report changed=false")
	}
	assertUnlocked("merge write")
	if got := len(idx.AllClusters()); got != 2 {
		t.Fatalf("near-duplicate must merge, expected 2 clusters, got %d", got)
	}

	// DB-error early return: prime the cache so AllClusters isn't re-queried on a
	// closed DB, close the index to force WriteFacesForPath to fail, then confirm
	// the deferred unlock still fires and the cache is invalidated.
	p.clusterMu.Lock()
	p.clusterCacheOK = true
	p.clusterMu.Unlock()
	_ = idx.Close()
	if changed := p.assignAndPersist(job("/c.jpg"), []Detection{
		{BBox: [4]int{0, 0, 10, 10}, Embedding: axisVec(9)},
	}); changed {
		t.Fatal("a failed DB write must report changed=false")
	}
	assertUnlocked("failed write")
	p.clusterMu.Lock()
	if p.clusterCacheOK || p.cachedClusters != nil {
		p.clusterMu.Unlock()
		t.Fatal("failed DB write should invalidate the cluster cache")
	}
	p.clusterMu.Unlock()
}

// TestSubmitBlockingDoesNotHangWithoutHelper covers the "every worker's
// spawnDaemon failed, so nothing consumes the queue" case. Without the
// live-worker check, SubmitBlocking would park forever on a channel no one
// reads. We force enabled=true so Start actually launches workers; each then
// fails spawnDaemon (helper absent) and exits, driving liveWorkers to 0, after
// which SubmitBlocking must return false instead of blocking.
func TestSubmitBlockingDoesNotHangWithoutHelper(t *testing.T) {
	if Available() {
		t.Skip("pv-face-detect is installed; this test needs the no-consumer path")
	}
	p := NewPipeline(testIndex(t), nil)
	p.enabled.Store(true) // pretend the helper works so Start spawns workers
	p.Start(context.Background())
	defer p.Stop()

	// Wait for all workers to fail spawnDaemon and exit (liveWorkers -> 0).
	deadline := time.Now().Add(3 * time.Second)
	for p.liveWorkers.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("workers did not exit after failing to spawn the helper")
		}
		time.Sleep(2 * time.Millisecond)
	}

	done := make(chan bool, 1)
	go func() { done <- p.SubmitBlocking(context.Background(), photoJob()) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("SubmitBlocking accepted a job with no live workers")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SubmitBlocking blocked forever with no consumers")
	}
}

// TestDisabledPipelineIsNoOpAndStopSafe verifies the graceful-degrade contract:
// with the helper absent the pipeline is disabled, so Start/Submit/Stop are safe
// no-ops even under concurrency and Stop returns promptly.
func TestDisabledPipelineIsNoOpAndStopSafe(t *testing.T) {
	p := NewPipeline(testIndex(t), nil)
	if p.Enabled() {
		t.Skip("pv-face-detect is installed; this test targets the disabled path")
	}
	p.Start(context.Background()) // no-op

	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			p.Submit(photoJob())
			p.SubmitBlocking(context.Background(), photoJob())
		})
	}

	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop on a disabled pipeline did not return promptly")
	}
	wg.Wait()
}

// TestConcurrentSubmitDuringStopNoPanic exercises the shutdown teardown against
// a storm of live senders — the surface where the send-on-closed-channel panic
// used to live. The real daemon workers can't run headlessly (pv-face-detect is
// absent), so we wire up the same runtime state Start would and use stand-in
// consumers whose shutdown select mirrors the real worker (ctx / quit / jobs).
// Because Stop closes the private quit channel and never closes jobs, concurrent
// Submit/SubmitBlocking can't panic; this locks that invariant in.
func TestConcurrentSubmitDuringStopNoPanic(t *testing.T) {
	p := NewPipeline(testIndex(t), nil)
	p.enabled.Store(true)

	ctx, cancel := context.WithCancel(context.Background())
	const n = 4
	jobs := make(chan Job, n*4)
	quit := make(chan struct{})
	p.mu.Lock()
	p.jobs = jobs
	p.quit = quit
	p.cancel = cancel
	p.mu.Unlock()
	p.liveWorkers.Store(n)
	for range n {
		p.wg.Go(func() {
			defer p.liveWorkers.Add(-1)
			for {
				select {
				case <-ctx.Done():
					return
				case <-quit:
					return
				case <-jobs:
				}
			}
		})
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 300 {
				p.Submit(photoJob())
				_ = p.SubmitBlocking(ctx, photoJob())
			}
		})
	}

	done := make(chan struct{})
	go func() { p.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return promptly under concurrent submits")
	}
	wg.Wait()
}
