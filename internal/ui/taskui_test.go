package ui

import (
	"strconv"
	"sync"
	"testing"
)

// TestTaskLogAppendCap locks the pending-buffer bound: appending past the cap
// keeps the newest taskLogPendingCap lines and drops the oldest, so a pass whose
// modal is never laid out (drain never runs) can't grow memory without bound.
func TestTaskLogAppendCap(t *testing.T) {
	var l taskLog
	const n = taskLogPendingCap + 200
	for i := range n {
		l.append("line " + strconv.Itoa(i))
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) != taskLogPendingCap {
		t.Fatalf("pending grew to %d, want capped at %d", len(l.pending), taskLogPendingCap)
	}
	if first, want := l.pending[0], "line "+strconv.Itoa(n-taskLogPendingCap); first != want {
		t.Errorf("oldest retained line = %q, want %q", first, want)
	}
	if last, want := l.pending[len(l.pending)-1], "line "+strconv.Itoa(n-1); last != want {
		t.Errorf("newest line = %q, want %q", last, want)
	}
}

// TestTaskLogDrainMovesPending checks drain moves pending into visible and
// clears pending, so a subsequent render reads the drained lines with no
// pending backlog left over.
func TestTaskLogDrainMovesPending(t *testing.T) {
	var l taskLog
	l.append("a")
	l.append("b")
	l.append("c")

	l.drain()

	got := l.lines()
	if len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Fatalf("visible after drain = %v, want [a b c]", got)
	}
	l.mu.Lock()
	pendingLen := len(l.pending)
	l.mu.Unlock()
	if pendingLen != 0 {
		t.Fatalf("pending after drain = %d, want 0 (drain must clear it)", pendingLen)
	}

	// A second drain with nothing pending is a no-op and leaves visible intact.
	l.drain()
	if got := l.lines(); len(got) != 3 {
		t.Fatalf("visible after empty drain = %d lines, want 3", len(got))
	}
}

// TestTaskLogDrainCapsVisible checks the drained, on-screen slice is bounded at
// taskLogVisibleCap (keeping the newest) even when many appends accumulate
// across several drains.
func TestTaskLogDrainCapsVisible(t *testing.T) {
	var l taskLog
	const n = taskLogVisibleCap + 350
	for i := range n {
		l.append("line " + strconv.Itoa(i))
		if i%7 == 0 { // interleave drains so it isn't one big flush
			l.drain()
		}
	}
	l.drain()

	got := l.lines()
	if len(got) != taskLogVisibleCap {
		t.Fatalf("visible = %d lines, want capped at %d", len(got), taskLogVisibleCap)
	}
	// The tail is retained: the very last appended line must be present.
	if last, want := got[len(got)-1], "line "+strconv.Itoa(n-1); last != want {
		t.Errorf("newest visible line = %q, want %q", last, want)
	}
}

// TestTaskLogReset clears both buffers for a fresh modal open.
func TestTaskLogReset(t *testing.T) {
	var l taskLog
	l.append("x")
	l.drain()
	l.append("y")
	l.reset()
	if got := l.lines(); got != nil {
		t.Errorf("visible after reset = %v, want nil", got)
	}
	l.mu.Lock()
	pending := l.pending
	l.mu.Unlock()
	if pending != nil {
		t.Errorf("pending after reset = %v, want nil", pending)
	}
}

// TestTaskLogConcurrentAppendDrain is the -race safety net: many workers append
// while the UI thread drains, exercising the exact worker→UI handoff the modal
// views rely on. No assertion beyond "the race detector stays quiet"; run with
// `go test -race`.
func TestTaskLogConcurrentAppendDrain(t *testing.T) {
	var l taskLog
	var wg sync.WaitGroup
	const appenders = 6
	const perAppender = 4000

	for a := range appenders {
		wg.Add(1)
		go func(a int) {
			defer wg.Done()
			for i := range perAppender {
				l.append("w" + strconv.Itoa(a) + "-" + strconv.Itoa(i))
			}
		}(a)
	}

	done := make(chan struct{})
	var drainWG sync.WaitGroup
	drainWG.Go(func() {
		for {
			select {
			case <-done:
				l.drain()
				_ = l.lines()
				return
			default:
				l.drain()
				_ = l.lines()
			}
		}
	})

	wg.Wait()
	close(done)
	drainWG.Wait()
}

// TestProgressModel locks the set/bump/reset/load math: set stores an absolute
// pair, bump advances done monotonically, and reset zeroes both.
func TestProgressModel(t *testing.T) {
	var p progressModel

	p.set(0, 10)
	if done, max := p.load(); done != 0 || max != 10 {
		t.Fatalf("after set(0,10) = (%d,%d), want (0,10)", done, max)
	}

	for range 10 {
		p.bump()
	}
	if done, max := p.load(); done != 10 || max != 10 {
		t.Fatalf("after 10 bumps = (%d,%d), want (10,10)", done, max)
	}

	// A fresh batch replaces both bounds, not accumulates.
	p.set(0, 3)
	if done, max := p.load(); done != 0 || max != 3 {
		t.Fatalf("after set(0,3) = (%d,%d), want (0,3)", done, max)
	}

	p.reset()
	if done, max := p.load(); done != 0 || max != 0 {
		t.Fatalf("after reset = (%d,%d), want (0,0)", done, max)
	}
}

