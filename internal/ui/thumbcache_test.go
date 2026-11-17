package ui

import (
	"container/list"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gioui.org/op/paint"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/scan"
)

func TestThumbBackoff(t *testing.T) {
	// Doubles from the base each attempt, then saturates at the max.
	cases := []struct {
		failCount int
		want      time.Duration
	}{
		{1, thumbRetryBase},
		{2, 2 * thumbRetryBase},
		{3, 4 * thumbRetryBase},
		{100, thumbRetryMax}, // long past the cap
	}
	for _, c := range cases {
		if got := thumbBackoff(c.failCount); got != c.want {
			t.Errorf("thumbBackoff(%d) = %v, want %v", c.failCount, got, c.want)
		}
	}
	if got := thumbBackoff(50); got > thumbRetryMax {
		t.Errorf("thumbBackoff(50) = %v exceeds cap %v", got, thumbRetryMax)
	}
}

// entrySnapshot reads the shard entry for path under its lock and returns a copy
// of the fields the failure-caching test asserts on.
func entrySnapshot(tc *thumbCache, path string) (exists, failed, queued bool, failCount int, retryAfter time.Time) {
	k := thumbKey{path: path, tier: tierSmall}
	sh := tc.shardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	el, ok := sh.entries[k]
	if !ok {
		return false, false, false, 0, time.Time{}
	}
	te := el.Value.(*thumbEntry)
	return true, te.failed, te.queued, te.failCount, te.retryAfter
}

