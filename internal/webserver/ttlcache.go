package webserver

import (
	"sync"
	"time"
)

// ttlCache memoizes compute(key) for ttl, invalidating an entry early when the
// caller's generation (Index.Generation() for index-derived values) moves on.
// It backs the trash listing, subdir listings, year totals, per-view counts, child counts
// and HLS duration memo (W4-02).
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
	// swr enables stale-while-revalidate: an expired entry whose generation
	// still matches is returned immediately while at most one background
	// refresh runs through the existing inflight flight; a generation
	// mismatch or a missing entry still computes synchronously (W5-01).
	swr bool

	mu       sync.Mutex
	epoch    uint64 // bumped by clear; flights from an older epoch may not store
	m        map[K]ttlEntry[V]
	inflight map[K]*ttlFlight[V]
}

type ttlEntry[V any] struct {
	v   V
	at  time.Time
	gen uint64
}

type ttlFlight[V any] struct {
	done  chan struct{}
	gen   uint64
	epoch uint64 // c.epoch when the flight started; see finish
	v     V
	ok    bool // false when the leader's compute panicked
}

// clear drops every cached value and fences the flights already running: a
// refresh that started before the clear cannot resurrect a pre-clear value.
func (c *ttlCache[K, V]) clear() {
	c.mu.Lock()
	c.m = nil
	c.epoch++
	c.mu.Unlock()
}

// startFlightLocked registers a new flight for key. The caller must hold c.mu.
func (c *ttlCache[K, V]) startFlightLocked(key K, gen uint64) *ttlFlight[V] {
	f := &ttlFlight[V]{done: make(chan struct{}), gen: gen, epoch: c.epoch}
	if c.inflight == nil {
		c.inflight = make(map[K]*ttlFlight[V])
	}
	c.inflight[key] = f
	return f
}

// finish clears the flight, stores its value when the compute succeeded and
// the cache was not cleared in the meantime, and releases any followers.
func (c *ttlCache[K, V]) finish(key K, f *ttlFlight[V], gen uint64) {
	c.mu.Lock()
	if c.inflight[key] == f {
		delete(c.inflight, key)
	}
	if f.ok && f.epoch == c.epoch {
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
}

// refresh runs a stale-while-revalidate compute in the background. Unlike the
// synchronous leader it must not take the process down with it, so a panicking
// compute is recorded as a failed flight (its result is not stored) instead of
// propagating.
func (c *ttlCache[K, V]) refresh(key K, f *ttlFlight[V], gen uint64, compute func() V) {
	defer c.finish(key, f, gen)
	defer func() {
		if recover() != nil {
			f.ok = false
		}
	}()
	f.v = compute()
	f.ok = true
}

// get returns the cached value for key when it is younger than ttl and was
// computed at generation gen. With swr enabled, an expired entry whose
// generation still matches is returned immediately while one background flight
// recomputes it. Otherwise — swr disabled, a generation mismatch, or a missing
// entry — get runs compute outside the lock, stores the result and returns it.
func (c *ttlCache[K, V]) get(key K, gen uint64, compute func() V) V {
	c.mu.Lock()
	if e, ok := c.m[key]; ok && e.gen == gen {
		if time.Since(e.at) < c.ttl {
			c.mu.Unlock()
			return e.v
		}
		if c.swr {
			// Stale but still current-generation: hand the value back now and
			// let one background flight bring it up to date.
			if f, ok := c.inflight[key]; !ok || f.gen != gen || f.epoch != c.epoch {
				flight := c.startFlightLocked(key, gen)
				go c.refresh(key, flight, gen, compute)
			}
			c.mu.Unlock()
			return e.v
		}
	}
	// Join only a flight from the current epoch: one that started before a
	// clear computed a pre-clear value, which the caller must not see (W6-B1).
	if f, ok := c.inflight[key]; ok && f.gen == gen && f.epoch == c.epoch {
		c.mu.Unlock()
		<-f.done
		if f.ok {
			return f.v
		}
		// The leader panicked; compute for ourselves rather than hand back
		// a zero value.
		return compute()
	}
	// Lead. A flight for an older generation or epoch is simply superseded: its leader only deletes the map slot if it
	// still owns it.
	f := c.startFlightLocked(key, gen)
	c.mu.Unlock()

	defer c.finish(key, f, gen)
	f.v = compute()
	f.ok = true
	return f.v
}
