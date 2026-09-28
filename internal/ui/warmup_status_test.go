package ui

import (
	"path/filepath"
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