// TestThumbCacheFailureBackoff verifies that a thumbnail whose generation fails
// is marked failed with a future retryAfter and is NOT re-queued on subsequent
// Get calls within the backoff window — the fix for the per-frame fork loop.
func TestThumbCacheFailureBackoff(t *testing.T) {
	store, err := cache.NewThumbStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tc := newThumbCache(store, func() {})

	// A photo entry pointing at non-image bytes: generation always fails via
	// the in-process decoder, no external tool involved.
	src := filepath.Join(t.TempDir(), "broken.jpg")
	if err := os.WriteFile(src, []byte("not a jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(src)
	e := cache.Entry{Path: src, Type: scan.TypePhoto, ThumbID: cache.ThumbIDFor(src), ModTime: info.ModTime()}

	// First Get queues a decode.
	if _, _, ok := tc.Get(e); ok {
		t.Fatal("Get reported ready for a broken thumb")
	}

	// Wait for the worker to record the failure.
	deadline := time.Now().Add(3 * time.Second)
	var failed, queued bool
	var failCount int
	var retryAfter time.Time
	for time.Now().Before(deadline) {
		_, failed, queued, failCount, retryAfter = entrySnapshot(tc, src)
		if failed && !queued {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !failed || queued {
		t.Fatalf("entry not marked failed: failed=%v queued=%v", failed, queued)
	}
	if failCount != 1 {
		t.Fatalf("failCount = %d, want 1", failCount)
	}
	if !retryAfter.After(time.Now()) {
		t.Fatalf("retryAfter = %v, want a future time", retryAfter)
	}

	// A subsequent Get inside the backoff window must NOT re-queue (this is what
	// stops the per-frame fork loop). failCount stays 1; queued stays false.
	if _, _, ok := tc.Get(e); ok {
		t.Fatal("Get reported ready during backoff")
	}
	_, failed, queued, failCount, _ = entrySnapshot(tc, src)
	if !failed || queued || failCount != 1 {
		t.Fatalf("re-queued during backoff: failed=%v queued=%v failCount=%d", failed, queued, failCount)
	}
}

// TestThumbLaneNewestFirst covers G1/G3 ordering: the newest visible request
// is served first, and prefetch requests only once no visible one is queued.
func TestThumbLaneNewestFirst(t *testing.T) {
	l := newThumbLane()
	l.push(thumbJob{key: thumbKey{path: "p1"}, prio: prioPrefetch})
	l.push(thumbJob{key: thumbKey{path: "a"}, prio: prioVisible})
	l.push(thumbJob{key: thumbKey{path: "b"}, prio: prioVisible})
	l.push(thumbJob{key: thumbKey{path: "p2"}, prio: prioPrefetch})
	var got []string
	for range 4 {
		got = append(got, l.pop().key.path)
	}
	want := []string{"b", "a", "p2", "p1"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pop order = %v, want %v", got, want)
		}
	}
}

// TestThumbClaimDropsStale covers G1: a queued request whose cell hasn't been
// requested for more than thumbStaleFrames grid frames is skipped and
// un-queued, so a later Get re-requests it instead of it being stuck.
func TestThumbClaimDropsStale(t *testing.T) {
	tc := &thumbCache{fast: newThumbLane(), gen: newThumbLane()}
	for i := range tc.shards {
		tc.shards[i].entries = map[thumbKey]*list.Element{}
		tc.shards[i].lru = list.New()
	}
	tc.capPerShard.Store(64)
	e := cache.Entry{Path: "/x/stale.jpg"}
	tc.get(e, tierSmall, prioVisible) // queues at frame 0
	j := tc.fast.pop()
	for range thumbStaleFrames + 1 {
		tc.BeginFrame()
	}
	if tc.claim(j) {
		t.Fatal("claimed a request that went stale")
	}
	if _, _, queued, _, _ := entrySnapshot(tc, e.Path); queued {
		t.Fatal("stale request left queued")
	}

	tc.get(e, tierSmall, prioVisible) // re-requested this frame
	if !tc.claim(tc.fast.pop()) {
		t.Fatal("fresh request not claimed")
	}
}

// TestThumbCacheGetLargeReadyAllocFree covers G4-07: a ready large-tier
// lookup is served from an entry already in the cache, so it must not allocate
// a per-call string key.
func TestThumbCacheGetLargeReadyAllocFree(t *testing.T) {
	tc := &thumbCache{fast: newThumbLane(), gen: newThumbLane()}
	for i := range tc.shards {
		tc.shards[i].entries = map[thumbKey]*list.Element{}
		tc.shards[i].lru = list.New()
	}
	tc.capPerShard.Store(64)

	p := "/x/large.jpg"
	k := tierKey(p, tierLarge)
	sh := tc.shardFor(k)
	sh.mu.Lock()
	te := tc.touchOrCreate(sh, k)
	te.ready = true
	te.size = image.Pt(8, 8)
	sh.mu.Unlock()

	e := cache.Entry{Path: p}
	if _, _, ok := tc.get(e, tierLarge, prioVisible); !ok {
		t.Fatal("ready large entry not reported ready")
	}
	if n := testing.AllocsPerRun(100, func() { tc.get(e, tierLarge, prioVisible) }); n != 0 {
		t.Fatalf("large-tier get allocates %v per call, want 0", n)
	}
}

// TestThumbCacheRegeneratesCorruptThumb covers G4's self-healing read: a
// thumbnail file that exists and looks fresh but doesn't decode is discarded
// and regenerated instead of the entry being marked failed.
func TestThumbCacheRegeneratesCorruptThumb(t *testing.T) {
	store, err := cache.NewThumbStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "ok.jpg")
	writeJPEGWithOrientation(t, src, 64, 48, 1)
	info, _ := os.Stat(src)
	e := cache.Entry{Path: src, Type: scan.TypePhoto, ThumbID: cache.ThumbIDFor(src), ModTime: info.ModTime()}
	thumbPath, err := store.Path(e)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(thumbPath, []byte("truncated garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	tc := newThumbCache(store, func() {})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, sz, ok := tc.Get(e); ok {
			if sz.X != 64 || sz.Y != 48 {
				t.Fatalf("regenerated thumb is %v, want 64x48", sz)
			}
			return
		}
		if _, failed, _, _, _ := entrySnapshot(tc, src); failed {
			t.Fatal("corrupt thumbnail marked failed instead of regenerated")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("thumbnail never became ready")
}

// newBareThumbCache builds a thumbCache with no workers or stores, just the
// shard state and capacity the LRU tests drive directly.
func newBareThumbCache(capPerShard int) *thumbCache {
	tc := &thumbCache{fast: newThumbLane(), gen: newThumbLane(), wake: make(chan struct{}, 1)}
	for i := range tc.shards {
		tc.shards[i].entries = map[thumbKey]*list.Element{}
		tc.shards[i].lru = list.New()
	}
	tc.capPerShard.Store(int64(capPerShard))
	return tc
}

// sameShardKeys returns n small-tier keys that all hash to one shard.
func sameShardKeys(tc *thumbCache, n int) []thumbKey {
	first := tierKey(fmt.Sprintf("/x/%d.jpg", 0), tierSmall)
	sh := tc.shardFor(first)
	keys := []thumbKey{first}
	for i := 1; len(keys) < n; i++ {
		k := tierKey(fmt.Sprintf("/x/%d.jpg", i), tierSmall)
		if tc.shardFor(k) == sh {
			keys = append(keys, k)
		}
	}
	return keys
}

// has reports whether k is present in its shard.
func has(tc *thumbCache, k thumbKey) bool {
	sh := tc.shardFor(k)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	_, ok := sh.entries[k]
	return ok
}

// TestThumbCacheEvictsLRU covers G3-10: over capacity, the least-recently-used
// shard entry is dropped and newer ones stay.
func TestThumbCacheEvictsLRU(t *testing.T) {
	tc := newBareThumbCache(2)
	ks := sameShardKeys(tc, 3)
	sh := tc.shardFor(ks[0])
	for _, k := range ks {
		sh.mu.Lock()
		tc.touchOrCreate(sh, k)
		sh.mu.Unlock()
	}
	if has(tc, ks[0]) {
		t.Error("LRU entry survived eviction")
	}
	if !has(tc, ks[1]) || !has(tc, ks[2]) {
		t.Error("recent entries were evicted")
	}
}

// TestThumbCacheTouchPromotesMRU covers G3-10: touching an existing entry makes
// it most-recently-used, so the next insertion evicts the other one.
func TestThumbCacheTouchPromotesMRU(t *testing.T) {
	tc := newBareThumbCache(2)
	ks := sameShardKeys(tc, 3)
	sh := tc.shardFor(ks[0])
	for _, k := range []thumbKey{ks[0], ks[1], ks[0]} {
		sh.mu.Lock()
		tc.touchOrCreate(sh, k)
		sh.mu.Unlock()
	}
	sh.mu.Lock()
	tc.touchOrCreate(sh, ks[2])
	sh.mu.Unlock()
	if has(tc, ks[1]) {
		t.Error("stale LRU entry survived after being touched")
	}
	if !has(tc, ks[0]) || !has(tc, ks[2]) {
		t.Error("touched entry or new entry missing")
	}
	sh.mu.Lock()
	front := sh.lru.Front().Value.(*thumbEntry).key
	sh.mu.Unlock()
	if front != ks[2] {
		t.Errorf("lru front = %v, want the last touched key %v", front, ks[2])
	}
}

// TestThumbCacheKeepsQueuedEntries covers G3-10: an in-flight (queued, not
// ready) entry is never evicted; the next-oldest ready entry goes instead.
func TestThumbCacheKeepsQueuedEntries(t *testing.T) {
	tc := newBareThumbCache(2)
	ks := sameShardKeys(tc, 3)
	sh := tc.shardFor(ks[0])
	sh.mu.Lock()
	te := tc.touchOrCreate(sh, ks[0])
	te.queued, te.ready = true, false
	sh.mu.Unlock()
	for _, k := range ks[1:] {
		sh.mu.Lock()
		tc.touchOrCreate(sh, k)
		sh.mu.Unlock()
	}
	if !has(tc, ks[0]) {
		t.Error("in-flight entry was evicted")
	}
	if has(tc, ks[1]) {
		t.Error("ready entry was not the eviction victim")
	}
}

// TestThumbCacheEnsureCapacityShrinks covers G3-10: EnsureCapacity raises the
// per-shard cap to the working set, floors it at the default, and a shrunk cap
// trims the shard back down on the next insertion.
func TestThumbCacheEnsureCapacityShrinks(t *testing.T) {
	tc := newBareThumbCache(1)
	tc.EnsureCapacity(100000)
	if got, want := tc.capPerShard.Load(), int64((100000+thumbCacheShards-1)/thumbCacheShards); got != want {
		t.Errorf("cap after EnsureCapacity(100000) = %d, want %d", got, want)
	}
	tc.EnsureCapacity(10)
	if got, want := tc.capPerShard.Load(), int64((thumbCacheCapacity+thumbCacheShards-1)/thumbCacheShards); got != want {
		t.Errorf("cap after EnsureCapacity(10) = %d, want the default %d", got, want)
	}

	ks := sameShardKeys(tc, 4)
	sh := tc.shardFor(ks[0])
	tc.capPerShard.Store(8)
	for _, k := range ks[:3] {
		sh.mu.Lock()
		tc.touchOrCreate(sh, k)
		sh.mu.Unlock()
	}
	tc.capPerShard.Store(2)
	sh.mu.Lock()
	tc.touchOrCreate(sh, ks[3])
	sh.mu.Unlock()
	sh.mu.Lock()
	n := len(sh.entries)
	sh.mu.Unlock()
	if n != 2 {
		t.Errorf("shard holds %d entries after shrink, want 2", n)
	}
}

// TestThumbCacheFinishReinsertsEvicted covers G3-10: a completed decode whose
// entry was evicted mid-flight is reinserted ready, sized and un-queued.
func TestThumbCacheFinishReinsertsEvicted(t *testing.T) {
	tc := newBareThumbCache(4)
	k := tierKey("/x/finished.jpg", tierSmall)
	if has(tc, k) {
		t.Fatal("key present before finish")
	}
	tc.finish(thumbJob{key: k}, paint.ImageOp{}, image.Pt(3, 3), true)
	sh := tc.shardFor(k)
	sh.mu.Lock()
	el, ok := sh.entries[k]
	if !ok {
		sh.mu.Unlock()
		t.Fatal("finish did not reinsert the evicted entry")
	}
	te := el.Value.(*thumbEntry)
	ready, queued, size := te.ready, te.queued, te.size
	sh.mu.Unlock()
	if !ready || queued || size != image.Pt(3, 3) {
		t.Errorf("reinserted entry ready=%v queued=%v size=%v, want ready, not queued, 3x3", ready, queued, size)
	}
}

// TestThumbCacheCoalescesInvalidates covers G3-10: a tight burst of finish
// calls collapses into one (or at most two) invalidate calls.
func TestThumbCacheCoalescesInvalidates(t *testing.T) {
	tc := newBareThumbCache(64)
	var n atomic.Int32
	tc.invalidate = func() { n.Add(1) }
	go tc.coalescer()
	// The coalescer is deliberately left running: closing tc.wake here would
	// race with finish's non-blocking send, so let the goroutine leak.

	for i := range 50 {
		k := tierKey(fmt.Sprintf("/x/coalesce/%d.jpg", i), tierSmall)
		tc.finish(thumbJob{key: k}, paint.ImageOp{}, image.Pt(1, 1), true)
	}
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := n.Load(); got < 1 || got > 2 {
		t.Errorf("invalidate called %d times for one burst, want 1 or 2", got)
	}
}
