package workpool

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

func TestRunIndexedVisitsEveryIndexOnce(t *testing.T) {
	for _, workers := range []int{0, 1, 3, 64} {
		const n = 500
		hits := make([]int32, n)
		RunIndexed(context.Background(), n, workers, func(i int) { atomic.AddInt32(&hits[i], 1) })
		for i, h := range hits {
			if h != 1 {
				t.Fatalf("workers=%d: index %d ran %d times, want 1", workers, i, h)
			}
		}
	}
}

func TestRunIndexedStopsClaimingOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int64
	RunIndexed(ctx, 10000, 4, func(i int) {
		if ran.Add(1) == 10 {
			cancel()
		}
	})
	// At most one more index per worker can start after the cancel.
	if got := ran.Load(); got > 10+4 {
		t.Errorf("ran %d calls after cancelling at 10, want at most 14", got)
	}
}

func TestRunStreamProcessesEveryItem(t *testing.T) {
	var mu sync.Mutex
	seen := map[int]bool{}
	RunStream(context.Background(), 4, func(yield func(int) bool) {
		for i := range 300 {
			if !yield(i) {
				return
			}
		}
	}, func(i int) {
		mu.Lock()
		seen[i] = true
		mu.Unlock()
	})
	if len(seen) != 300 {
		t.Errorf("processed %d distinct items, want 300", len(seen))
	}
}

func TestRunStreamCancelStopsProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var produced, processed atomic.Int64
	RunStream(ctx, 2, func(yield func(int) bool) {
		for i := 0; ; i++ {
			if !yield(i) {
				return
			}
			produced.Add(1)
		}
	}, func(int) {
		if processed.Add(1) == 5 {
			cancel()
		}
	})
	if produced.Load() > 1000 {
		t.Errorf("producer kept going after cancel: %d items", produced.Load())
	}
}
