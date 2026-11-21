package ui

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestScanIntoReportsTotal verifies a scan publishes the number of files it
// will visit as its process-bar total, so indexing progress shows done /
// total rather than a bare running count. The scan row is paused the moment
// it appears, so the test can read it before End removes it.
func TestScanIntoReportsTotal(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "lib")
	for _, name := range []string{"a.jpg", "b.png", "sub/c.jpg", "notes.txt"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newTestController(t, root, filepath.Join(dir, "cache"))
	var reg *ProcessRegistry
	// Pause re-enters this callback (forceNotify), so claim with a CAS
	// rather than a sync.Once, which would deadlock on itself.
	var claimed atomic.Bool
	var scanProc atomic.Pointer[Process]
	reg = NewProcessRegistry(func() {
		for _, s := range reg.Snapshot() {
			if s.Kind == ProcScan && claimed.CompareAndSwap(false, true) {
				p := reg.Get(s.ID)
				scanProc.Store(p)
				p.Pause()
			}
		}
	})
	c.SetProcessRegistry(reg)

	done := make(chan struct{})
	go func() { c.scanInto(context.Background(), root); close(done) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		var total int64 = -1
		for _, s := range reg.Snapshot() {
			if s.Kind == ProcScan {
				total = s.Total
			}
		}
		if total == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("scan total = %d, want 3", total)
		}
		time.Sleep(2 * time.Millisecond)
	}
	scanProc.Load().Resume()
	<-done
}
