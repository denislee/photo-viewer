package ui

import (
	"container/list"
	"context"
	"image"
	_ "image/jpeg"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/op/paint"

	"github.com/dns/photo-viewer/internal/cache"
	"github.com/dns/photo-viewer/internal/thumb"
)

// thumbCacheCapacity is the floor on the number of decoded thumbnails kept in
// memory across all shards. Each entry holds an RGBA pixel buffer (~256 KB at
// the default thumb size), so 1024 entries ≈ 256 MB peak. This covers any
// normal viewport, but a large display (e.g. 4K) zoomed all the way out
// (minCellDp = 64) can put more than 1024 cells on screen at once. When that
// happens the cap is raised to the working set via EnsureCapacity so live
// cells aren't evicted mid-frame and re-decoded every frame (cache thrashing).
const thumbCacheCapacity = 1024

// thumbCacheShards is the number of independent shards. Sharding lets the
// Gio render goroutine call Get() for every visible cell while decode
// workers store their results in parallel, without all of them serializing
// on a single mutex. 16 is comfortably larger than typical CPU counts so
// shard contention is rare even under bursty scrolling.
const thumbCacheShards = 16

// A thumbnail whose generation fails (corrupt file, or a missing external tool
// in the supported degraded mode) must not be re-queued every frame: each
// attempt re-enters the thumbnail pipeline, and ThumbStore's failure memo
// (C5-06) only skips the external-tool fork — the request and its bookkeeping
// still cost a queue slot and a worker wake-up per frame. Instead a failed
// entry is marked and retried no sooner than an exponential backoff from
// thumbRetryBase up to thumbRetryMax, so a transient failure still heals while
// a permanent one costs one attempt per (growing) interval.
const (
	thumbRetryBase = 3 * time.Second
	thumbRetryMax  = 5 * time.Minute
)

// thumbStaleFrames is how many grid frames a queued request may go unrequested
// before workers drop it (G1). Cells scrolled past during a fast scroll or a
// G/gg jump stop calling Get, so their requests go stale and are skipped
// instead of being decoded ahead of what is on screen now.
const thumbStaleFrames = 2

// thumbStackCap bounds each request stack. Past it the oldest requests, the
// least likely to still be wanted, are dropped (and re-requested if seen again).
const thumbStackCap = 2048

// LargeThumbCellPx is the cell size in pixels above which the grid draws the
// LargeThumbSize tier instead of the 256 px one (G5).
const LargeThumbCellPx = 300

type thumbTier uint8

const (
	tierSmall thumbTier = iota
	tierLarge
)

// thumbKey is the thumbnail cache key: a path plus the tier it is cached at.
// A struct key avoids the path+"\x00large" string allocation that every
// GetLarge and Prefetch call used to pay (G4-07).
type thumbKey struct {
	path string
	tier thumbTier
}

type thumbPrio uint8

const (
	prioVisible  thumbPrio = iota // on screen now
	prioPrefetch                  // just outside the viewport (G3)
)

// thumbJob is one queued thumbnail request.
type thumbJob struct {
	e    cache.Entry
	key  thumbKey
	tier thumbTier
	prio thumbPrio
}

// thumbLane is a pair of newest-first request stacks (visible above
// prefetch) served by a pool of workers.
type thumbLane struct {
	mu     sync.Mutex
	cond   *sync.Cond
	stacks [2][]thumbJob
}

func newThumbLane() *thumbLane {
	l := &thumbLane{}
	l.cond = sync.NewCond(&l.mu)
	return l
}

// push adds j on top of its priority's stack and returns any requests that fell
// off the bottom of a full stack.
func (l *thumbLane) push(j thumbJob) []thumbJob {
	l.mu.Lock()
	st := append(l.stacks[j.prio], j)
	var dropped []thumbJob
	if n := len(st) - thumbStackCap; n > 0 {
		dropped = append(dropped, st[:n]...)
		st = append(st[:0], st[n:]...)
	}
	l.stacks[j.prio] = st
	l.mu.Unlock()
	l.cond.Signal()
	return dropped
}

