package webserver

import (
	"sync"
	"time"
)

// ttlCache memoizes compute(key) for ttl, invalidating an entry early when the
// caller's generation (Index.Generation() for index-derived values) moves on.
// It replaces three hand-rolled copies of the same TTL + generation + mutex
// logic (sidebar aggregate, per-view counts, trash listing; W4-02).
//
// The lock is only held to read or write the map, never across compute, so a
// slow recompute for one key no longer stalls lookups of unrelated keys (W3-01).
// Concurrent cold lookups of the same key collapse onto a single compute: the
// first caller leads, the rest wait on its flight and share the result (the
// ThumbStore.flightMu shape).
//
// The zero value is not usable; set ttl (and optionally sweepAt) first.
type ttlCache[K comparable, V any] struct {
	ttl time.Duration
	// sweepAt, when > 0, bounds the map: once it holds more than sweepAt
	// entries, the insert path drops every entry the TTL has already made
	// stale in one pass (W-07). Keys that never repeat (dir views) would
	// otherwise accumulate forever, since the TTL only marks entries stale.
	sweepAt int

	mu       sync.Mutex
	m        map[K]ttlEntry[V]
	inflight map[K]*ttlFlight[V]
}

type ttlEntry[V any] struct {
	v   V
	at  time.Time
	gen uint64
}

type ttlFlight[V any] struct {
	done chan struct{}
	gen  uint64
	v    V
	ok   bool // false when the leader's compute panicked
}

// get returns the cached value for key when it is younger than ttl and was
// computed at generation gen; otherwise it runs compute (outside the lock),
// stores the result and returns it.
func (c *ttlCache[K, V]) get(key K, gen uint64, compute func() V) V {
	c.mu.Lock()
	if e, ok := c.m[key]; ok && e.gen == gen && time.Since(e.at) < c.ttl {
		c.mu.Unlock()
		return e.v
	}
	if f, ok := c.inflight[key]; ok && f.gen == gen {
		c.mu.Unlock()
		<-f.done
		if f.ok {
			return f.v
		}
		// The leader panicked; compute for ourselves rather than hand back
		// a zero value.
		return compute()
	}
	// Lead. A flight for an older generation (the index was cleared while it
	// ran) is simply superseded: its leader only deletes the map slot if it
	// still owns it.
	f := &ttlFlight[V]{done: make(chan struct{}), gen: gen}
	if c.inflight == nil {
		c.inflight = make(map[K]*ttlFlight[V])
	}
	c.inflight[key] = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		if c.inflight[key] == f {
			delete(c.inflight, key)
		}
		if f.ok {
			now := time.Now()
			if c.m == nil {
				c.m = make(map[K]ttlEntry[V])
			}
			if c.sweepAt > 0 && len(c.m) > c.sweepAt {
				for k, e := range c.m {
					if now.Sub(e.at) >= c.ttl {
						delete(c.m, k)
					}
				}
			}
			c.m[key] = ttlEntry[V]{v: f.v, at: now, gen: gen}
		}
		c.mu.Unlock()
		close(f.done)
	}()
	f.v = compute()
	f.ok = true
	return f.v
}
