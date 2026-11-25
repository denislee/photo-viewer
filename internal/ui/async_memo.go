package ui

import "sync"

// asyncMemo caches a slow per-path lookup (an image header decode, an
// exiftool read) that must not run on the UI goroutine. get returns the
// cached value, or the caller's pending placeholder while the lookup runs.
//
// One worker goroutine per memo serves every queued path (I-15): paging
// through N items queues N lookups instead of starting N goroutines, and the
// worker takes the most recently requested path first, so the item on screen
// is answered before the ones the user has already paged past. The map holds
// a not-ready slot for a queued path, which is how a second get knows not to
// queue it again (G6-04). It is bulk-cleared of ready values once it reaches
// viewerMetaCap; recomputing is cheap, so that beats a full LRU.
//
// The zero value is ready to use.
type asyncMemo[T any] struct {
	mu      sync.Mutex
	m       map[string]memoSlot[T]
	queue   []string // paths waiting for the worker, newest last
	running bool
}

type memoSlot[T any] struct {
	v     T
	ready bool
}

// get returns path's value when it has been computed, else pending, queueing
// path for the worker the first time it is asked for. compute must be the same
// function on every call; the worker calls invalidate after storing each value
// so the next frame repaints with it.
func (a *asyncMemo[T]) get(path string, pending T, compute func(string) T, invalidate func()) T {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.m[path]; ok {
		if s.ready {
			return s.v
		}
		return pending
	}
	if a.m == nil {
		a.m = make(map[string]memoSlot[T])
	}
	a.m[path] = memoSlot[T]{}
	a.queue = append(a.queue, path)
	if !a.running {
		a.running = true
		go a.work(compute, invalidate)
	}
	return pending
}

// work drains the queue newest-first, then exits; the next get restarts it.
func (a *asyncMemo[T]) work(compute func(string) T, invalidate func()) {
	for {
		a.mu.Lock()
		if len(a.queue) == 0 {
			a.running = false
			a.mu.Unlock()
			return
		}
		path := a.queue[len(a.queue)-1]
		a.queue = a.queue[:len(a.queue)-1]
		a.mu.Unlock()

		v := compute(path)

		a.mu.Lock()
		if len(a.m) >= viewerMetaCap {
			// Drop the ready values; keep the slots of still-queued paths so
			// they are not queued a second time.
			for p, s := range a.m {
				if s.ready {
					delete(a.m, p)
				}
			}
		}
		a.m[path] = memoSlot[T]{v: v, ready: true}
		a.mu.Unlock()
		if invalidate != nil {
			invalidate()
		}
	}
}