// pop blocks until a request is available and returns the newest visible one,
// or the newest prefetch one when no visible request is queued.
func (l *thumbLane) pop() thumbJob {
	l.mu.Lock()
	defer l.mu.Unlock()
	for {
		for p := range l.stacks {
			if st := l.stacks[p]; len(st) > 0 {
				j := st[len(st)-1]
				st[len(st)-1] = thumbJob{}
				l.stacks[p] = st[:len(st)-1]
				return j
			}
		}
		l.cond.Wait()
	}
}

// thumbCache lazily resolves thumbnail JPEGs into paint.ImageOp values and
// caches them with an LRU policy. Multiple frames may request the same thumb
// concurrently; only one decode is queued per entry. When a decode completes,
// invalidate is fired so the next frame can paint the result.
//
// Requests go through two lanes (G2). The fast lane decodes thumbnails already
// on disk (a ~2 ms JPEG decode). One that is missing or stale moves to the
// generation lane, whose workers run ffmpeg/exiftool/heif-convert or the
// in-process resampler under the store's semaphores, so a burst of uncached
// files can't hold up cells whose thumbnails just need decoding. Both lanes
// serve the newest request first and drop requests that went stale (G1).
//
// State is split across thumbCacheShards independent shards keyed by a
// hash of the cache key; each shard has its own mutex, map, and LRU list.
type thumbCache struct {
	store *cache.ThumbStore
	// large is the LargeThumbSize tier, nil if it couldn't be created.
	large      *cache.ThumbStore
	invalidate func()
	// capPerShard is the per-shard LRU ceiling. Read under each shard's mutex
	// during eviction but written from the Gio layout goroutine via
	// EnsureCapacity, so it's atomic to avoid a data race.
	capPerShard atomic.Int64

	// frame counts grid layouts (BeginFrame); requests remember the frame
	// they were last wanted in.
	frame atomic.Uint64

	shards [thumbCacheShards]thumbCacheShard
	fast   *thumbLane
	gen    *thumbLane

	// Coalescing: workers set dirty and signal wake; the coalescer goroutine
	// wakes on the channel, sleeps 16 ms to batch concurrent completions, then
	// fires invalidate. When idle the coalescer blocks on wake (no ticker).
	dirty atomic.Bool
	wake  chan struct{}
}

type thumbCacheShard struct {
	mu      sync.Mutex
	entries map[thumbKey]*list.Element // value: *thumbEntry, list ordered MRU→LRU
	lru     *list.List
}

type thumbEntry struct {
	key    thumbKey
	ready  bool
	op     paint.ImageOp
	size   image.Point
	queued bool
	// working is set while a worker is decoding or generating the entry, so a
	// duplicate request (a prefetch promoted to visible) isn't run twice.
	working bool
	prio    thumbPrio
	// lastWanted is the frame of the latest Get/Prefetch for the entry.
	lastWanted uint64
	// failed is set when the last decode attempt failed; retryAfter is the
	// earliest time the entry may be re-queued, and failCount drives the
	// exponential backoff. A successful decode clears all three.
	failed     bool
	failCount  int
	retryAfter time.Time
}

func newThumbCache(store *cache.ThumbStore, invalidate func()) *thumbCache {
	tc := &thumbCache{
		store:      store,
		invalidate: invalidate,
		fast:       newThumbLane(),
		gen:        newThumbLane(),
		wake:       make(chan struct{}, 1),
	}
	if store != nil {
		if large, err := store.Tier(cache.LargeThumbDir, cache.LargeThumbSize); err == nil {
			tc.large = large
		}
	}
	tc.capPerShard.Store((thumbCacheCapacity + thumbCacheShards - 1) / thumbCacheShards)
	for i := range tc.shards {
		tc.shards[i].entries = make(map[thumbKey]*list.Element)
		tc.shards[i].lru = list.New()
	}
	for range max(runtime.NumCPU()/2, 2) {
		go tc.fastWorker()
	}
	for range max(runtime.NumCPU(), 4) {
		go tc.genWorker()
	}
	go tc.coalescer()
	return tc
}

