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

// TestTTLCacheSWRServesStaleWithoutBlocking verifies that past the TTL a
// stale-while-revalidate cache hands back the stale value immediately, kicks a
// single background refresh, and picks the fresh value up on a later lookup
// (W5-01).
func TestTTLCacheSWRServesStaleWithoutBlocking(t *testing.T) {
	c := &ttlCache[string, int]{ttl: 10 * time.Millisecond, swr: true}
	var refreshes atomic.Int32

	if got := c.get("k", 1, func() int { return 1 }); got != 1 {
		t.Fatalf("first get = %d, want 1", got)
	}
	time.Sleep(20 * time.Millisecond)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	compute := func() int {
		refreshes.Add(1)
		once.Do(func() { close(started) })
		<-release
		return 2
	}

	done := make(chan int, 1)
	go func() { done <- c.get("k", 1, compute) }()
	select {
	case got := <-done:
		if got != 1 {
			t.Fatalf("stale get = %d, want stale 1", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("stale get blocked on the refresh compute")
	}

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("no background refresh started")
	}

	// A concurrent stale lookup must not block and must not start a second
	// refresh: the inflight flight is shared.
	done2 := make(chan int, 1)
	go func() { done2 <- c.get("k", 1, compute) }()
	select {
	case got := <-done2:
		if got != 1 {
			t.Fatalf("concurrent stale get = %d, want stale 1", got)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("concurrent stale get blocked on the refresh compute")
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh compute ran %d times while blocked, want 1", got)
	}

	close(release)

	// Poll until the background refresh has landed its value.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		e, ok := c.m["k"]
		var v int
		if ok {
			v = e.v
		}
		inflight := len(c.inflight)
		c.mu.Unlock()
		if ok && v == 2 && inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background refresh did not store 2 (present=%v value=%d inflight=%d)", ok, v, inflight)
		}
		time.Sleep(time.Millisecond)
	}
	if got := refreshes.Load(); got != 1 {
		t.Fatalf("refresh compute ran %d times, want 1", got)
	}
	if got := c.get("k", 1, func() int { return 3 }); got != 2 {
		t.Fatalf("post-refresh get = %d, want the refreshed 2", got)
	}
}

// TestTTLCacheSWRGenerationMismatchBlocks verifies that a generation change
// still forces a synchronous recompute even with SWR on: a stale value from an
// older generation must never be served.
func TestTTLCacheSWRGenerationMismatchBlocks(t *testing.T) {
	c := &ttlCache[string, int]{ttl: 10 * time.Millisecond, swr: true}

	if got := c.get("k", 1, func() int { return 1 }); got != 1 {
		t.Fatalf("gen-1 get = %d, want 1", got)
	}
	time.Sleep(20 * time.Millisecond)

	var calls atomic.Int32
	if got := c.get("k", 2, func() int { calls.Add(1); return 2 }); got != 2 {
		t.Fatalf("gen-2 get = %d, want 2 (synchronous recompute)", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("compute ran %d times, want 1", got)
	}
}

// TestTTLCacheClearDropsAndFencesInflight verifies clear drops the map and that
// a refresh already in flight cannot resurrect a pre-clear value.
func TestTTLCacheClearDropsAndFencesInflight(t *testing.T) {
	c := &ttlCache[string, int]{ttl: 10 * time.Millisecond, swr: true}

	if got := c.get("k", 1, func() int { return 1 }); got != 1 {
		t.Fatalf("first get = %d, want 1", got)
	}
	time.Sleep(20 * time.Millisecond)

	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	if got := c.get("k", 1, func() int {
		once.Do(func() { close(started) })
		<-release
		return 2
	}); got != 1 {
		t.Fatalf("stale get = %d, want stale 1", got)
	}
	<-started

	c.clear()
	close(release)

	// Wait for the fenced refresh to finish, then assert it stored nothing.
	deadline := time.Now().Add(2 * time.Second)
	for {
		c.mu.Lock()
		_, ok := c.m["k"]
		inflight := len(c.inflight)
		c.mu.Unlock()
		if !ok && inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pre-clear refresh leaked into the map (present=%v inflight=%d)", ok, inflight)
		}
		time.Sleep(time.Millisecond)
	}

	var calls atomic.Int32
	if got := c.get("k", 1, func() int { calls.Add(1); return 3 }); got != 3 {
		t.Fatalf("post-clear get = %d, want a fresh synchronous 3", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("compute ran %d times after clear, want 1", got)
	}
}

// TestTTLCacheClearDoesNotJoinPreClearFlight: a get after clear, while the
// pre-clear refresh is still blocked, must compute fresh rather than wait for
// and return the pre-clear value (W6-B1).
func TestTTLCacheClearDoesNotJoinPreClearFlight(t *testing.T) {
	c := &ttlCache[string, int]{ttl: 10 * time.Millisecond, swr: true}
	c.get("k", 1, func() int { return 1 })
	time.Sleep(20 * time.Millisecond)

	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	c.get("k", 1, func() int { close(started); <-release; return 2 })
	<-started

	c.clear()
	done := make(chan int, 1)
	go func() { done <- c.get("k", 1, func() int { return 3 }) }()
	select {
	case got := <-done:
		if got != 3 {
			t.Fatalf("post-clear get = %d, want fresh 3", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-clear get joined the blocked pre-clear flight")
	}
}

// TestTTLCacheSWRBackgroundPanicRecovered verifies a panicking background
// refresh does not take the process down and stores no value, so a later lookup
// recomputes.
func TestTTLCacheSWRBackgroundPanicRecovered(t *testing.T) {
	c := &ttlCache[string, int]{ttl: 10 * time.Millisecond, swr: true}

	if got := c.get("k", 1, func() int { return 1 }); got != 1 {
		t.Fatalf("first get = %d, want 1", got)
	}
	time.Sleep(20 * time.Millisecond)

	if got := c.get("k", 1, func() int { panic("boom") }); got != 1 {
		t.Fatalf("stale get = %d, want stale 1", got)
	}

	deadline := time.Now().Add(2 * time.Second)
	var calls atomic.Int32
	for {
		got := c.get("k", 1, func() int { calls.Add(1); return 2 })
		if got == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("get never returned the recomputed 2 (last %d)", got)
		}
		time.Sleep(time.Millisecond)
	}
	if got := calls.Load(); got == 0 {
		t.Fatal("recompute never ran after the panicking refresh")
	}
}

// TestCachedCountViewSharesSidebarCounts verifies sidebarAggregate's All total
// goes through the shared view-count cache (W3-01).
func TestCachedCountViewSharesSidebarCounts(t *testing.T) {
	s, _, cleanup := fixture(t, 5)
	defer cleanup()

	s.sidebarAggregate("All", true)

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
