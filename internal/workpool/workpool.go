// Package workpool runs small, cancel-aware worker pools. It replaces the
// "N workers over [0,n), results by index" pool that import, organize and
// pv-organize each hand-wrote (X6-02).
package workpool

import (
	"context"
	"sync"
	"sync/atomic"
)

// RunIndexed calls fn(i) for every i in [0, n) on up to workers goroutines
// and returns once all of them have returned. Indexes are claimed from an
// atomic counter, so each runs at most once and fn may write its result into
// slot i of a pre-sized slice without locking. Once ctx is cancelled no
// further index is claimed (calls already running finish). Pause gates and
// any finer-grained cancel checks belong inside fn. workers < 1 is treated
// as 1.
func RunIndexed(ctx context.Context, n, workers int, fn func(i int)) {
	if n <= 0 {
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(max(workers, 1), n) {
		wg.Go(func() {
			for {
				i := int(next.Add(1) - 1)
				if i >= n || ctx.Err() != nil {
					return
				}
				fn(i)
			}
		})
	}
	wg.Wait()
}

// RunStream is RunIndexed for a stream of unknown length: produce pushes
// items through yield (which reports false once ctx is cancelled, telling the
// producer to stop), and up to workers goroutines call fn on them. The hand-
// off channel is lightly buffered so a slow pool back-pressures the producer
// instead of buffering the whole stream. RunStream returns once the producer
// and every worker have returned.
func RunStream[T any](ctx context.Context, workers int, produce func(yield func(T) bool), fn func(T)) {
	workers = max(workers, 1)
	jobs := make(chan T, workers*2)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for item := range jobs {
				if ctx.Err() != nil {
					continue // drain so the producer is never stuck on a send
				}
				fn(item)
			}
		})
	}
	produce(func(item T) bool {
		select {
		case jobs <- item:
			return true
		case <-ctx.Done():
			return false
		}
	})
	close(jobs)
	wg.Wait()
}