// BeginFrame advances the request frame counter. The grid calls it once per
// layout; requests not renewed within thumbStaleFrames frames are dropped.
func (tc *thumbCache) BeginFrame() { tc.frame.Add(1) }

// EnsureCapacity sets the total LRU ceiling to accommodate at least `working`
// decoded thumbnails — the count the grid currently needs resident (visible
// cells plus a scroll buffer). Called once per layout frame from the single
// Gio layout goroutine, so a plain Store is safe. The cap is floored at
// thumbCacheCapacity; it shrinks freely so zooming back in after a 4K zoom-out
// lets the LRU reclaim the enlarged resident set as new entries push old ones out.
func (tc *thumbCache) EnsureCapacity(working int) {
	working = max(working, thumbCacheCapacity)
	per := int64((working + thumbCacheShards - 1) / thumbCacheShards)
	tc.capPerShard.Store(per)
}

// shardFor returns the shard responsible for key. FNV-1a over the path is
// cheap and produces good distribution for filesystem-path strings, which
// share long directory prefixes that a naive hash would bunch into one shard;
// the tier is then mixed in so the two tiers of one path land independently.
func (tc *thumbCache) shardFor(key thumbKey) *thumbCacheShard {
	var h uint32 = 2166136261
	for i := 0; i < len(key.path); i++ {
		h ^= uint32(key.path[i])
		h *= 16777619
	}
	h ^= uint32(key.tier)
	h *= 16777619
	return &tc.shards[h%thumbCacheShards]
}

// tierKey is the cache key for path at tier.
func tierKey(path string, tier thumbTier) thumbKey {
	return thumbKey{path, tier}
}

func (tc *thumbCache) storeFor(tier thumbTier) *cache.ThumbStore {
	if tier == tierLarge && tc.large != nil {
		return tc.large
	}
	return tc.store
}

// coalescer wakes the Gio frame loop when one or more thumbnail decodes have
// completed. It blocks on the wake channel while idle — no ticker fires when
// nothing is happening. When a worker signals wake, the coalescer sleeps 16 ms
// to coalesce any concurrent completions, then calls invalidate exactly once.
func (tc *thumbCache) coalescer() {
	for range tc.wake {
		time.Sleep(16 * time.Millisecond)
		if tc.dirty.Swap(false) && tc.invalidate != nil {
			tc.invalidate()
		}
	}
}

// Get returns the cached 256 px image op for entry if available. If not, the
// entry is queued for background decode and the bool is false. Subsequent
// frames after invalidate fires will see the populated op.
func (tc *thumbCache) Get(e cache.Entry) (paint.ImageOp, image.Point, bool) {
	return tc.get(e, tierSmall, prioVisible)
}

// GetLarge is Get for big cells: it returns the LargeThumbSize thumbnail when
// ready, and meanwhile the 256 px one (requesting both).
func (tc *thumbCache) GetLarge(e cache.Entry) (paint.ImageOp, image.Point, bool) {
	if tc.large == nil {
		return tc.Get(e)
	}
	if op, sz, ok := tc.get(e, tierLarge, prioVisible); ok {
		return op, sz, true
	}
	return tc.get(e, tierSmall, prioVisible)
}

// Prefetch requests e's thumbnail at low priority: it is decoded only when no
// on-screen request is waiting (G3).
func (tc *thumbCache) Prefetch(e cache.Entry, large bool) {
	tier := tierSmall
	if large && tc.large != nil {
		tier = tierLarge
	}
	tc.get(e, tier, prioPrefetch)
}

