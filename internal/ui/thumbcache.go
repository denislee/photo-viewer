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

// thumbCacheCapacity caps the number of decoded thumbnails kept in memory.
// Each entry holds an RGBA pixel buffer (~256 KB at the default thumb size),
// so 1024 entries ≈ 256 MB peak. Far larger than any visible window, so live
// cells are never evicted; old entries are dropped once a long browse session
// has touched many directories.
const thumbCacheCapacity = 1024

// thumbCache lazily resolves thumbnail JPEGs into paint.ImageOp values and
// caches them with an LRU policy. Multiple frames may request the same thumb
// concurrently; only one decode is queued per entry. When a decode completes,
// invalidate is fired so the next frame can paint the result.
type thumbCache struct {
	store      *cache.ThumbStore
	invalidate func()
	capacity   int

	mu      sync.Mutex
	entries map[string]*list.Element // value: *thumbEntry, list ordered MRU→LRU
	lru     *list.List
	queue   chan cache.Entry

	// Coalescing: workers bump dirty; a single coalescer goroutine fires
	// invalidate at most once every ~16ms so a burst of decodes doesn't
	// hammer the Gio frame loop with one redraw per thumbnail.
	dirty atomic.Bool
}

type thumbEntry struct {
	path   string
	ready  bool
	op     paint.ImageOp
	size   image.Point
	queued bool
}

func newThumbCache(store *cache.ThumbStore, invalidate func()) *thumbCache {
	tc := &thumbCache{
		store:      store,
		invalidate: invalidate,
		capacity:   thumbCacheCapacity,
		entries:    make(map[string]*list.Element),
		lru:        list.New(),
		queue:      make(chan cache.Entry, 256),
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
	tc.mu.Lock()
	te := tc.touchOrCreate(e.Path)
	if te.ready {
		op, sz := te.op, te.size
		tc.mu.Unlock()
		return op, sz, true
	}
	if !te.queued {
		te.queued = true
		tc.mu.Unlock()
		select {
		case tc.queue <- e:
		default:
			// queue full; drop and let a later frame retry by clearing queued.
			tc.mu.Lock()
			te.queued = false
			tc.mu.Unlock()
		}
		return paint.ImageOp{}, image.Point{}, false
	}
	tc.mu.Unlock()
	return paint.ImageOp{}, image.Point{}, false
}

// touchOrCreate returns the entry for path, promoting it to the MRU end of
// the LRU list (creating it if missing). Caller must hold tc.mu. Eviction of
// the oldest entries happens here when the cache exceeds its capacity, but
// only ready entries with no in-flight decode are eligible — a queued entry
// is needed by the next frame.
func (tc *thumbCache) touchOrCreate(path string) *thumbEntry {
	if el, ok := tc.entries[path]; ok {
		tc.lru.MoveToFront(el)
		return el.Value.(*thumbEntry)
	}
	te := &thumbEntry{path: path}
	el := tc.lru.PushFront(te)
	tc.entries[path] = el
	tc.evictIfNeeded()
	return te
}

// evictIfNeeded drops the least-recently-used ready entries until the cache
// is within capacity. Caller must hold tc.mu.
func (tc *thumbCache) evictIfNeeded() {
	for tc.lru.Len() > tc.capacity {
		oldest := tc.lru.Back()
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
					tc.lru.Remove(prev)
					delete(tc.entries, cand.path)
					break
				}
				prev = prev.Prev()
			}
			if prev == nil {
				return // nothing evictable
			}
			continue
		}
		tc.lru.Remove(oldest)
		delete(tc.entries, te.path)
	}
}

func (tc *thumbCache) worker() {
	for e := range tc.queue {
		op, sz, ok := tc.decode(e)
		tc.mu.Lock()
		el := tc.entries[e.Path]
		var te *thumbEntry
		if el == nil {
			// Evicted while in-flight; reinsert at MRU.
			te = &thumbEntry{path: e.Path}
			el = tc.lru.PushFront(te)
			tc.entries[e.Path] = el
			tc.evictIfNeeded()
		} else {
			te = el.Value.(*thumbEntry)
		}
		te.queued = false
		if ok {
			te.op = op
			te.size = sz
			te.ready = true
		}
		tc.mu.Unlock()
		if ok {
			tc.dirty.Store(true)
		}
	}
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
