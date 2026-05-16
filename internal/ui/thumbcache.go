package ui

import (
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

// thumbCache lazily resolves thumbnail JPEGs into paint.ImageOp values and
// caches them. Multiple frames may request the same thumb concurrently;
// only one decode is queued per entry. When a decode completes, invalidate
// is fired so the next frame can paint the result.
type thumbCache struct {
	store      *cache.ThumbStore
	invalidate func()

	mu      sync.Mutex
	entries map[string]*thumbEntry // key: cache.Entry.Path
	queue   chan cache.Entry

	// Coalescing: workers bump dirty; a single coalescer goroutine fires
	// invalidate at most once every ~16ms so a burst of decodes doesn't
	// hammer the Gio frame loop with one redraw per thumbnail.
	dirty atomic.Bool
}

type thumbEntry struct {
	ready  bool
	op     paint.ImageOp
	size   image.Point
	queued bool
}

func newThumbCache(store *cache.ThumbStore, invalidate func()) *thumbCache {
	tc := &thumbCache{
		store:      store,
		invalidate: invalidate,
		entries:    make(map[string]*thumbEntry),
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
	te, ok := tc.entries[e.Path]
	if !ok {
		te = &thumbEntry{}
		tc.entries[e.Path] = te
	}
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

func (tc *thumbCache) worker() {
	for e := range tc.queue {
		op, sz, ok := tc.decode(e)
		tc.mu.Lock()
		te := tc.entries[e.Path]
		if te == nil {
			te = &thumbEntry{}
			tc.entries[e.Path] = te
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