func (tc *thumbCache) get(e cache.Entry, tier thumbTier, prio thumbPrio) (paint.ImageOp, image.Point, bool) {
	key := tierKey(e.Path, tier)
	sh := tc.shardFor(key)
	sh.mu.Lock()
	te := tc.touchOrCreate(sh, key)
	te.lastWanted = tc.frame.Load()
	if te.ready {
		op, sz := te.op, te.size
		sh.mu.Unlock()
		return op, sz, true
	}
	// A previously-failed entry stays on the placeholder until its backoff
	// window elapses, so a broken file isn't re-forked every frame.
	if te.failed && time.Now().Before(te.retryAfter) {
		sh.mu.Unlock()
		return paint.ImageOp{}, image.Point{}, false
	}
	push := false
	if !te.queued {
		te.queued, te.prio, push = true, prio, true
	} else if prio < te.prio && !te.working {
		// A prefetched cell scrolled into view: queue it again on the
		// visible stack. Whichever copy a worker pops first runs; the
		// other is skipped.
		te.prio, push = prio, true
	}
	sh.mu.Unlock()
	if push {
		tc.enqueue(tc.fast, thumbJob{e: e, key: key, tier: tier, prio: prio})
	}
	return paint.ImageOp{}, image.Point{}, false
}

// enqueue pushes j onto lane and un-queues any requests that fell off the
// bottom of a full stack, so a later Get re-requests them.
func (tc *thumbCache) enqueue(lane *thumbLane, j thumbJob) {
	for _, d := range lane.push(j) {
		sh := tc.shardFor(d.key)
		sh.mu.Lock()
		if el := sh.entries[d.key]; el != nil {
			if te := el.Value.(*thumbEntry); te.queued && !te.working && te.prio == d.prio {
				te.queued = false
			}
		}
		sh.mu.Unlock()
	}
}

