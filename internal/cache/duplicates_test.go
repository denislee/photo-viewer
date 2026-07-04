package cache

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// seedDupFiles writes n pairs of byte-identical files into dir and returns the
// scan.Results describing them. Identical content within a pair guarantees a
// shared size AND a shared quick hash, so EnsureHashes exercises both worker
// pools: the quick-hash (I/O) pass and the full-hash (CPU) pass. Each file is
// larger than 2*quickHashWindow so quickHash also takes its head/tail-window
// branch rather than the whole-file copy.
func seedDupFiles(t *testing.T, dir string, n int) []scan.Result {
	t.Helper()
	out := make([]scan.Result, 0, n*2)
	for i := 0; i < n; i++ {
		// Distinct bytes per pair → distinct quick hash across pairs; identical
		// within a pair → they collide and reach the full-hash phase.
		content := bytes.Repeat([]byte{byte(i), 0xab, 0xcd}, quickHashWindow) // ~192 KiB
		for _, suffix := range []string{"a", "b"} {
			p := filepath.Join(dir, fmt.Sprintf("dup-%03d-%s.bin", i, suffix))
			if err := os.WriteFile(p, content, 0o644); err != nil {
				t.Fatal(err)
			}
			out = append(out, scan.Result{
				Path:    p,
				Type:    scan.TypePhoto,
				Size:    int64(len(content)),
				ModTime: time.Date(2024, 1, 1, 0, 0, i, 0, time.UTC),
			})
		}
	}
	return out
}

// settleGoroutines returns the minimum live-goroutine count observed over a
// short window, giving finishing goroutines time to unwind before we snapshot
// a baseline.
func settleGoroutines() int {
	min := 1 << 30
	for i := 0; i < 10; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		if n := runtime.NumGoroutine(); n < min {
			min = n
		}
	}
	return min
}

// waitGoroutinesLE polls (goroutine teardown is asynchronous) until the live
// count drops to <= target, returning the final count and whether it settled
// within the timeout.
func waitGoroutinesLE(target int) (int, bool) {
	for i := 0; i < 200; i++ {
		runtime.GC()
		if n := runtime.NumGoroutine(); n <= target {
			return n, true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine(), false
}

// TestEnsureHashesNoGoroutineLeak asserts that a hashing pass leaves no
// worker / feeder / closer goroutines behind, whether it runs to completion or
// is cancelled. This guards the I-05 fix: EnsureHashes derives a cancellable
// context and its hash workers select on ctx.Done() when sending, so no return
// path can wedge a worker on a send with no reader and leak the pool.
//
// Note: the *specific* leak the fix closes only manifests on a mid-batch flush
// error (an early return that abandons the results channel while workers are
// still producing). That path can't be triggered without an invasive DB hook —
// on a healthy index every flush succeeds — so this test instead pins the
// broader invariant (no goroutine outlives a pass) across the completed and
// cancelled paths, and the concurrent-cancel case drives real load through the
// ctx.Done() send arm the fix added.
func TestEnsureHashesNoGoroutineLeak(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	dir, err := os.MkdirTemp("", "pv-dedup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	results := seedDupFiles(t, dir, 24)
	reseed := func() {
		if err := idx.Clear(); err != nil {
			t.Fatalf("Clear: %v", err)
		}
		idx.ReconcileBatch(results)
	}

	// Warm-up pass so the SQLite driver / runtime spin up whatever long-lived
	// goroutines they lazily create; only then is the baseline stable.
	reseed()
	if err := idx.EnsureHashes(context.Background(), nil, nil); err != nil {
		t.Fatalf("warm-up EnsureHashes: %v", err)
	}
	base := settleGoroutines()

	// A pass that runs to completion must not leak.
	reseed()
	if err := idx.EnsureHashes(context.Background(), nil, nil); err != nil {
		t.Fatalf("completed EnsureHashes: %v", err)
	}
	if n, ok := waitGoroutinesLE(base + 2); !ok {
		t.Errorf("after completed pass: %d goroutines live, baseline %d", n, base)
	}

	// A pass cancelled before it begins feeding must not leak.
	reseed()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = idx.EnsureHashes(ctx, nil, nil)
	if n, ok := waitGoroutinesLE(base + 2); !ok {
		t.Errorf("after pre-cancelled pass: %d goroutines live, baseline %d", n, base)
	}

	// A pass cancelled mid-flight drives real work through the ctx.Done() send
	// arm; still no goroutine may outlive it.
	reseed()
	ctx2, cancel2 := context.WithCancel(context.Background())
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel2()
	}()
	_ = idx.EnsureHashes(ctx2, nil, nil)
	cancel2()
	if n, ok := waitGoroutinesLE(base + 2); !ok {
		t.Errorf("after mid-flight cancel: %d goroutines live, baseline %d", n, base)
	}
}

// TestEnsureHashesFindsDuplicates is a straightforward functional check that a
// completed pass hashes the seeded pairs and FindDuplicates groups them — it
// also gives the leak test's "completed pass" some correctness backing.
func TestEnsureHashesFindsDuplicates(t *testing.T) {
	idx, cleanup := loadEmpty(t)
	defer cleanup()

	dir, err := os.MkdirTemp("", "pv-dedup-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	const pairs = 5
	idx.ReconcileBatch(seedDupFiles(t, dir, pairs))
	if err := idx.EnsureHashes(context.Background(), nil, nil); err != nil {
		t.Fatalf("EnsureHashes: %v", err)
	}

	groups := idx.FindDuplicates()
	if len(groups) != pairs {
		t.Fatalf("FindDuplicates returned %d groups, want %d", len(groups), pairs)
	}
	for _, g := range groups {
		if len(g.Entries) != 2 {
			t.Errorf("group %s has %d entries, want 2", g.Hash, len(g.Entries))
		}
	}
}
