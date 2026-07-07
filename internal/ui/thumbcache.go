package ui

import (
	"container/list"
	"image"
	_ "image/jpeg"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"gioui.org/op/paint"

	"github.com/dns/photo-viewer/internal/cache"
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
// in the supported degraded mode) must not be re-queued every frame — that
// re-runs ThumbStore.Path, which for RAW/HEIC/video forks exiftool/heif-convert/
// ffmpeg once per frame per broken file, a constant fork loop that pegs a core.
// Instead a failed entry is marked and retried no sooner than an exponential
// backoff from thumbRetryBase up to thumbRetryMax, so a transient failure still
// heals while a permanent one costs one attempt per (growing) interval.
const (
	thumbRetryBase = 3 * time.Second
	thumbRetryMax  = 5 * time.Minute
)

// thumbCache lazily resolves thumbnail JPEGs into paint.ImageOp values and
// caches them with an LRU policy. Multiple frames may request the same thumb
// concurrently; only one decode is queued per entry. When a decode completes,
// invalidate is fired so the next frame can paint the result.
//
// State is split across thumbCacheShards independent shards keyed by a
// hash of the thumbnail path; each shard has its own mutex, map, and LRU
// list. The decode queue and the coalescer remain singletons since they're
// inherently global.
type thumbCache struct {
	store      *cache.ThumbStore
	invalidate func()
	// capPerShard is the per-shard LRU ceiling. Read under each shard's mutex
	// during eviction but written from the Gio layout goroutine via
	// EnsureCapacity, so it's atomic to avoid a data race. Grow-only.
	capPerShard atomic.Int64

	shards [thumbCacheShards]thumbCacheShard
	queue  chan cache.Entry

	// Coalescing: workers bump dirty; a single coalescer goroutine fires
	// invalidate at most once every ~16ms so a burst of decodes doesn't
	// hammer the Gio frame loop with one redraw per thumbnail.
	dirty atomic.Bool
}

type thumbCacheShard struct {
	mu      sync.Mutex
	entries map[string]*list.Element // value: *thumbEntry, list ordered MRU→LRU
	lru     *list.List
}

type thumbEntry struct {
	path   string
	ready  bool
	op     paint.ImageOp
	size   image.Point
	queued bool
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
		queue:      make(chan cache.Entry, 256),
	}
	tc.capPerShard.Store((thumbCacheCapacity + thumbCacheShards - 1) / thumbCacheShards)
	for i := range tc.shards {
		tc.shards[i].entries = make(map[string]*list.Element)
		tc.shards[i].lru = list.New()
	}
	workers := runtime.NumCPU()
	if workers < 4 {
		workers = 4
	}
	for i := 0; i < workers; i++ {
		go tc.worker()
	}
	go tc.coalescer()
	return tc
}

// EnsureCapacity raises the total LRU ceiling so it can hold at least
// `working` decoded thumbnails — the count the grid currently needs resident
// (visible cells plus a scroll buffer). Called once per layout frame. It only
// ever grows the cap (never below thumbCacheCapacity) so zooming out on a big
// display stops the cache from evicting cells it's about to repaint, while
// zooming back in doesn't churn the cap up and down.
func (tc *thumbCache) EnsureCapacity(working int) {
	working = max(working, thumbCacheCapacity)
	per := int64((working + thumbCacheShards - 1) / thumbCacheShards)
	for {
		cur := tc.capPerShard.Load()
		if per <= cur {
			return
		}
		if tc.capPerShard.CompareAndSwap(cur, per) {
			return
		}
	}
}

// shardFor returns the shard responsible for path. FNV-1a is cheap and
// produces good distribution for filesystem-path strings, which share long
// directory prefixes that a naive hash would bunch into one shard.
func (tc *thumbCache) shardFor(path string) *thumbCacheShard {
	var h uint32 = 2166136261
	for i := 0; i < len(path); i++ {
		h ^= uint32(path[i])
		h *= 16777619
	}
	return &tc.shards[h%thumbCacheShards]
}

// coalescer wakes the Gio frame loop at most ~60 times per second when one or
// more decodes have completed since the last wake. This avoids per-thumbnail
// invalidate storms when the user scrolls quickly and dozens of workers
// finish decoding at roughly the same instant.
func (tc *thumbCache) coalescer() {
	t := time.NewTicker(16 * time.Millisecond)
	defer t.Stop()
	for range t.C {
		if tc.dirty.Swap(false) && tc.invalidate != nil {
			tc.invalidate()
		}
	}
}

// Get returns the cached image op for entry if available. If not, the entry is
// queued for background decode and the bool is false. Subsequent frames after
// invalidate fires will see the populated op.
func (tc *thumbCache) Get(e cache.Entry) (paint.ImageOp, image.Point, bool) {
	sh := tc.shardFor(e.Path)
	sh.mu.Lock()
	te := tc.touchOrCreate(sh, e.Path)
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
	if !te.queued {
		te.queued = true
		sh.mu.Unlock()
		select {
		case tc.queue <- e:
		default:
			// queue full; drop and let a later frame retry by clearing queued.
			sh.mu.Lock()
			te.queued = false
			sh.mu.Unlock()
		}
		return paint.ImageOp{}, image.Point{}, false
	}
	sh.mu.Unlock()
	return paint.ImageOp{}, image.Point{}, false
}

// touchOrCreate returns the entry for path within sh, promoting it to MRU
// (creating it if missing). Caller must hold sh.mu. When the entry is
// already at the front the MoveToFront call is skipped — common on the
// hot path where the same cell is re-fetched every frame.
func (tc *thumbCache) touchOrCreate(sh *thumbCacheShard, path string) *thumbEntry {
	if el, ok := sh.entries[path]; ok {
		if sh.lru.Front() != el {
			sh.lru.MoveToFront(el)
		}
		return el.Value.(*thumbEntry)
	}
	te := &thumbEntry{path: path}
	el := sh.lru.PushFront(te)
	sh.entries[path] = el
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
					delete(sh.entries, cand.path)
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
		delete(sh.entries, te.path)
	}
}

func (tc *thumbCache) worker() {
	for e := range tc.queue {
		// Decode happens entirely outside the lock — paint.NewImageOp can
		// be expensive (image snapshot) and must not block other Get
		// callers on this shard.
		op, sz, ok := tc.decode(e)
		sh := tc.shardFor(e.Path)
		sh.mu.Lock()
		el := sh.entries[e.Path]
		var te *thumbEntry
		if el == nil {
			// Evicted while in-flight; reinsert at MRU.
			te = &thumbEntry{path: e.Path}
			el = sh.lru.PushFront(te)
			sh.entries[e.Path] = el
			tc.evictIfNeeded(sh)
		} else {
			te = el.Value.(*thumbEntry)
		}
		te.queued = false
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

func (tc *thumbCache) decode(e cache.Entry) (paint.ImageOp, image.Point, bool) {
	path, err := tc.store.Path(e)
	if err != nil || path == "" {
		return paint.ImageOp{}, image.Point{}, false
	}
	f, err := os.Open(path)
	if err != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return paint.ImageOp{}, image.Point{}, false
	}
	op := paint.NewImageOp(img)
	op.Filter = paint.FilterLinear
	return op, img.Bounds().Size(), true
}