// claim checks a popped request against its entry and marks it working. It
// returns false, and clears queued for a stale request, when the request
// should be skipped: already done, already running, evicted, or no longer
// wanted on screen.
func (tc *thumbCache) claim(j thumbJob) bool {
	sh := tc.shardFor(j.key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	el := sh.entries[j.key]
	if el == nil {
		return false
	}
	te := el.Value.(*thumbEntry)
	if !te.queued || te.ready || te.working {
		return false
	}
	if tc.frame.Load()-te.lastWanted > thumbStaleFrames {
		te.queued = false
		return false
	}
	te.working = true
	return true
}

// release clears working on an entry handed from the fast lane to the
// generation lane, so the generation worker can claim it.
func (tc *thumbCache) release(j thumbJob) {
	sh := tc.shardFor(j.key)
	sh.mu.Lock()
	if el := sh.entries[j.key]; el != nil {
		el.Value.(*thumbEntry).working = false
	}
	sh.mu.Unlock()
}

// touchOrCreate returns the entry for key within sh, promoting it to MRU
// (creating it if missing). Caller must hold sh.mu. When the entry is
// already at the front the MoveToFront call is skipped — common on the
// hot path where the same cell is re-fetched every frame.
func (tc *thumbCache) touchOrCreate(sh *thumbCacheShard, key thumbKey) *thumbEntry {
	if el, ok := sh.entries[key]; ok {
		if sh.lru.Front() != el {
			sh.lru.MoveToFront(el)
		}
		return el.Value.(*thumbEntry)
	}
	te := &thumbEntry{key: key}
	el := sh.lru.PushFront(te)
	sh.entries[key] = el
	tc.evictIfNeeded(sh)
	return te
}

// evictIfNeeded drops the least-recently-used ready entries from sh until
// it's within capacity. Caller must hold sh.mu.
func (tc *thumbCache) evictIfNeeded(sh *thumbCacheShard) {
	cap := int(tc.capPerShard.Load())
	for sh.lru.Len() > cap {
		oldest := sh.lru.Back()
		if oldest == nil {
			return
		}
		te := oldest.Value.(*thumbEntry)
		if te.queued && !te.ready {
			// Don't evict an entry whose decode hasn't landed yet — when it
			// does, the worker will store into a stale *thumbEntry. Walk
			// back from the oldest to find an evictable candidate instead.
			prev := oldest.Prev()
			for prev != nil {
				cand := prev.Value.(*thumbEntry)
				if !cand.queued || cand.ready {
					sh.lru.Remove(prev)
					delete(sh.entries, cand.key)
					break
				}
				prev = prev.Prev()
			}
			if prev == nil {
				return // nothing evictable
			}
			continue
		}
		sh.lru.Remove(oldest)
		delete(sh.entries, te.key)
	}
}

// fastWorker serves the fast lane: decode thumbnails already on disk, and hand
// anything missing, stale or unreadable to the generation lane.
func (tc *thumbCache) fastWorker() {
	for {
		j := tc.fast.pop()
		if !tc.claim(j) {
			continue
		}
		store := tc.storeFor(j.tier)
		if store.Has(j.e) {
			if op, sz, ok := decodeThumbFile(store, j.e); ok {
				tc.finish(j, op, sz, true)
				continue
			}
		}
		tc.release(j)
		tc.enqueue(tc.gen, j)
	}
}

// genWorker serves the generation lane: generate the thumbnail (bounded by the
// store's semaphores) and decode it. A thumbnail that exists but doesn't
// decode — a zero-byte or truncated file a crash left behind, since the store
// no longer fsyncs (G4) — is discarded and regenerated once before the entry
// is marked failed.
func (tc *thumbCache) genWorker() {
	for {
		j := tc.gen.pop()
		if !tc.claim(j) {
			continue
		}
		store := tc.storeFor(j.tier)
		op, sz, ok := decodeThumbFile(store, j.e)
		if !ok && store.Has(j.e) {
			store.Discard(j.e.ThumbID)
			op, sz, ok = decodeThumbFile(store, j.e)
		}
		tc.finish(j, op, sz, ok)
	}
}

// finish records a request's outcome on its entry.
func (tc *thumbCache) finish(j thumbJob, op paint.ImageOp, sz image.Point, ok bool) {
	sh := tc.shardFor(j.key)
	sh.mu.Lock()
	el := sh.entries[j.key]
	var te *thumbEntry
	if el == nil {
		// Evicted while in-flight; reinsert at MRU.
		te = &thumbEntry{key: j.key}
		el = sh.lru.PushFront(te)
		sh.entries[j.key] = el
		tc.evictIfNeeded(sh)
	} else {
		te = el.Value.(*thumbEntry)
	}
	te.queued = false
	te.working = false
	if ok {
		te.op = op
		te.size = sz
		te.ready = true
		te.failed = false
		te.failCount = 0
	} else {
		te.failCount++
		te.failed = true
		te.retryAfter = time.Now().Add(thumbBackoff(te.failCount))
	}
	sh.mu.Unlock()
	if ok {
		tc.dirty.Store(true)
		select {
		case tc.wake <- struct{}{}:
		default: // coalescer already has a pending wake signal
		}
	}
}

// thumbBackoff returns the retry delay for the failCount-th consecutive decode
// failure: thumbRetryBase doubled each attempt, capped at thumbRetryMax.
func thumbBackoff(failCount int) time.Duration {
	d := thumbRetryBase
	for i := 1; i < failCount; i++ {
		d *= 2
		if d >= thumbRetryMax {
			return thumbRetryMax
		}
	}
	return d
}

// decodeThumbFile returns e's thumbnail from store, generating it first if it
// is missing or stale. Thumbnails are JPEGs, so libjpeg-turbo decodes them
// straight to RGBA and Gio uploads them without another copy.
func decodeThumbFile(store *cache.ThumbStore, e cache.Entry) (paint.ImageOp, image.Point, bool) {
	path, err := store.Path(e)
	if err != nil || path == "" {
		return paint.ImageOp{}, image.Point{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	img, err := thumb.DecodeScaled(context.Background(), data, 0, 0)
	if err != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	op := paint.NewImageOp(img)
	op.Filter = paint.FilterLinear
	return op, img.Bounds().Size(), true
}
