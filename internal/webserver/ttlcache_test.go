package webserver

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/cache"
)

// TestTTLCacheSlowKeyDoesNotBlockOthers verifies that compute runs outside the
// cache lock: a slow recompute of one key must not stall another key (W3-01).
func TestTTLCacheSlowKeyDoesNotBlockOthers(t *testing.T) {
	c := &ttlCache[string, int]{ttl: time.Minute}
	started := make(chan struct{})
	release := make(chan struct{})
	go c.get("a", 0, func() int {
		close(started)
		<-release
		return 1
	})
	<-started

	done := make(chan int, 1)
	go func() { done <- c.get("b", 0, func() int { return 2 }) }()

	select {
	case got := <-done:
		if got != 2 {
			t.Fatalf("get(b) = %d, want 2", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("key b blocked behind key a's compute")
	}
	close(release)
}

// TestTTLCacheCollapsesConcurrentColdLookups verifies concurrent cold lookups
// of the same key collapse onto a single compute and share its result.
func TestTTLCacheCollapsesConcurrentColdLookups(t *testing.T) {
	c := &ttlCache[string, int]{ttl: time.Minute}
	var calls atomic.Int32
	release := make(chan struct{})
	compute := func() int {
		calls.Add(1)
		<-release
		return 42
	}

	const n = 32
	var wg sync.WaitGroup
	results := make([]int, n)
	wg.Add(n)
	for i := range n {
		go func() {
			defer wg.Done()
			results[i] = c.get("k", 0, compute)
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("compute ran %d times, want 1 (cold lookups did not collapse)", got)
	}
	for i, got := range results {
		if got != 42 {
			t.Errorf("result[%d] = %d, want 42", i, got)
		}
	}
}

// TestTTLCacheGenerationAndTTL verifies the cache invalidates on a generation
// change and expires entries past the TTL.
func TestTTLCacheGenerationAndTTL(t *testing.T) {
	c := &ttlCache[string, int]{ttl: 50 * time.Millisecond}
	var calls atomic.Int32

	if got := c.get("k", 1, func() int { calls.Add(1); return 1 }); got != 1 {
		t.Fatalf("first get = %d, want 1", got)
	}
	if got := c.get("k", 1, func() int { calls.Add(1); return 99 }); got != 1 {
		t.Fatalf("same-generation get = %d, want cached 1", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d after cache hit, want 1", calls.Load())
	}

	if got := c.get("k", 2, func() int { calls.Add(1); return 1 }); got != 1 {
		t.Fatalf("new-generation get = %d, want 1", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d after generation change, want 2", calls.Load())
	}

	time.Sleep(60 * time.Millisecond)
	if got := c.get("k", 2, func() int { calls.Add(1); return 1 }); got != 1 {
		t.Fatalf("post-TTL get = %d, want 1", got)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d after TTL expiry, want 3", calls.Load())
	}
}

// TestTTLCacheLeaderPanicReleasesFollowers verifies a panicking leader still
// releases its flight so followers compute for themselves instead of hanging.
func TestTTLCacheLeaderPanicReleasesFollowers(t *testing.T) {
	c := &ttlCache[string, int]{ttl: time.Minute}
	release := make(chan struct{})

	go func() {
		defer func() { _ = recover() }()
		c.get("k", 0, func() int {
			<-release
			panic("boom")
		})
	}()

	time.Sleep(20 * time.Millisecond)

	done := make(chan int, 1)
	go func() { done <- c.get("k", 0, func() int { return 7 }) }()

	close(release)

	select {
	case got := <-done:
		if got != 7 {
			t.Fatalf("follower get = %d, want 7", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("follower hung after the leader panicked")
	}
}

// TestCachedCountViewSharesSidebarCounts verifies rootSidebarAgg's All total
// goes through the shared view-count cache (W3-01).
func TestCachedCountViewSharesSidebarCounts(t *testing.T) {
	s, _, cleanup := fixture(t, 5)
	defer cleanup()

	s.rootSidebarAgg("All", true)

	key := cache.View{Kind: "all", Filter: "All", ShowRAW: true}
	s.viewCounts.mu.Lock()
	defer s.viewCounts.mu.Unlock()
	e, ok := s.viewCounts.m[key]
	if !ok {
		t.Fatalf("viewCounts has no entry for %+v", key)
	}
	if e.v != 5 {
		t.Errorf("cached count = %d, want 5", e.v)
	}
}