// TestProgressModelConcurrentBump is the -race net for the per-file tick: many
// workers bump concurrently and every increment must land (no lost updates).
func TestProgressModelConcurrentBump(t *testing.T) {
	var p progressModel
	p.set(0, 0)

	var wg sync.WaitGroup
	const workers = 8
	const perWorker = 1000
	for range workers {
		wg.Go(func() {
			for range perWorker {
				p.bump()
			}
		})
	}
	wg.Wait()

	if done, _ := p.load(); done != workers*perWorker {
		t.Fatalf("done after concurrent bumps = %d, want %d (lost updates)", done, workers*perWorker)
	}
}

// TestProcScopeBeginNilRegistry: with no registry wired (tests, standalone use)
// begin returns nil so the caller's `if proc != nil` block is skipped.
func TestProcScopeBeginNilRegistry(t *testing.T) {
	var s procScope
	if proc := s.begin(ProcImport, "x", nil); proc != nil {
		t.Fatalf("begin with no registry = %v, want nil", proc)
	}
}

// TestProcScopeAttachDetach: begin registers a process, attachLocked publishes
// it to the slot, and detachLocked nils it — the lifecycle Import and Organize
// share (keep==nil, always store/clear).
func TestProcScopeAttachDetach(t *testing.T) {
	var mu sync.Mutex
	reg := NewProcessRegistry(func() {})
	s := &procScope{reg: reg}

	proc := s.begin(ProcImport, "test", nil)
	if proc == nil {
		t.Fatal("begin with a registry returned nil")
	}

	mu.Lock()
	s.attachLocked(proc, nil)
	got := s.currentLocked()
	mu.Unlock()
	if got != proc {
		t.Fatalf("currentLocked after attach = %v, want the begun proc", got)
	}

	mu.Lock()
	s.detachLocked(nil)
	got = s.currentLocked()
	mu.Unlock()
	if got != nil {
		t.Fatalf("currentLocked after detach = %v, want nil", got)
	}
	proc.End()
}

// TestProcScopeGenGuard mirrors the Duplicates Rescan supersession: a stale pass
// (keep()==false) must neither claim the slot from the live pass nor nil it out
// on the way down. Only the current pass (keep()==true) may write the slot.
func TestProcScopeGenGuard(t *testing.T) {
	var mu sync.Mutex
	reg := NewProcessRegistry(func() {})
	s := &procScope{reg: reg}

	live := s.begin(ProcDuplicates, "live", nil)
	stale := s.begin(ProcDuplicates, "stale", nil)

	// The live pass claims the slot.
	mu.Lock()
	s.attachLocked(live, func() bool { return true })
	mu.Unlock()

	// A superseded pass must not overwrite it.
	mu.Lock()
	s.attachLocked(stale, func() bool { return false })
	got := s.currentLocked()
	mu.Unlock()
	if got != live {
		t.Fatalf("stale attach overwrote the slot: got %v, want the live proc", got)
	}

	// A superseded pass tearing down must not clear the live slot.
	mu.Lock()
	s.detachLocked(func() bool { return false })
	got = s.currentLocked()
	mu.Unlock()
	if got != live {
		t.Fatal("stale detach cleared the live pass's slot")
	}

	// The live pass clears its own slot.
	mu.Lock()
	s.detachLocked(func() bool { return true })
	got = s.currentLocked()
	mu.Unlock()
	if got != nil {
		t.Fatal("live detach did not clear the slot")
	}

	live.End()
	stale.End()
}

// TestProcScopeConcurrent is the -race net for the lifecycle under load: many
// passes begin, attach, read, drive progress (notify), detach, and end against
// one scope+registry. The view mutex serializes the slot writes exactly as the
// real views do.
func TestProcScopeConcurrent(t *testing.T) {
	var mu sync.Mutex
	reg := NewProcessRegistry(func() {})
	s := &procScope{reg: reg}

	var wg sync.WaitGroup
	const goroutines = 40
	for range goroutines {
		wg.Go(func() {
			proc := s.begin(ProcImport, "t", nil)
			mu.Lock()
			s.attachLocked(proc, nil)
			mu.Unlock()
			mu.Lock()
			cur := s.currentLocked()
			mu.Unlock()
			if cur != nil {
				cur.AddDone(1) // routes through registry.notify()
			}
			mu.Lock()
			s.detachLocked(nil)
			mu.Unlock()
			proc.End()
		})
	}
	wg.Wait()

	if n := reg.Count(); n != 0 {
		t.Fatalf("registry left with %d live processes, want 0", n)
	}
}
