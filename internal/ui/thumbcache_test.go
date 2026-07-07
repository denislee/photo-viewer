package ui

import (
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
