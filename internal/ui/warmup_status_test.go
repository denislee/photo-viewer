package ui

import (
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dns/photo-viewer/internal/scan"
)

// TestWarmUpKeepsScanStatus guards G-11: a warm-up pass that runs while a
// directory scan is in flight must not overwrite the scan's status (target,
// start/end time, reconciled count); it reports its own progress in
// IndexStatus.WarmUp instead.
func TestWarmUpKeepsScanStatus(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	c := newTestController(t, root, filepath.Join(dir, "cache"))

	var results []scan.Result
	for _, name := range []string{"a.jpg", "b.jpg", "c.jpg"} {
		results = append(results, scan.Result{Path: filepath.Join(root, name), Type: scan.TypePhoto, Size: 1, ModTime: time.Now()})
	}
	c.index.ReconcileBatch(results)

	// Simulate a scanInto that is part way through.
	scanStart := time.Now().Add(-time.Minute)
	c.mu.Lock()
	c.scanning = 1
	c.scanTarget = root
	c.scanStartedAt = scanStart
	c.scanBatched = 42
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.scanning = 0
		c.mu.Unlock()
	}()

	c.WarmUp()
	deadline := time.Now().Add(5 * time.Second)
	for c.IndexStatus().WarmUp.Active {
		if time.Now().After(deadline) {
			t.Fatal("warm-up did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}

	st := c.IndexStatus()
	if !st.Active || st.Target != root || !st.StartedAt.Equal(scanStart) || !st.EndedAt.IsZero() || st.Batched != 42 {
		t.Errorf("scan status changed by warm-up: active=%v target=%q started=%v ended=%v batched=%d",
			st.Active, st.Target, st.StartedAt, st.EndedAt, st.Batched)
	}
	w := st.WarmUp
	if w.StartedAt.IsZero() || w.EndedAt.IsZero() || w.Done != 3 || w.Total != 3 {
		t.Errorf("WarmUp status = %+v, want started+ended with 3/3 done", w)
	}
}

// TestWarmUpCancelledDoneIsExact guards U4-09: a cancelled warm-up pass must
// publish the exact number of entries it processed, not the last multiple of
// 50 it happened to report, so the info modal never shows a stale count while
// the pass sits idle forever.
func TestWarmUpCancelledDoneIsExact(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	c := newTestController(t, root, filepath.Join(dir, "cache"))

	// 500 index rows whose files do not exist on disk: thumbnail generation
	// fails fast, which keeps the pass quick while still driving the counter.
	const total = 500
	results := make([]scan.Result, 0, total)
	for i := 0; i < total; i++ {
		results = append(results, scan.Result{
			Path:    filepath.Join(root, fmt.Sprintf("p%04d.jpg", i)),
			Type:    scan.TypePhoto,
			Size:    1,
			ModTime: time.Now(),
		})
	}
	c.index.ReconcileBatch(results)

	var calls atomic.Int64
	c.SetInvalidate(func() {
		if calls.Add(1) == 8 {
			c.CancelScan()
		}
	})

	c.WarmUp()
	deadline := time.Now().Add(10 * time.Second)
	for c.IndexStatus().WarmUp.Active {
		if time.Now().After(deadline) {
			t.Fatal("warm-up did not finish")
		}
		time.Sleep(2 * time.Millisecond)
	}
	// The final defer publishes its invalidate just after clearing Active, so
	// wait for the count to stop moving before deriving the expectation.
	settled := int64(-1)
	for {
		n := calls.Load()
		if n == settled {
			break
		}
		settled = n
		if time.Now().After(deadline) {
			t.Fatal("invalidate count did not settle")
		}
		time.Sleep(2 * time.Millisecond)
	}

	w := c.IndexStatus().WarmUp
	// One call at WarmUp start, one per processed entry, one in the final defer.
	want := int(calls.Load()) - 2
	if w.Done != want {
		t.Errorf("WarmUp.Done = %d, want %d (processed entries)", w.Done, want)
	}
	if w.Done <= 0 {
		t.Errorf("WarmUp.Done = %d, want > 0", w.Done)
	}
	if w.Done >= w.Total {
		t.Errorf("WarmUp.Done = %d of %d, want a cancelled pass to stop short", w.Done, w.Total)
	}
	if w.Total != total {
		t.Errorf("WarmUp.Total = %d, want %d", w.Total, total)
	}
}
