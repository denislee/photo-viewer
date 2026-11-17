package ui

import (
	"container/list"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	sh := tc.shardFor(path)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	el, ok := sh.entries[path]
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
	l.push(thumbJob{key: "p1", prio: prioPrefetch})
	l.push(thumbJob{key: "a", prio: prioVisible})
	l.push(thumbJob{key: "b", prio: prioVisible})
	l.push(thumbJob{key: "p2", prio: prioPrefetch})
	var got []string
	for range 4 {
		got = append(got, l.pop().key)
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
		tc.shards[i].entries = map[string]*list.Element{}
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
