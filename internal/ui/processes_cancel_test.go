package ui

import (
	"context"
	"testing"
	"time"
)

// TestBeginCtxCancelReleasesPausedWorker is the U5-03 regression: a process
// paused via the bar must release its pause gate when the worker's context is
// cancelled by any route (not just Process.Cancel), so a worker blocked in
// Wait() observes ctx.Done() and exits instead of leaking.
func TestBeginCtxCancelReleasesPausedWorker(t *testing.T) {
	reg := NewProcessRegistry(nil)
	ctx, cancel := context.WithCancel(context.Background())
	p := reg.Begin(ctx, ProcScan, "t", nil, true)
	p.Pause()

	done := make(chan struct{})
	go func() {
		p.Wait()
		if ctx.Err() == nil {
			t.Error("Wait returned with ctx not cancelled")
		}
		close(done)
	}()

	// Let the goroutine reach Wait before cancelling.
	time.Sleep(50 * time.Millisecond)
	if !p.IsPaused() {
		t.Fatal("process should be paused")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("paused worker did not observe ctx cancellation")
	}

	p.End()
	if got := reg.Count(); got != 0 {
		t.Fatalf("process count after End = %d, want 0", got)
	}
}

// TestBeginNilCtxStillPausable: a nil ctx must not arm any cancel hook, so the
// process stays pausable and Wait blocks until an explicit Resume.
func TestBeginNilCtxStillPausable(t *testing.T) {
	reg := NewProcessRegistry(nil)
	p := reg.Begin(context.Background(), ProcScan, "t", nil, true)
	p.Pause()

	done := make(chan struct{})
	go func() {
		p.Wait()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("Wait returned while paused")
	case <-time.After(50 * time.Millisecond):
	}

	p.Resume()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after Resume")
	}

	p.End()
}

// TestEndStopsAfterFunc: End must stop the context hook, so cancelling the ctx
// after the process is gone neither panics nor touches the discarded gate.
func TestEndStopsAfterFunc(t *testing.T) {
	reg := NewProcessRegistry(nil)
	ctx, cancel := context.WithCancel(context.Background())
	p := reg.Begin(ctx, ProcScan, "t", nil, true)
	p.End()
	cancel()

	if got := reg.Count(); got != 0 {
		t.Fatalf("process count after End = %d, want 0", got)
	}
}

// TestPauseAfterCancelIsInert: once ctx is cancelled the AfterFunc has fired,
// so a late Pause must not close the gate again and strand the worker.
func TestPauseAfterCancelIsInert(t *testing.T) {
	reg := NewProcessRegistry(func() {})
	ctx, cancel := context.WithCancel(context.Background())
	p := reg.Begin(ctx, ProcScan, "t", nil, true)
	defer p.End()
	cancel()
	p.Pause()
	if p.IsPaused() {
		t.Fatal("Pause after cancel closed the gate")
	}
}
